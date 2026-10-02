#!/usr/bin/env python3
# display-agent 的播放进程：用 GStreamer 在 DRM/KMS 上直接出画面，视频走 H3 的硬件解码器（cedrus）。
#
# 由 display-agent 拉起并守护（程序内嵌，每次启动重写，随 OTA 更新），stdin/stdout 上逐行收发 JSON：
#   请求  {"id": 1, "cmd": "config", "width": 1440, "height": 900, "fade": 0.6}
#         {"id": 2, "cmd": "load", "items": [{"path": "...", "type": "image|video", "duration": 10}],
#          "overlay": "/path/x.bgra" | null, "canvas": [1440, 900], "media": [x, y, w, h]}
#         {"id": 3, "cmd": "stats"}   {"id": 4, "cmd": "ping"}
#   回复  {"id": 1, "ok": true, ...} 或 {"id": 1, "error": "..."}
#   事件  {"event": "playing", "index": 0, "path": "..."}  {"event": "error", "index": 0, "message": "..."}
# 日志写 stderr（英文），由 display-agent 转进 journal。
#
# 画面由显示控制器（Allwinner DE2）的两个硬件图层叠出来，不经 GPU、不做软件合成：
#   - 下层（VI 图层，能缩放、吃 NV12）：播放内容。每一项一条 playbin 管线，解码器按 rank 自动选，
#     有 cedrus 时就是 v4l2slh264dec（硬解，NV12 dmabuf 直接交给 kmssink 显示，零拷贝）；
#     画面按 cover 撑满媒体区：videocrop 只打裁剪标记、kmssink 按标记取源矩形，不拷像素。
#   - 上层（主图层，ARGB）：服务端渲染的模板叠加图，媒体区是全透明的"洞"，下层的内容从洞里透出来。
#     这条管线常驻（appsrc → kmssink），并由它按叠加图尺寸设置显示模式（force-modesetting）。
# 两个 kmssink 共用本进程打开的同一个 DRM fd（只有一个 DRM master）。
#
# 切换过渡（约 0.6 秒淡出到黑 → 淡入）在上层做：把洞里填上透明度渐变的黑色、逐帧推给上层图层。
# 下层的硬解视频帧碰不得（dmabuf，CPU 改它太慢），而上层一帧只是一次内存拷贝。
# 切换的空档里洞是全黑的，所以拆旧管线、建新管线的那一下看不出来；模板属性区始终不动。
#
# 测试/无显示环境：环境变量 DISPLAY_PLAYER_SINK=fakesink 时两层都换成 fakesink，不碰 DRM。

import ctypes
import ctypes.util
import json
import os
import sys

import gi

gi.require_version("Gst", "1.0")
gi.require_version("GstVideo", "1.0")
from gi.repository import GLib, Gst, GstVideo  # noqa: E402

FADE_STEP_MS = 33
FOURCC_NV12 = 0x3231564E


def log(msg):
    print("player(gst): " + msg, file=sys.stderr, flush=True)


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


# ---- DRM：找显示设备与视频图层（libdrm，经 ctypes） ----

class _PlaneRes(ctypes.Structure):
    _fields_ = [("count_planes", ctypes.c_uint32), ("planes", ctypes.POINTER(ctypes.c_uint32))]


class _Plane(ctypes.Structure):
    _fields_ = [("count_formats", ctypes.c_uint32), ("formats", ctypes.POINTER(ctypes.c_uint32)),
                ("plane_id", ctypes.c_uint32), ("crtc_id", ctypes.c_uint32), ("fb_id", ctypes.c_uint32),
                ("crtc_x", ctypes.c_uint32), ("crtc_y", ctypes.c_uint32), ("x", ctypes.c_uint32),
                ("y", ctypes.c_uint32), ("possible_crtcs", ctypes.c_uint32), ("gamma_size", ctypes.c_uint32)]


