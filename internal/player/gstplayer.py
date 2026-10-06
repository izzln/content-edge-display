#!/usr/bin/env python3
# display-agent 的播放进程：用 GStreamer 在 DRM/KMS 上直接出画面，视频走 H3 的硬件解码器（cedrus）。
#
# 由 display-agent 拉起并守护（程序内嵌，每次启动重写，随 OTA 更新），stdin/stdout 上逐行收发 JSON：
#   请求  {"id": 1, "cmd": "config", "width": 1440, "height": 900}   （测试可加 "fade": 秒 缩短过渡）
#         {"id": 2, "cmd": "load", "items": [{"path": "...", "type": "image|video", "duration": 10}],
#          "overlay": "/path/x.bgra" | null, "media": [x, y, w, h]}
#         （叠加图已由代理光栅化成输出分辨率的 BGRA，media 是媒体区在输出坐标里的位置）
#         {"id": 3, "cmd": "stats"}   （代理的看门狗定时发它：回不来就重启本进程）
#   回复  {"id": 1, "ok": true, ...} 或 {"id": 1, "error": "..."}
#   事件  {"event": "playing", "index": 0, "path": "..."}  {"event": "error", "index": 0, "message": "..."}
#         {"event": "crop", "size": [w, h], "crop": [左, 右, 上, 下]}
# 日志写 stderr（英文），由 display-agent 转进 journal。
#
# 画面由显示控制器（Allwinner DE2）的两个硬件图层叠出来，不经 GPU、不做整屏软件合成：
#   - 上层（主图层，ARGB）：服务端渲染的模板叠加图。媒体区（下面叫"洞"）是透明的，有底图时可能压着装饰。
#     这一层由本进程经 libdrm 直接驱动：自己设显示模式，两块哑缓冲轮换，画好后台那块再用 drmModeSetPlane 切过去。
#   - 下层（VI 图层，能缩放、吃 NV12）：播放内容，从洞里透明的地方透出来。每一项一条管线，输出交给
#     kmssink（只管这一个图层，不设显示模式）：
#       视频  解码器按 rank 自动选，有 cedrus 时就是 v4l2slh264dec（硬解，NV12 dmabuf 直接导入显示，零拷贝）；
#             cover 撑满媒体区靠 videocrop 打裁剪标记、kmssink 按标记取源矩形，不拷像素。
#       图片  软件裁剪 + 缩放成正好媒体区大小，kmssink 1:1 显示——只算一次，结果确定。
#
# 两个图层的提交都是阻塞式的（内核在下一个 vblank 生效后才返回），谁都不向 DRM 要事件：
# 两边共用一个 fd（只有一个 DRM master），要是各自等翻页/vblank 事件，会互相抢走对方的事件、
# 或者非阻塞翻页撞上另一边进行中的提交（EBUSY），表现为画面卡住不动、也不报错。
#
# 切换过渡（约 0.6 秒淡出到黑 → 淡入）在上层做：洞里垫一层透明度渐变的黑幕，黑幕在叠加图之下——
# 透明处变暗，压进媒体区的装饰不动。下层的硬解视频帧碰不得（dmabuf，CPU 改它太慢），而上层每一步只重画
# 洞所在的那些行。切换的空档里洞里透明的地方是全黑的，所以拆旧管线、建新管线的那一下看不出来；模板属性区始终不动。
#
# 测试/无显示环境：环境变量 DISPLAY_PLAYER_SINK=fakesink 时下层换成 fakesink、上层不输出，不碰 DRM。

import ctypes
import ctypes.util
import json
import mmap
import os
import sys

# GStreamer 自己的错误与少数关键元素的警告也写进日志：现场排查"放不出来"全靠它们说清是哪一环、为什么。
os.environ.setdefault("GST_DEBUG", "1,kmssink:2,v4l2codecs*:2,videocrop:2")
os.environ.setdefault("GST_DEBUG_NO_COLOR", "1")

import gi  # noqa: E402

gi.require_version("Gst", "1.0")
gi.require_version("GstVideo", "1.0")
from gi.repository import GLib, Gst, GstVideo  # noqa: E402

FADE = 0.6          # 切换时淡出、淡入各自的时长（秒）
FADE_STEP_MS = 33
FOURCC_NV12 = 0x3231564E
FOURCC_ARGB8888 = 0x34325241

# 图片：先裁成媒体区的宽高比（videocrop，属性在拿到图片尺寸后设），再缩放到媒体区大小，转成图层吃的格式。
IMAGE_OUTPUT = ("videoconvert ! videocrop name=crop ! videoscale method=4-tap ! "
                "video/x-raw,width={w},height={h},pixel-aspect-ratio=1/1 ! videoconvert ! {sink}")
# 视频：解封装 → 解码 → 显示写死成一条管线（服务端已把视频统一转成 MP4/H.264），不走 playbin 的自动连接。
# 原因在格式协商：解码器第一次协商时，playbin 还没把它连到我们的输出上，由 playbin 代答"下游吃什么"，
# 而 playbin 会把解码器自己能出的全部格式也附在答案末尾；v4l2slh264dec 再按自己的偏好排序，
# 选中 Allwinner 分块格式 NV12_32L32——下游其实显示不了，只能退到逐帧软件解分块（卡顿）。
# 写死管线后解码器从一开始就连着下面的 capsfilter，只能在其中选：
#   - 系统内存 caps：放开的话会协商成 DMA_DRM caps，这条路上的分配查询拿不到 VideoMeta，解码器判协商失败；
#     限定之后帧仍是解码器自己的 dmabuf，kmssink 照样直接导入显示，不拷贝。
#   - 线性格式：硬解出 NV12，软解（没有 cedrus 时）出 I420，图层都能直接显示。
# 每次播放都从第一种开始，只有前一种放不出来时才往后退。不记住退到了哪一种：一次偶然的失败（如换片时 CMA
# 一时分不出来）不能让这个视频从此不裁剪撑满、也不再垫牺牲帧（cedrus 首帧绿斑）：
VIDEO_CAPS = 'capsfilter caps="video/x-raw,format=(string){{NV12,I420}}"'
VIDEO_DIRECT = "filesrc name=src ! qtdemux ! h264parse name=parse ! {decoder} name=dec ! " + VIDEO_CAPS
VIDEO_OUTPUTS = (
    ("cover", VIDEO_DIRECT + " ! videocrop name=crop ! {sink}"),  # 裁剪标记 → 图层取源矩形，撑满媒体区
    ("fit", VIDEO_DIRECT + " ! {sink}"),                          # 不裁：按比例缩放居中，留黑边
    # 兜底：playbin 自动连接，什么封装、编码都能放（如运营方直接拷进目录、没转码的文件），软件转格式后再裁
    ("playbin", "capsfilter caps=video/x-raw ! videoconvert ! videocrop name=crop ! {sink}"),
)
PLAYBIN_VIDEO, PLAYBIN_NATIVE_VIDEO = 0x1, 0x40  # 只要视频（屏幕一律静音，不开声卡）；不让 playbin 自己插转换


def log(msg):
    print("gstplayer: " + msg, file=sys.stderr, flush=True)


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


# ---- DRM（libdrm，经 ctypes） ----

_u16, _u32, _u64, _int, _vp = ctypes.c_uint16, ctypes.c_uint32, ctypes.c_uint64, ctypes.c_int, ctypes.c_void_p


class _Res(ctypes.Structure):
    _fields_ = [("count_fbs", _int), ("fbs", ctypes.POINTER(_u32)),
                ("count_crtcs", _int), ("crtcs", ctypes.POINTER(_u32)),
                ("count_connectors", _int), ("connectors", ctypes.POINTER(_u32)),
                ("count_encoders", _int), ("encoders", ctypes.POINTER(_u32)),
                ("min_width", _u32), ("max_width", _u32), ("min_height", _u32), ("max_height", _u32)]


class _ModeInfo(ctypes.Structure):
    _fields_ = [("clock", _u32), ("hdisplay", _u16), ("hsync_start", _u16), ("hsync_end", _u16),
                ("htotal", _u16), ("hskew", _u16), ("vdisplay", _u16), ("vsync_start", _u16),
                ("vsync_end", _u16), ("vtotal", _u16), ("vscan", _u16), ("vrefresh", _u32),
                ("flags", _u32), ("type", _u32), ("name", ctypes.c_char * 32)]


class _Connector(ctypes.Structure):
    _fields_ = [("connector_id", _u32), ("encoder_id", _u32), ("connector_type", _u32),
                ("connector_type_id", _u32), ("connection", _int), ("mm_width", _u32), ("mm_height", _u32),
                ("subpixel", _int), ("count_modes", _int), ("modes", ctypes.POINTER(_ModeInfo)),
                ("count_props", _int), ("props", ctypes.POINTER(_u32)), ("prop_values", ctypes.POINTER(_u64)),
                ("count_encoders", _int), ("encoders", ctypes.POINTER(_u32))]


class _Encoder(ctypes.Structure):
    _fields_ = [("encoder_id", _u32), ("encoder_type", _u32), ("crtc_id", _u32),
                ("possible_crtcs", _u32), ("possible_clones", _u32)]