class _Res(ctypes.Structure):
    _fields_ = [("count_fbs", ctypes.c_int), ("fbs", ctypes.POINTER(ctypes.c_uint32)),
                ("count_crtcs", ctypes.c_int), ("crtcs", ctypes.POINTER(ctypes.c_uint32)),
                ("count_connectors", ctypes.c_int), ("connectors", ctypes.POINTER(ctypes.c_uint32)),
                ("count_encoders", ctypes.c_int), ("encoders", ctypes.POINTER(ctypes.c_uint32)),
                ("min_width", ctypes.c_uint32), ("max_width", ctypes.c_uint32),
                ("min_height", ctypes.c_uint32), ("max_height", ctypes.c_uint32)]


def _libdrm():
    name = ctypes.util.find_library("drm") or "libdrm.so.2"
    lib = ctypes.CDLL(name)
    lib.drmModeGetResources.restype = ctypes.POINTER(_Res)
    lib.drmModeGetPlaneResources.restype = ctypes.POINTER(_PlaneRes)
    lib.drmModeGetPlane.restype = ctypes.POINTER(_Plane)
    return lib


def open_display():
    """打开有显示接口的 DRM 设备（H3 上 GPU 也是一个 card，要跳过）。返回 (fd, 视频图层 id 或 -1)。"""
    drm = _libdrm()
    for i in range(8):
        path = "/dev/dri/card%d" % i
        if not os.path.exists(path):
            continue
        fd = os.open(path, os.O_RDWR | os.O_CLOEXEC)
        res = drm.drmModeGetResources(fd)
        if res and res.contents.count_connectors > 0:
            drm.drmModeFreeResources(res)
            return fd, _video_plane(drm, fd), path
        if res:
            drm.drmModeFreeResources(res)
        os.close(fd)
    raise RuntimeError("no DRM device with display connectors under /dev/dri")


def _video_plane(drm, fd):
    """第一个能显示 NV12、挂在第一个 CRTC 上的覆盖图层（DE2 的 VI 图层）。

    此时还没开 universal planes，列表里只有覆盖图层，不会选到上层用的主图层。"""
    pres = drm.drmModeGetPlaneResources(fd)
    if not pres:
        return -1
    try:
        for i in range(pres.contents.count_planes):
            p = drm.drmModeGetPlane(fd, pres.contents.planes[i])
            if not p:
                continue
            try:
                pl = p.contents
                fmts = {pl.formats[j] for j in range(pl.count_formats)}
                if FOURCC_NV12 in fmts and pl.possible_crtcs & 1:
                    return pl.plane_id
            finally:
                drm.drmModeFreePlane(p)
    finally:
        drm.drmModeFreePlaneResources(pres)
    return -1


def cover_crop(src_w, src_h, dst_w, dst_h):
    """按 cover 撑满目标区域时，源画面四边各裁掉多少像素（左, 右, 上, 下）。"""
    if src_w <= 0 or src_h <= 0 or dst_w <= 0 or dst_h <= 0:
        return 0, 0, 0, 0
    if src_w * dst_h > dst_w * src_h:  # 源更宽：裁左右
        keep = src_h * dst_w // dst_h
        cut = src_w - keep
        return cut // 2, cut - cut // 2, 0, 0
    keep = src_w * dst_h // dst_w  # 源更高：裁上下
    cut = src_h - keep
    return 0, 0, cut // 2, cut - cut // 2