class _PlaneRes(ctypes.Structure):
    _fields_ = [("count_planes", _u32), ("planes", ctypes.POINTER(_u32))]


class _Plane(ctypes.Structure):
    _fields_ = [("count_formats", _u32), ("formats", ctypes.POINTER(_u32)),
                ("plane_id", _u32), ("crtc_id", _u32), ("fb_id", _u32),
                ("crtc_x", _u32), ("crtc_y", _u32), ("x", _u32), ("y", _u32),
                ("possible_crtcs", _u32), ("gamma_size", _u32)]


class _ObjProps(ctypes.Structure):
    _fields_ = [("count_props", _u32), ("props", ctypes.POINTER(_u32)), ("prop_values", ctypes.POINTER(_u64))]


class _Prop(ctypes.Structure):
    _fields_ = [("prop_id", _u32), ("flags", _u32), ("name", ctypes.c_char * 32)]


DRM_CLIENT_CAP_UNIVERSAL_PLANES = 2
DRM_MODE_OBJECT_PLANE = 0xEEEEEEEE
DRM_MODE_CONNECTED = 1
PLANE_OVERLAY, PLANE_PRIMARY = 0, 1


def _libdrm():
    lib = ctypes.CDLL(ctypes.util.find_library("drm") or "libdrm.so.2", use_errno=True)
    # 参数类型必须写全：设备是 32 位 ARM，64 位参数要占一对寄存器，0xEEEEEEEE 也超出 int，
    # 让 ctypes 按默认的 int 去传，参数会错位。
    p = ctypes.POINTER
    for fn, args, res in (
        ("drmSetClientCap", [_int, _u64, _u64], _int),
        ("drmModeGetResources", [_int], p(_Res)),
        ("drmModeFreeResources", [_vp], None),
        ("drmModeGetConnector", [_int, _u32], p(_Connector)),
        ("drmModeFreeConnector", [_vp], None),
        ("drmModeGetEncoder", [_int, _u32], p(_Encoder)),
        ("drmModeFreeEncoder", [_vp], None),
        ("drmModeGetPlaneResources", [_int], p(_PlaneRes)),
        ("drmModeFreePlaneResources", [_vp], None),
        ("drmModeGetPlane", [_int, _u32], p(_Plane)),
        ("drmModeFreePlane", [_vp], None),
        ("drmModeObjectGetProperties", [_int, _u32, _u32], p(_ObjProps)),
        ("drmModeFreeObjectProperties", [_vp], None),
        ("drmModeGetProperty", [_int, _u32], p(_Prop)),
        ("drmModeFreeProperty", [_vp], None),
        ("drmModeObjectSetProperty", [_int, _u32, _u32, _u32, _u64], _int),
        ("drmModeCreateDumbBuffer", [_int, _u32, _u32, _u32, _u32, p(_u32), p(_u32), p(_u64)], _int),
        ("drmModeMapDumbBuffer", [_int, _u32, p(_u64)], _int),
        ("drmModeAddFB2", [_int, _u32, _u32, _u32, p(_u32), p(_u32), p(_u32), p(_u32), _u32], _int),
        ("drmModeSetCrtc", [_int, _u32, _u32, _u32, _u32, p(_u32), _int, p(_ModeInfo)], _int),
        ("drmModeSetPlane", [_int, _u32, _u32, _u32, _u32, _int, _int, _u32, _u32,
                             _u32, _u32, _u32, _u32], _int),
    ):
        f = getattr(lib, fn)
        f.argtypes, f.restype = args, res
    return lib


def _check(ret, what):
    if ret != 0:
        err = ctypes.get_errno() or -ret
        raise OSError(err, "%s: %s" % (what, os.strerror(err)))


def _plane_props(drm, fd, plane_id):
    """图层的属性：{名字: (属性 id, 当前值)}。"""
    out = {}
    props = drm.drmModeObjectGetProperties(fd, plane_id, DRM_MODE_OBJECT_PLANE)
    if not props:
        return out
    try:
        for i in range(props.contents.count_props):
            prop = drm.drmModeGetProperty(fd, props.contents.props[i])
            if prop:
                out[prop.contents.name.decode()] = (prop.contents.prop_id, int(props.contents.prop_values[i]))
                drm.drmModeFreeProperty(prop)
    finally:
        drm.drmModeFreeObjectProperties(props)
    return out


def _planes(drm, fd):
    """列出全部图层（含主图层）：[{id, type, formats, crtcs}]。"""
    drm.drmSetClientCap(fd, DRM_CLIENT_CAP_UNIVERSAL_PLANES, 1)
    out = []
    pres = drm.drmModeGetPlaneResources(fd)
    if not pres:
        return out
    try:
        for i in range(pres.contents.count_planes):
            p = drm.drmModeGetPlane(fd, pres.contents.planes[i])
            if not p:
                continue
            try:
                pl = p.contents
                ptype = _plane_props(drm, fd, pl.plane_id).get("type", (0, -1))[1]
                out.append({"id": pl.plane_id, "type": ptype,
                            "formats": {pl.formats[j] for j in range(pl.count_formats)},
                            "crtcs": pl.possible_crtcs})
            finally:
                drm.drmModeFreePlane(p)
    finally:
        drm.drmModeFreePlaneResources(pres)
    return out


def pick_planes(planes, crtc_bit=1):
    """选上层（模板叠加图，ARGB）与下层（播放内容，NV12）各用哪个图层，只看所用 CRTC 上的。

    上层必须是主图层：显示模式设在 CRTC 的主图层上，而且 Allwinner DE2 只有 UI 图层吃 ARGB8888
    （VI 图层只有 XRGB/YUV，没有 alpha）。下层是能显示 NV12 的覆盖图层。"""
    on_crtc = [p for p in planes if p["crtcs"] & crtc_bit]
    argb = [p for p in on_crtc if FOURCC_ARGB8888 in p["formats"]]
    nv12 = [p for p in on_crtc if FOURCC_NV12 in p["formats"]]
    # 读不到图层类型时退而求其次：第一个吃 ARGB 的图层当上层（DE2 上就是主图层），其余吃 NV12 的当下层
    up = next((p for p in argb if p["type"] == PLANE_PRIMARY), argb[0] if argb else None)
    down = next((p for p in nv12 if p is not up and p["type"] != PLANE_PRIMARY), None)
    return (up["id"] if up else -1), (down["id"] if down else -1)


_veils = {}