class Player:
    def __init__(self, loop):
        self.loop = loop
        self.test = os.environ.get("DISPLAY_PLAYER_SINK") == "fakesink"
        self.fd, self.plane = -1, -1
        self.width = self.height = 0
        self.fade = 0.6
        self.overlay_pipe = self.appsrc = None
        self.base = b""          # 上层叠加图（BGRA，预乘 alpha）；没有模板时全透明
        self.hole = (0, 0, 0, 0)  # 媒体区在显示坐标里的位置
        self.scene = None
        self.items = []
        self.index = -1
        self.pipe = None          # 当前播放项的管线
        self.started = False      # 当前项是否已开始显示
        self.timer = 0            # 图片到点淡出 / 视频剩余时间巡检
        self.fading = 0           # 进行中的淡入淡出（GLib 定时器 id）
        self.alpha = 255          # 洞里黑色的不透明度：255 全黑，0 透出播放内容
        self.decoder = ""         # 最近一次视频用的解码器

    # ---- 请求 ----

    def handle(self, req):
        cmd = req.get("cmd")
        if cmd == "config":
            return self.configure(req)
        if cmd == "load":
            return self.load(req)
        if cmd == "stats":
            hw = ""
            if self.decoder:
                hw = self.decoder if self.decoder.startswith("v4l2sl") else "no"
            return {"hwdec": hw, "output_w": self.width, "output_h": self.height}
        if cmd == "ping":
            return {}
        raise ValueError("unknown command %r" % cmd)

    def configure(self, req):
        if self.overlay_pipe:
            return {}  # 显示模式只在启动时定一次
        self.width, self.height = int(req["width"]), int(req["height"])
        self.fade = float(req.get("fade", 0.6))
        if not self.test:
            self.fd, self.plane, path = open_display()
            log("display %s, video plane %d, output %dx%d" % (path, self.plane, self.width, self.height))
            if not Gst.ElementFactory.find("v4l2slh264dec"):
                log("WARNING: hardware decoder v4l2slh264dec is not available "
                    "(cedrus missing or gstreamer1.0-plugins-bad not installed); videos will be decoded in software")
        self.base = bytes(self.width * self.height * 4)
        self.hole = (0, 0, self.width, self.height)
        self._start_overlay()
        return {}

    def _start_overlay(self):
        caps = "video/x-raw,format=BGRA,width=%d,height=%d,framerate=0/1" % (self.width, self.height)
        if self.test:
            sink = "fakesink sync=false"
        else:
            sink = ("kmssink fd=%d force-modesetting=true restore-crtc=false sync=false "
                    "plane-properties=\"props,zpos=(int)1\"" % self.fd)
        self.overlay_pipe = Gst.parse_launch(
            "appsrc name=src format=time is-live=false caps=\"%s\" ! %s" % (caps, sink))
        self.appsrc = self.overlay_pipe.get_by_name("src")
        self._watch(self.overlay_pipe, self._on_overlay_message)
        self._push_overlay()
        self.overlay_pipe.set_state(Gst.State.PLAYING)

    def load(self, req):
        if not self.overlay_pipe:
            raise RuntimeError("not configured")
        scene = {k: v for k, v in req.items() if k not in ("id", "cmd")}
        if scene == self.scene:
            return {}  # 内容没变（代理重发）：不打断播放
        cw, ch = req.get("canvas") or (self.width, self.height)
        overlay = req.get("overlay")
        if overlay:
            with open(overlay, "rb") as f:
                base = f.read()
            if len(base) != self.width * self.height * 4:
                raise ValueError("overlay %s is not %dx%d BGRA" % (overlay, self.width, self.height))
            mx, my, mw, mh = req["media"]
            sx, sy = self.width / cw, self.height / ch
            hole = (round(mx * sx), round(my * sy), round(mw * sx), round(mh * sy))
        else:
            base, hole = bytes(self.width * self.height * 4), (0, 0, self.width, self.height)
        items = [it for it in req.get("items", []) if it.get("path")]

        def switch():
            self.scene, self.base, self.hole, self.items = scene, base, hole, items
            self._stop_item()
            self.alpha = 255
            self._push_overlay()
            if items:
                self._play(0)
        if self.pipe and self.alpha < 255:
            self._fade_to(255, switch)  # 先把当前内容淡出，再换
        else:
            switch()
        return {}

    # ---- 上层：叠加图与淡入淡出 ----

    def _frame(self, alpha):
        if alpha <= 0:
            return self.base
        buf = bytearray(self.base)
        x, y, w, h = self.hole
        stride, row = self.width * 4, bytes((0, 0, 0, alpha)) * w  # BGRA 预乘：半透明黑就是 (0,0,0,a)
        for r in range(y, y + h):
            o = r * stride + x * 4
            buf[o:o + w * 4] = row
        return bytes(buf)

    def _push_overlay(self):
        self.appsrc.emit("push-buffer", Gst.Buffer.new_wrapped(self._frame(self.alpha)))

    def _fade_to(self, target, done=None):
        steps = max(1, int(self.fade * 1000 / FADE_STEP_MS))
        start = self.alpha
        if self.fading:
            GLib.source_remove(self.fading)
            self.fading = 0
        if start == target or self.fade <= 0:
            self.alpha = target
            self._push_overlay()
            if done:
                done()
            return
        state = {"n": 0}

        def step():
            state["n"] += 1
            p = min(1.0, state["n"] / steps)
            p = p * p * (3 - 2 * p)  # smoothstep，起止更柔和
            self.alpha = round(start + (target - start) * p)
            self._push_overlay()
            if state["n"] >= steps:
                self.fading = 0
                if done:
                    done()
                return False
            return True
        self.fading = GLib.timeout_add(FADE_STEP_MS, step)

    # ---- 下层：播放项 ----

    def _play(self, index):
        self.index = index % len(self.items)
        item = self.items[self.index]
        x, y, w, h = self.hole
        if self.test:
            sink = "fakesink sync=true"
        else:
            sink = ("kmssink name=sink fd=%d plane-id=%d can-scale=true "
                    "plane-properties=\"props,zpos=(int)0\"" % (self.fd, self.plane))
        bin_ = Gst.parse_bin_from_description("videoconvert ! videocrop name=crop ! " + sink, True)
        if not self.test:
            GstVideo.VideoOverlay.set_render_rectangle(bin_.get_by_name("sink"), x, y, w, h)
        crop = bin_.get_by_name("crop")
        crop.get_static_pad("sink").add_probe(Gst.PadProbeType.EVENT_DOWNSTREAM, self._on_caps, (crop, w, h))

        pipe = Gst.ElementFactory.make("playbin", None)
        pipe.set_property("uri", Gst.filename_to_uri(item["path"]))
        pipe.set_property("video-sink", bin_)
        pipe.set_property("flags", 0x1)  # 只要视频：屏幕一律静音，也不去开声卡
        pipe.connect("deep-element-added", self._on_element)
        self._watch(pipe, lambda _bus, msg: self._on_item_message(pipe, msg))
        self.pipe, self.started = pipe, False
        if pipe.set_state(Gst.State.PLAYING) == Gst.StateChangeReturn.FAILURE:
            self._item_failed("cannot start pipeline")

    def _on_caps(self, pad, info, data):
        ev = info.get_event()
        if ev.type == Gst.EventType.CAPS:
            crop, w, h = data
            s = ev.parse_caps().get_structure(0)
            ok_w, sw = s.get_int("width")
            ok_h, sh = s.get_int("height")
            if ok_w and ok_h:
                left, right, top, bottom = cover_crop(sw, sh, w, h)
                crop.set_property("left", left)
                crop.set_property("right", right)
                crop.set_property("top", top)
                crop.set_property("bottom", bottom)
        return Gst.PadProbeReturn.OK

    def _on_element(self, _bin, _sub, element):
        f = element.get_factory()
        if f and "Decoder/Video" in (f.get_metadata("klass") or ""):
            name = f.get_name()
            if name != self.decoder:
                log("video decoder: %s" % name)
            self.decoder = name

    def _on_item_message(self, pipe, msg):
        if pipe is not self.pipe:
            return True  # 已经换掉的管线残留的消息
        t = msg.type
        if t == Gst.MessageType.STATE_CHANGED and msg.src is pipe and not self.started:
            _, new, _ = msg.parse_state_changed()
            if new == Gst.State.PLAYING:
                self.started = True
                self._on_started()
        elif t == Gst.MessageType.EOS:
            if self.items[self.index].get("type") == "video":
                self._advance()  # 图片 EOS 不算结束：kmssink 一直显示最后一帧，到点再切
        elif t == Gst.MessageType.ERROR:
            err, _ = msg.parse_error()
            self._item_failed(err.message)
        return True

    def _on_started(self):
        item = self.items[self.index]
        emit({"event": "playing", "index": self.index, "path": item["path"]})
        self._fade_to(0)
        if len(self.items) == 1 and item.get("type") != "video":
            return  # 单张图片：一直显示，不再切换
        if item.get("type") == "video":
            self.timer = GLib.timeout_add(100, self._check_remaining)
        else:
            dur = max(float(item.get("duration") or 10), 2 * self.fade + 0.2)
            self.timer = GLib.timeout_add(int((dur - self.fade) * 1000), self._image_done)

    def _image_done(self):
        self.timer = 0
        self._fade_to(255, self._next)
        return False

    def _check_remaining(self):
        ok_d, dur = self.pipe.query_duration(Gst.Format.TIME)
        ok_p, pos = self.pipe.query_position(Gst.Format.TIME)
        if ok_d and ok_p and dur > 0 and dur - pos <= self.fade * Gst.SECOND:
            self.timer = 0
            self._fade_to(255)  # 播完（EOS）时再切
            return False
        return True

    def _advance(self):
        if self.alpha >= 255 and not self.fading:
            self._next()
        else:
            self._fade_to(255, self._next)

    def _next(self):
        self._stop_item()
        if self.items:
            self._play(self.index + 1)

    def _item_failed(self, why):
        item = self.items[self.index] if 0 <= self.index < len(self.items) else {}
        log("cannot play %s: %s" % (item.get("path"), why))
        emit({"event": "error", "index": self.index, "message": why})
        self._stop_item()
        self.alpha = 255
        self._push_overlay()
        if len(self.items) > 1:
            GLib.timeout_add(1000, self._retry_next)

    def _retry_next(self):
        if not self.pipe and self.items:
            self._play(self.index + 1)
        return False

    def _stop_item(self):
        if self.timer:
            GLib.source_remove(self.timer)
            self.timer = 0
        if self.pipe:
            pipe, self.pipe = self.pipe, None
            pipe.get_bus().remove_signal_watch()
            pipe.set_state(Gst.State.NULL)

    # ---- 杂项 ----

    def _watch(self, pipe, handler):
        bus = pipe.get_bus()
        bus.add_signal_watch()
        bus.connect("message", handler)

    def _on_overlay_message(self, bus, msg):
        if msg.type == Gst.MessageType.ERROR:
            err, dbg = msg.parse_error()
            log("overlay pipeline failed: %s (%s)" % (err.message, dbg))
            self.loop.quit()  # 上层坏了就没法显示：退出，由代理重启
        return True


def main():
    Gst.init(None)
    loop = GLib.MainLoop()
    player = Player(loop)
    buf = b""

    def on_stdin(fd, cond):
        nonlocal buf
        if cond & (GLib.IO_HUP | GLib.IO_ERR):
            loop.quit()
            return False
        data = os.read(fd, 65536)
        if not data:
            loop.quit()  # 代理退出了
            return False
        buf += data
        while b"\n" in buf:
            line, buf = buf.split(b"\n", 1)
            if not line.strip():
                continue
            req = {}
            try:
                req = json.loads(line)
                reply = player.handle(req) or {}
                reply.update({"id": req.get("id"), "ok": True})
            except Exception as e:  # noqa: BLE001 — 任何错误都回给代理，不让进程死掉
                reply = {"id": req.get("id"), "error": str(e)}
            emit(reply)
        return True

    GLib.io_add_watch(sys.stdin.fileno(), GLib.IO_IN | GLib.IO_HUP | GLib.IO_ERR, on_stdin)
    log("started (%s)" % Gst.version_string())
    loop.run()


if __name__ == "__main__":
    main()