def veil(a):
    """透明度通道的查找表：叠加图的像素盖在透明度为 a 的黑幕上之后的透明度（用到才算，按 a 缓存）。
    预乘格式下黑幕不贡献颜色，所以 rgb 不变，只有 alpha 从 p 变成 p + a·(255−p)/255。"""
    if a not in _veils:
        _veils[a] = bytes(p + (a * (255 - p) + 127) // 255 for p in range(256))
    return _veils[a]


def deco_spans(base, row, hole):
    """洞里每一行不透明像素（压进媒体区的装饰）所在的列范围 (起, 止)，没有就是 None。
    每个画面算一次，淡入淡出的每一帧都用它（row 是 base 每行的字节数）。"""
    x, y, w, h = hole
    spans = []
    for r in range(y, y + h):
        a = base[r * row + x * 4 + 3:r * row + (x + w) * 4:4]
        lead = w - len(a.lstrip(b"\0"))
        spans.append(None if lead == w else (lead, len(a.rstrip(b"\0"))))
    return spans


def paint(buf, pitch, width, base, hole, spans, alpha, full):
    """把一帧上层画面画进 buf（每行 pitch 字节，宽 width 像素，BGRA 预乘）。

    full 时先整幅铺上 base；然后洞里垫一层 alpha 的黑幕——黑幕在叠加图之下：洞里透明的地方变暗（视频淡出），
    底图压进媒体区的装饰（spans，见 deco_spans）保持不动。alpha 为 0 时洞里还原 base。
    只改透明度通道、按行查表，走的是 C 实现。"""
    row = width * 4
    src = memoryview(base)
    if full:
        if pitch == row:
            buf[:len(base)] = src
        else:
            for r in range(len(base) // row):
                buf[r * pitch:r * pitch + row] = src[r * row:(r + 1) * row]
        if not alpha:
            return
    x, y, w, h = hole
    if not alpha:
        for r in range(y, y + h):
            o, s = r * pitch + x * 4, r * row + x * 4
            buf[o:o + w * 4] = src[s:s + w * 4]
        return
    fill, lut = bytes((0, 0, 0, alpha)) * w, veil(alpha)
    for r, span in zip(range(y, y + h), spans):
        o = r * pitch + x * 4
        buf[o:o + w * 4] = fill  # 洞里透明的地方：黑幕本身
        if span:  # 这一行有压进媒体区的装饰：只对这一段查表
            a, b = span
            s = r * row + (x + a) * 4
            seg = bytearray(src[s:s + (b - a) * 4])
            seg[3::4] = seg[3::4].translate(lut)
            buf[o + a * 4:o + b * 4] = seg


class Display:
    """DRM 输出。上层（主图层）在这里直接驱动；下层的图层 id 交给 kmssink。"""

    def __init__(self, width, height):
        self.drm = drm = _libdrm()
        self.width, self.height = width, height
        self.fd = -1
        for i in range(8):
            path = "/dev/dri/card%d" % i
            if not os.path.exists(path):
                continue
            fd = os.open(path, os.O_RDWR | os.O_CLOEXEC)
            out = self._find_output(fd)
            if out:
                self.fd, self.path = fd, path
                self.conn, self.crtc, crtc_index, self.mode = out
                break
            os.close(fd)  # H3 上 GPU 也是一个 card，没有显示接口
        if self.fd < 0:
            raise RuntimeError("no connected display under /dev/dri")
        self.primary, self.video = pick_planes(_planes(drm, self.fd), 1 << crtc_index)
        if self.primary < 0 or self.video < 0:
            raise RuntimeError("no usable planes (overlay %d, video %d)" % (self.primary, self.video))
        self.bufs = [self._dumb() for _ in range(2)]
        # 叠放次序：内容在下、叠加图在上。DE2 的默认值本来就是这样，这里写明，不依赖默认。
        self._set_prop(self.video, "zpos", 0)
        self._set_prop(self.primary, "zpos", 1)
        _check(drm.drmModeSetCrtc(self.fd, self.crtc, self.bufs[0]["fb"], 0, 0,
                                  (_u32 * 1)(self.conn), 1, ctypes.byref(self.mode)), "set display mode")
        self.front = 0
        self.failed = False

    def _find_output(self, fd):
        """第一个已连接的显示接口：(接口 id, CRTC id, CRTC 序号, 显示模式)；没有接口的设备返回 None。"""
        drm = self.drm
        res = drm.drmModeGetResources(fd)
        if not res:
            return None
        try:
            r = res.contents
            crtcs = [r.crtcs[i] for i in range(r.count_crtcs)]
            for i in range(r.count_connectors):
                c = drm.drmModeGetConnector(fd, r.connectors[i])
                if not c:
                    continue
                try:
                    cc = c.contents
                    if cc.connection != DRM_MODE_CONNECTED or cc.count_modes <= 0:
                        continue
                    modes = [cc.modes[j] for j in range(cc.count_modes)]
                    # 同一分辨率下内核按刷新率从高到低排，取第一个；分辨率由代理按接口提供的模式定好，必然在列表里
                    mode = next((m for m in modes if (m.hdisplay, m.vdisplay) == (self.width, self.height)), None)
                    if mode is None:
                        raise RuntimeError("display does not offer %dx%d (modes: %s)" % (
                            self.width, self.height, " ".join(sorted({m.name.decode() for m in modes}))))
                    idx = self._crtc_index(fd, cc, crtcs)
                    if idx < 0:
                        raise RuntimeError("no CRTC for connector %d" % cc.connector_id)
                    return cc.connector_id, crtcs[idx], idx, _ModeInfo.from_buffer_copy(mode)
                finally:
                    drm.drmModeFreeConnector(c)
        finally:
            drm.drmModeFreeResources(res)
        return None

    def _crtc_index(self, fd, conn, crtcs):
        for eid in [conn.encoder_id] + [conn.encoders[k] for k in range(conn.count_encoders)]:
            e = self.drm.drmModeGetEncoder(fd, eid) if eid else None
            if not e:
                continue
            try:
                if e.contents.crtc_id in crtcs:
                    return crtcs.index(e.contents.crtc_id)
                for k in range(len(crtcs)):
                    if e.contents.possible_crtcs & (1 << k):
                        return k
            finally:
                self.drm.drmModeFreeEncoder(e)
        return -1

    def _dumb(self):
        drm, fd = self.drm, self.fd
        handle, pitch, size, offset, fb = _u32(), _u32(), _u64(), _u64(), _u32()
        _check(drm.drmModeCreateDumbBuffer(fd, self.width, self.height, 32, 0, ctypes.byref(handle),
                                           ctypes.byref(pitch), ctypes.byref(size)), "create buffer")
        _check(drm.drmModeAddFB2(fd, self.width, self.height, FOURCC_ARGB8888, (_u32 * 4)(handle.value),
                                 (_u32 * 4)(pitch.value), (_u32 * 4)(), ctypes.byref(fb), 0), "add framebuffer")
        _check(drm.drmModeMapDumbBuffer(fd, handle.value, ctypes.byref(offset)), "map buffer")
        m = mmap.mmap(fd, size.value, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE, offset=offset.value)
        return {"fb": fb.value, "map": m, "pitch": pitch.value, "drawn": None}

    def _set_prop(self, plane, name, value):
        prop = _plane_props(self.drm, self.fd, plane).get(name)
        if prop and prop[1] != value:
            ret = self.drm.drmModeObjectSetProperty(self.fd, plane, DRM_MODE_OBJECT_PLANE, prop[0], value)
            if ret:
                log("cannot set %s=%d on plane %d: %s" % (name, value, plane, os.strerror(ctypes.get_errno() or -ret)))

    def describe(self):
        return "%s, connector %d, crtc %d, %s@%dHz, overlay plane %d, video plane %d" % (
            self.path, self.conn, self.crtc, self.mode.name.decode(), self.mode.vrefresh, self.primary, self.video)

    def show(self, base, hole, spans, alpha):
        """把上层画到后台缓冲并切过去（阻塞到下一个 vblank 生效，之后前台那块才可以再画）。"""
        b = self.bufs[1 - self.front]
        drawn = b["drawn"]
        full = drawn is None or drawn[0] is not base or drawn[1] != hole
        if full or drawn[2] != alpha:
            paint(b["map"], b["pitch"], self.width, base, hole, spans, alpha, full)
            b["drawn"] = (base, hole, alpha)
        ret = self.drm.drmModeSetPlane(self.fd, self.primary, self.crtc, b["fb"], 0, 0, 0, self.width, self.height,
                                       0, 0, self.width << 16, self.height << 16)
        if ret:
            if not self.failed:
                log("cannot update overlay plane: %s" % os.strerror(ctypes.get_errno() or -ret))
            self.failed = True
            return
        self.failed = False
        self.front = 1 - self.front


def cover_crop(src_w, src_h, dst_w, dst_h):
    """按 cover 撑满目标区域时，源画面四边各裁掉多少像素（左, 右, 上, 下）。

    保留的宽高与左、上的偏移都取偶数：NV12 的色度是 2×2 一组，奇数偏移会让图层拒收或颜色错位。"""
    if src_w <= 0 or src_h <= 0 or dst_w <= 0 or dst_h <= 0:
        return 0, 0, 0, 0
    if src_w * dst_h > dst_w * src_h:  # 源更宽：裁左右
        cut = src_w - (src_h * dst_w // dst_h & ~1)
        return cut // 2 & ~1, cut - (cut // 2 & ~1), 0, 0
    cut = src_h - (src_w * dst_h // dst_w & ~1)  # 源更高：裁上下
    return 0, 0, cut // 2 & ~1, cut - (cut // 2 & ~1)


def prime_decoder(pipe):
    """让解码器先白解一次第一帧。

    cedrus 每个解码会话解的第一帧会坏：只解出顶上一截，其余是没写过的缓冲（YUV 全 0，显示成绿色）；
    后面的帧都参考它，绿斑要到下一个 I 帧才消失。会话里第二次起的解码都正常。所以把第一帧的码流
    复制一份先送进解码器当牺牲品，它解出来的那一帧在解码器出口丢掉——两份时间戳相同，
    解码器先输出的就是先解的那份。软解时多解一帧，无害。"""
    def dup_first(pad, info):
        pad.remove_probe(info.id)
        pad.push(info.get_buffer().copy())
        return Gst.PadProbeReturn.OK

    def drop_first(pad, info):
        pad.remove_probe(info.id)
        return Gst.PadProbeReturn.DROP
    pipe.get_by_name("parse").get_static_pad("src").add_probe(Gst.PadProbeType.BUFFER, dup_first)
    pipe.get_by_name("dec").get_static_pad("src").add_probe(Gst.PadProbeType.BUFFER, drop_first)


class Player:
    def __init__(self):
        self.test = os.environ.get("DISPLAY_PLAYER_SINK") == "fakesink"
        self.display = None
        self.width = self.height = 0
        self.fade = FADE
        self.h264_decoder = "avdec_h264"  # 有 cedrus 时是 v4l2slh264dec（硬解），见 configure
        self.base = b""          # 上层叠加图（BGRA，预乘 alpha）；没有模板时全透明
        self.hole = (0, 0, 0, 0)  # 媒体区在显示坐标里的位置
        self.spans = []           # 洞里压着装饰的列范围（deco_spans）
        self.scene = None
        self.items = []
        self.index = -1
        self.shown = set()        # 本次 load 以来已经显示过的项（只为第一次显示记日志）
        self.pipe = None          # 当前播放项的管线
        self.bus_handler = 0      # 当前管线总线上的消息回调
        self.started = False      # 当前项是否已开始显示
        self.source = ""          # 当前项的画面尺寸与裁剪（日志用）
        self.timer = 0            # 图片到点淡出 / 视频剩余时间巡检
        self.fading = 0           # 进行中的淡入淡出（GLib 定时器 id）
        self.alpha = 255          # 洞里黑幕的不透明度：255 全黑，0 透出播放内容
        self.decoder = ""         # 最近一次视频用的解码器
        self.out_mode = None          # 当前视频项用的输出方式（VIDEO_OUTPUTS 的下标）

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
            return {"hwdec": hw}
        raise ValueError("unknown command %r" % cmd)

    def configure(self, req):
        """设显示模式（代理在每次拉起本进程后只发一次）。"""
        width, height = int(req["width"]), int(req["height"])
        self.fade = float(req.get("fade", FADE))
        if Gst.ElementFactory.find("v4l2slh264dec"):
            self.h264_decoder = "v4l2slh264dec"
        if not self.test:
            self.display = Display(width, height)
            log("display " + self.display.describe())
            if self.h264_decoder != "v4l2slh264dec":
                log("WARNING: hardware decoder v4l2slh264dec is not available "
                    "(cedrus missing or gstreamer1.0-plugins-bad not installed); videos will be decoded in software")
        self.width, self.height = width, height
        self.base = bytes(width * height * 4)
        self.hole = (0, 0, width, height)
        self.spans = [None] * height
        self._show()
        return {}

    def load(self, req):
        if not self.width:
            raise RuntimeError("not configured")
        scene = {k: v for k, v in req.items() if k not in ("id", "cmd")}
        if scene == self.scene:
            return {}  # 内容没变（代理重发）：不打断播放
        overlay = req.get("overlay")
        if overlay:
            with open(overlay, "rb") as f:
                base = f.read()
            if len(base) != self.width * self.height * 4:
                raise ValueError("overlay %s is not %dx%d BGRA" % (overlay, self.width, self.height))
            hole = tuple(req["media"])
        else:
            base, hole = bytes(self.width * self.height * 4), (0, 0, self.width, self.height)
        items = [it for it in req.get("items", []) if it.get("path")]
        spans = deco_spans(base, self.width * 4, hole)

        def switch():
            self.scene, self.base, self.hole, self.spans, self.items = scene, base, hole, spans, items
            self.shown = set()
            self._stop_item()
            self.alpha = 255
            self._show()
            log("scene: %d item(s), media area %dx%d at %d,%d%s" % (
                len(items), hole[2], hole[3], hole[0], hole[1], "" if overlay else " (no template)"))
            if items:
                self._play(0)
        if self.pipe and self.alpha < 255:
            self._fade_to(255, switch)  # 先把当前内容淡出，再换
        else:
            switch()
        return {}

    # ---- 上层：叠加图与淡入淡出 ----

    def _show(self):
        if self.display:
            self.display.show(self.base, self.hole, self.spans, self.alpha)

    def _fade_to(self, target, done=None):
        steps = max(1, int(self.fade * 1000 / FADE_STEP_MS))
        start = self.alpha
        if self.fading:
            GLib.source_remove(self.fading)
            self.fading = 0
        if start == target or self.fade <= 0:
            self.alpha = target
            self._show()
            if done:
                done()
            return
        state = {"n": 0}

        def step():
            state["n"] += 1
            p = min(1.0, state["n"] / steps)
            p = p * p * (3 - 2 * p)  # smoothstep，起止更柔和
            self.alpha = round(start + (target - start) * p)
            self._show()
            if state["n"] >= steps:
                self.fading = 0
                if done:
                    done()
                return False
            return True
        self.fading = GLib.timeout_add(FADE_STEP_MS, step)

    # ---- 下层：播放项 ----

    def _play(self, index, mode=0):
        self.index = index % len(self.items)
        item = self.items[self.index]
        x, y, w, h = self.hole
        d = self.display
        if self.test:
            sink = "fakesink name=sink sync=true"
        else:
            sink = "kmssink name=sink fd=%d connector-id=%d plane-id=%d skip-vsync=true" % (d.fd, d.conn, d.video)
        if item.get("type") == "video":
            name, desc = VIDEO_OUTPUTS[mode]
            desc = desc.format(decoder=self.h264_decoder, sink=sink)
        else:
            mode, name, desc = None, "playbin", IMAGE_OUTPUT.format(w=w, h=h, sink=sink)

        if name == "playbin":
            out = Gst.parse_bin_from_description(desc, True)
            pipe = Gst.ElementFactory.make("playbin", None)
            pipe.set_property("uri", Gst.filename_to_uri(item["path"]))
            pipe.set_property("video-sink", out)
            pipe.set_property("flags", PLAYBIN_VIDEO | PLAYBIN_NATIVE_VIDEO)
            pipe.connect("deep-element-added", self._on_element)
        else:
            out = pipe = Gst.parse_launch(desc)
            pipe.get_by_name("src").set_property("location", item["path"])
            self.decoder = self.h264_decoder
            prime_decoder(pipe)
        if d:
            GstVideo.VideoOverlay.set_render_rectangle(out.get_by_name("sink"), x, y, w, h)
        crop = out.get_by_name("crop")
        if crop:
            crop.get_static_pad("sink").add_probe(Gst.PadProbeType.EVENT_DOWNSTREAM, self._on_caps, (crop, w, h))

        bus = pipe.get_bus()
        bus.add_signal_watch()
        self.bus_handler = bus.connect("message", lambda _bus, msg: self._on_item_message(pipe, msg))
        self.pipe, self.started, self.source, self.out_mode = pipe, False, "", mode
        if pipe.set_state(Gst.State.PLAYING) == Gst.StateChangeReturn.FAILURE:
            self._item_failed("cannot start pipeline")

    def _on_caps(self, pad, info, data):
        # 回调里抛异常会让整条管线报 data stream error：算不出裁剪就不裁，也不能把播放搞挂。
        try:
            ev = info.get_event()
            if ev.type == Gst.EventType.CAPS:
                crop, w, h = data
                # 用 VideoInfo 取宽高，不碰 GstStructure：gst-python 1.26 起 caps.get_structure()
                # 返回的是要配合 with 用的包装对象，1.24 及以前又是普通对象，两边写法不通用。
                vi = GstVideo.VideoInfo.new_from_caps(ev.parse_caps())
                if vi:
                    left, right, top, bottom = cover_crop(vi.width, vi.height, w, h)
                    emit({"event": "crop", "size": [vi.width, vi.height], "crop": [left, right, top, bottom]})
                    self.source = "%dx%d %s, crop l%d r%d t%d b%d" % (
                        vi.width, vi.height, vi.finfo.name, left, right, top, bottom)
                    crop.set_property("left", left)
                    crop.set_property("right", right)
                    crop.set_property("top", top)
                    crop.set_property("bottom", bottom)
        except Exception as e:  # noqa: BLE001
            log("cannot compute cover crop: %s" % e)
        return Gst.PadProbeReturn.OK

    def _on_element(self, _bin, _sub, element):
        f = element.get_factory()
        if f and "Decoder/Video" in (f.get_metadata("klass") or ""):
            self.decoder = f.get_name()  # 写进 showing 日志，也供 stats 上报硬解/软解

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
            err, dbg = msg.parse_error()
            src = msg.src.get_path_string() if msg.src else "?"
            detail = "%s [%s: %s]" % (err.message, src, (dbg or "").replace("\n", " "))
            if self._video_fallback(detail):
                return True
            self._item_failed(err.message, detail)
        return True

    def _video_fallback(self, detail):
        """视频放不出来时，换下一种输出方式重放同一项；都试过了就返回 False（按坏文件跳过）。"""
        item = self.items[self.index]
        if item.get("type") != "video" or self.started:
            return False
        if self.out_mode + 1 >= len(VIDEO_OUTPUTS):
            return False
        log("video output '%s' failed for %s: %s; retrying with '%s'" % (
            VIDEO_OUTPUTS[self.out_mode][0], os.path.basename(item["path"]), detail, VIDEO_OUTPUTS[self.out_mode + 1][0]))
        self._stop_item()
        self._play(self.index, self.out_mode + 1)
        return True

    def _on_started(self):
        item = self.items[self.index]
        if self.index not in self.shown:
            self.shown.add(self.index)
            how = ""
            if item.get("type") == "video":
                how = ", %s via %s" % (self.decoder, VIDEO_OUTPUTS[self.out_mode][0])
            log("showing %s (%s%s%s)" % (os.path.basename(item["path"]), item.get("type"), how,
                                        ", " + self.source if self.source else ""))
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

    def _item_failed(self, why, detail=None):
        item = self.items[self.index] if 0 <= self.index < len(self.items) else {}
        log("cannot play %s: %s" % (item.get("path"), detail or why))
        emit({"event": "error", "index": self.index, "message": why})
        self._stop_item()
        self.alpha = 255
        self._show()
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
            bus = pipe.get_bus()
            # 回调闭包引用着管线、管线又持有总线：不断开这个环，每一项的管线都释放不掉
            bus.disconnect(self.bus_handler)
            bus.remove_signal_watch()
            pipe.set_state(Gst.State.NULL)


def main():
    Gst.init(None)
    loop = GLib.MainLoop()
    player = Player()
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
