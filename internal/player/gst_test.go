package player

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/testutil"
)

// 测试二进制兼任"假播放进程"：GST 启动的子进程就是它自己（见 fakePlayer）。
func TestMain(m *testing.M) {
	if os.Getenv("GST_FAKE_PLAYER") == "1" {
		fakePlayer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakePlayer 按 gstplayer.py 的协议应答，并把收到的请求逐行记进 GST_FAKE_LOG。
//   - GST_FAKE_EXIT_ONCE=<标记文件>：第一次收到 load 后退出（模拟崩溃），之后正常；
//   - GST_FAKE_HANG_ONCE=<标记文件>：第一次运行时不回状态查询（模拟卡死）；
//   - GST_FAKE_DIE=1：一启动就退出（模拟起不来，如 HDMI 没接）。
func fakePlayer() {
	logPath := os.Getenv("GST_FAKE_LOG")
	if os.Getenv("GST_FAKE_DIE") == "1" {
		f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, "%d\n", time.Now().UnixMilli())
		f.Close()
		os.Exit(1)
	}
	once := func(env string) bool {
		marker := os.Getenv(env)
		if marker == "" {
			return false
		}
		if _, err := os.Stat(marker); err == nil {
			return false
		}
		os.WriteFile(marker, nil, 0o644)
		return true
	}
	hang := once("GST_FAKE_HANG_ONCE")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var req map[string]any
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(sc.Bytes(), '\n'))
			f.Close()
		}
		reply := map[string]any{"id": req["id"], "ok": true}
		switch req["cmd"] {
		case "stats":
			if hang {
				continue
			}
			reply["hwdec"] = "v4l2slh264dec"
		}
		out, _ := json.Marshal(reply)
		os.Stdout.Write(append(out, '\n'))
		if req["cmd"] == "load" && once("GST_FAKE_EXIT_ONCE") {
			os.Exit(3)
		}
	}
}

// startFake 用假播放进程启动 GST，返回它和请求记录文件。
func startFake(t *testing.T, w, h int, env ...string) (*GST, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.log")
	p := NewGST(dir, w, h)
	p.callTimeout, p.watchInterval, p.restartDelay = time.Second, 200*time.Millisecond, 50*time.Millisecond
	p.python = os.Args[0]
	p.env = append([]string{"GST_FAKE_PLAYER=1", "GST_FAKE_LOG=" + logPath}, env...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return p, logPath
}

func requests(path string) []map[string]any {
	data, _ := os.ReadFile(path)
	var out []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func cmds(path string, which string) []map[string]any {
	var out []map[string]any
	for _, r := range requests(path) {
		if r["cmd"] == which {
			out = append(out, r)
		}
	}
	return out
}

// overlayPNG 造一张 w×h 的叠加图：左半不透明、右半（媒体区）全透明。
func overlayPNG(t *testing.T, dir string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w/2; x++ {
			img.SetRGBA(x, y, color.RGBA{0x1E, 0x3A, 0x8A, 0xFF})
		}
	}
	path := filepath.Join(dir, "ovl.png")
	var buf bytes.Buffer
	png.Encode(&buf, img)
	os.WriteFile(path, buf.Bytes(), 0o644)
	return path
}

func TestStartWritesPlayerScript(t *testing.T) {
	p, _ := startFake(t, 1440, 900)
	got, err := os.ReadFile(p.script)
	if err != nil || !bytes.Equal(got, gstScript) {
		t.Fatalf("播放脚本应按程序内嵌的版本写出：%v", err)
	}
}

// 播放进程先拿到显示配置，再拿到画面；它退出后要被拉起来，并重新拿到同一个画面
// （内容没变时服务端只回 304，不重发的话一次崩溃就黑屏到下次内容变化）。
func TestConfigThenSceneAndResendAfterRestart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed")
	p, logPath := startFake(t, 1440, 900, "GST_FAKE_EXIT_ONCE="+marker)
	scene := Scene{Items: []Item{{Path: "/m/a.jpg", Type: "image", Duration: 7}, {Path: "/m/b.mp4", Type: "video"}}}
	if err := p.Load(scene); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, 5*time.Second, "重启后重新下发画面", func() bool { return len(cmds(logPath, "load")) >= 2 })

	reqs := requests(logPath)
	if reqs[0]["cmd"] != "config" || reqs[0]["width"] != 1440.0 || reqs[0]["height"] != 900.0 {
		t.Fatalf("第一个请求应是显示配置：%v", reqs[0])
	}
	if len(cmds(logPath, "config")) < 2 {
		t.Fatal("重启后应重新配置显示")
	}
	for _, l := range cmds(logPath, "load") {
		items := l["items"].([]any)
		first := items[0].(map[string]any)
		if len(items) != 2 || first["path"] != "/m/a.jpg" || first["duration"] != 7.0 || l["overlay"] != nil {
			t.Fatalf("画面内容不对：%v", l)
		}
	}
}

// 叠加图要按显示屏实际输出分辨率光栅化（输出 1080p 而模板是 1440×900 时也不能错位），
// 媒体区按画布坐标交给播放进程，由它换算。
func TestOverlayRasterizedToOutputSize(t *testing.T) {
	p, logPath := startFake(t, 1920, 1080)
	pngPath := overlayPNG(t, t.TempDir(), 1440, 900)
	err := p.Load(Scene{
		Items:      []Item{{Path: "/m/a.mp4", Type: "video"}},
		OverlayPNG: pngPath,
		Media:      manifest.Rect{X: 720, Y: 0, W: 720, H: 900},
	})
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, 3*time.Second, "下发画面", func() bool { return len(cmds(logPath, "load")) >= 1 })
	l := cmds(logPath, "load")[0]
	if l["overlay"] != pngPath+".1920x1080.bgra" {
		t.Fatalf("叠加图应光栅化到输出分辨率，得到 %v", l["overlay"])
	}
	if !jsonEq(l["media"], []int{960, 0, 960, 1080}) {
		t.Fatalf("媒体区应按同一比例换算到输出坐标：%v", l["media"])
	}
	raw, err := os.ReadFile(pngPath + ".1920x1080.bgra")
	if err != nil || len(raw) != 1920*1080*4 {
		t.Fatalf("光栅化结果应正好 1920×1080×4 字节：%v %d", err, len(raw))
	}
	if a := raw[(540*1920+1440)*4+3]; a != 0 {
		t.Fatalf("媒体区（右半）alpha = %d，应为 0（全透明）", a)
	}
	if a := raw[(540*1920+200)*4+3]; a != 0xFF {
		t.Fatalf("属性区（左半）alpha = %d，应为 255", a)
	}
}

func jsonEq(got any, want []int) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return bytes.Equal(a, b)
}

// 播放进程接连起不来时重启间隔翻倍，不要每 2 秒拉一次、刷满日志。
func TestRestartBacksOff(t *testing.T) {
	_, logPath := startFake(t, 1440, 900, "GST_FAKE_DIE=1")
	starts := func() []int64 {
		data, _ := os.ReadFile(logPath)
		var out []int64
		for _, l := range strings.Fields(string(data)) {
			n, _ := strconv.ParseInt(l, 10, 64)
			out = append(out, n)
		}
		return out
	}
	testutil.WaitFor(t, 5*time.Second, "起了 4 次", func() bool { return len(starts()) >= 4 })
	// 间隔依次是 50、100、200 毫秒，再加上每次拉起进程本身的耗时（负载高时几十到上百毫秒、忽大忽小）。
	// 比较差值而不是倍数，把拉起耗时抵消掉：第三次比第一次多等 150 毫秒，留出抖动余量。
	s := starts()
	if g1, g3 := s[1]-s[0], s[3]-s[2]; g3-g1 < 100 {
		t.Fatalf("重启间隔应翻倍增长：%v", s)
	}
}

func TestStatsFromPlayer(t *testing.T) {
	p, logPath := startFake(t, 1440, 900)
	testutil.WaitFor(t, 3*time.Second, "配置显示", func() bool { return len(cmds(logPath, "config")) >= 1 })
	testutil.WaitFor(t, 3*time.Second, "问到状态", func() bool { return p.Stats().HWDec != "" })
	if st := p.Stats(); st.HWDec != "v4l2slh264dec" || st.OutputW != 1440 || st.OutputH != 900 {
		t.Fatalf("got %+v", st)
	}
}

// 播放进程卡死（不退出、也不应答）时要被杀掉重启。
func TestHungPlayerIsRestarted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "hung")
	p, logPath := startFake(t, 1440, 900, "GST_FAKE_HANG_ONCE="+marker)
	p.Load(Scene{Items: []Item{{Path: "/m/a.jpg", Type: "image"}}})
	testutil.WaitFor(t, 8*time.Second, "卡死后重启", func() bool { return len(cmds(logPath, "config")) >= 2 })
	testutil.WaitFor(t, 3*time.Second, "重启后重新下发画面", func() bool { return len(cmds(logPath, "load")) >= 2 })
}

// ---- 真实播放脚本（需要 Python + GStreamer，没有就跳过）----

func gstPython(t *testing.T) string {
	t.Helper()
	check := `import gi; gi.require_version("Gst","1.0"); gi.require_version("GstVideo","1.0")
from gi.repository import Gst, GstVideo; Gst.init(None)
assert all(Gst.ElementFactory.find(n) for n in ("playbin","videocrop","videoscale","videoconvert","fakesink","jpegdec","qtdemux","h264parse","avdec_h264"))`
	for _, py := range []string{"python3", "python3.13", "python3.12", "python3.11", "/usr/bin/python3"} {
		if exec.Command(py, "-c", check).Run() == nil {
			return py
		}
	}
	t.Skip("没有带 GStreamer 绑定的 Python（apt install python3-gst-1.0 gstreamer1.0-plugins-good）")
	return ""
}

// 纯计算部分：cover 裁剪与淡入淡出用的叠加帧。
func TestPlayerScriptHelpers(t *testing.T) {
	py := gstPython(t)
	scriptPath(t)
	out, err := exec.Command(py, "-c", `
import gstplayer as g
assert g.cover_crop(1440, 810, 720, 900) == (396, 396, 0, 0), g.cover_crop(1440, 810, 720, 900)  # 宽片放进竖条：裁左右
assert g.cover_crop(720, 1280, 720, 900) == (0, 0, 190, 190), g.cover_crop(720, 1280, 720, 900)  # 竖片放进矮框：裁上下
assert g.cover_crop(1441, 900, 1440, 900) == (0, 1, 0, 0)  # 奇数像素差：右边多裁一个
assert g.cover_crop(1440, 900, 1440, 900) == (0, 0, 0, 0)
assert g.cover_crop(0, 0, 720, 900) == (0, 0, 0, 0)
assert g.cover_crop(1000, 900, 994, 900) == (2, 4, 0, 0)  # 左偏移取偶数（NV12 色度 2×2 一组）
assert g.cover_crop(1000, 901, 1000, 900) == (0, 0, 0, 1)  # 保留高度取偶数

# kmssink 认定的显示屏像素宽高比（照抄 gst_video_calculate_device_ratio）：现场那台 16:9 屏跑 1440×900，
# kmssink 把 1000×900 的媒体区缩成了 909×900——就是 11/10
assert g.device_par(1440, 900, 527, 296) == (11, 10), g.device_par(1440, 900, 527, 296)
assert g.device_par(1440, 900, 0, 0) == (1, 1)            # EDID 没给尺寸
assert g.device_par(1440, 900, 474, 296) == (1, 1)        # 16:10 的屏跑 16:10：方像素
assert g.device_par(1280, 1024, 527, 296) == (64, 45)     # 16:9 屏跑 5:4
assert g.device_par(1440, 900, 296, 527) == (3, 5)        # 尺寸横竖反了：表里比例的倒数也参与比较
from gi.repository import GstVideo
ok, n, d = GstVideo.video_calculate_display_ratio(800, 720, 11, 10, 11, 10)
assert ok and n * 720 == d * 800, (n, d)  # 帧标上同样的比例后两者相消：显示比例就是像素比例，铺满

# 图层实际位置：从 debugfs 的 DRM 状态里取（格式照内核 drm_atomic_plane_print_state）
import tempfile, os
d = tempfile.mkdtemp()
open(os.path.join(d, "state"), "w").write("""plane[31]: plane-0
	crtc=(null)
	fb=0
	crtc-pos=0x0+0+0
	src-pos=0.000000x0.000000+0.000000+0.000000
plane[33]: plane-1
	crtc=crtc-0
	fb=57
		allocated by = gstplayer.py
		format=NV12 little-endian (0x3231564e)
	crtc-pos=1000x900+0+0
	src-pos=800.000000x720.000000+240.000000+0.000000
	rotation=1
plane[39]: plane-2
	crtc-pos=1440x900+0+0
	src-pos=1440.000000x900.000000+0.000000+0.000000
crtc[51]: crtc-0
""")
pat = os.path.join(d, "state")
assert g.plane_state(33, pat) == ("1000x900+0+0", "800x720+240+0"), g.plane_state(33, pat)
assert g.plane_state(39, pat) == ("1440x900+0+0", "1440x900+0+0"), g.plane_state(39, pat)
assert g.plane_state(99, pat) is None and g.plane_state(33, os.path.join(d, "none")) is None

# 选图层：按 Allwinner DE2（H3）的真实布局——VI 图层排在最前，只有 XRGB/YUV；主图层是第一个 UI 图层。
# 上层必须选主图层（吃 ARGB），下层选 VI 图层；第二个 CRTC（mixer1）上的图层不能选。
NV12, ARGB, XRGB = g.FOURCC_NV12, g.FOURCC_ARGB8888, 0x34325258
de2 = [
    {"id": 33, "type": 0, "formats": {XRGB, NV12}, "crtcs": 1},  # VI，overlay
    {"id": 35, "type": 1, "formats": {ARGB, XRGB}, "crtcs": 1},  # UI0，primary
    {"id": 37, "type": 0, "formats": {ARGB, XRGB}, "crtcs": 1},  # UI1
    {"id": 41, "type": 1, "formats": {ARGB, XRGB, NV12}, "crtcs": 2},  # mixer1
]
assert g.pick_planes(de2) == (35, 33), g.pick_planes(de2)
# 读不到类型（-1）：第一个吃 ARGB 的当上层，吃 NV12 的另一个当下层
untyped = [dict(p, type=-1) for p in de2]
assert g.pick_planes(untyped) == (35, 33), g.pick_planes(untyped)
assert g.pick_planes([]) == (-1, -1)
assert g.pick_planes(de2, crtc_bit=2) == (41, -1)  # 只看所用 CRTC 上的图层

# 视频 caps：只放线性格式的系统内存帧——分块格式（NV12_32L32）会被解码器优先选中而显示不了，DMA_DRM 协商不过
from gi.repository import Gst
Gst.init(None)
vcaps = Gst.Caps.from_string(g.VIDEO_CAPS.split('"')[1].replace("{{", "{").replace("}}", "}"))
assert vcaps.can_intersect(Gst.Caps.from_string("video/x-raw,format=NV12"))
assert not vcaps.can_intersect(Gst.Caps.from_string("video/x-raw,format=NV12_32L32"))
assert not vcaps.can_intersect(Gst.Caps.from_string("video/x-raw(memory:DMABuf),format=DMA_DRM"))

# 上层画面：洞里叠一层半透明黑幕（在叠加图之下）：透明处变成 (0,0,0,a)，压进媒体区的装饰 rgb 不变、
# alpha 按 p + a·(255−p)/255 变；洞外的叠加图原样不动；行宽可能带对齐填充
K, T, D = (9, 9, 9, 255), (0, 0, 0, 0), (5, 5, 5, 128)  # 不透明、透明、半透明装饰（预乘）
base = bytes(K + K + T + D) * 2  # 4×2，洞是右边两列
hole = (2, 0, 2, 2)
def pixels(buf, pitch):
    return [tuple(buf[r * pitch + i:r * pitch + i + 4]) for r in range(2) for i in range(0, 16, 4)]
spans = g.deco_spans(base, 16, hole)
assert spans == [(1, 2), (1, 2)], spans
for pitch in (16, 24):
    buf = bytearray(pitch * 2)
    g.paint(buf, pitch, 4, base, hole, spans, 128, True)
    assert pixels(buf, pitch) == [K, K, (0, 0, 0, 128), (5, 5, 5, 192)] * 2, (pitch, pixels(buf, pitch))
    g.paint(buf, pitch, 4, base, hole, spans, 255, False)  # 全黑：洞里全不透明，装饰颜色还在
    assert pixels(buf, pitch) == [K, K, (0, 0, 0, 255), (5, 5, 5, 255)] * 2, (pitch, pixels(buf, pitch))
    g.paint(buf, pitch, 4, base, hole, spans, 0, False)  # 淡入完：洞里还原叠加图
    assert pixels(buf, pitch) == [K, K, T, D] * 2, (pitch, pixels(buf, pitch))
    g.paint(buf, pitch, 4, bytes(32), hole, [None, None], 0, True)  # 换画面：整幅重画
    assert pixels(buf, pitch) == [(0, 0, 0, 0)] * 8

# 分时段亮度：整幅压一层黑幕。颜色乘 (255−d)/255、不透明度加上去；完全透明的像素保持透明（洞里由 paint 垫黑幕）
dimmed = g.dim_frame(base, 128)
assert g.dim_frame(base, 0) is base
dk, dd = (4, 4, 4, 255), (2, 2, 2, 192)
assert pixels(dimmed, 16) == [dk, dk, T, dd] * 2, pixels(dimmed, 16)
assert g.deco_spans(dimmed, 16, hole) == spans
for pitch in (16, 24):
    buf = bytearray(pitch * 2)
    g.paint(buf, pitch, 4, dimmed, hole, spans, 0, True, 128)  # 正常播放时：洞里透明处是黑幕本身，视频跟着变暗
    assert pixels(buf, pitch) == [dk, dk, (0, 0, 0, 128), dd] * 2, (pitch, pixels(buf, pitch))
    g.paint(buf, pitch, 4, dimmed, hole, spans, 128, False, 128)  # 淡出中：两层黑幕叠加，装饰只叠淡出的那层
    assert pixels(buf, pitch) == [dk, dk, (0, 0, 0, 192), (2, 2, 2, 224)] * 2, (pitch, pixels(buf, pitch))
`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// scriptPath 把内嵌的播放脚本写到临时目录（供直接用 Python 跑）。
func scriptPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gstplayer.py")
	os.WriteFile(p, gstScript, 0o644)
	t.Setenv("PYTHONPATH", dir)
	return p
}

func testMedia(t *testing.T, dir string) (img1, img2, video string) {
	t.Helper()
	write := func(name string, c color.RGBA) string {
		im := image.NewRGBA(image.Rect(0, 0, 320, 200))
		for i := 0; i < len(im.Pix); i += 4 {
			im.Pix[i], im.Pix[i+1], im.Pix[i+2], im.Pix[i+3] = c.R, c.G, c.B, 255
		}
		var buf bytes.Buffer
		jpeg.Encode(&buf, im, nil)
		p := filepath.Join(dir, name)
		os.WriteFile(p, buf.Bytes(), 0o644)
		return p
	}
	img1, img2 = write("a.jpg", color.RGBA{255, 0, 0, 255}), write("b.jpg", color.RGBA{0, 255, 0, 255})
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		video = filepath.Join(dir, "c.mp4")
		if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=640x360:r=30",
			"-t", "1.5", "-pix_fmt", "yuv420p", "-c:v", "libx264", video).CombinedOutput(); err != nil {
			t.Logf("ffmpeg 造不了测试视频，跳过视频部分：%v %s", err, out)
			video = ""
		}
	}
	return
}

// 让解码器白解第一帧（绕开 cedrus 每个会话第一帧解坏的问题）不能改变输出：帧数、时间戳都与不加时一致，
// 第一帧也不能出现两次。
func TestPlayerScriptPrimeDecoder(t *testing.T) {
	py := gstPython(t)
	scriptPath(t)
	_, _, video := testMedia(t, t.TempDir())
	if video == "" {
		t.Skip("没有 ffmpeg，造不了测试视频")
	}
	out, err := exec.Command(py, "-c", `
import sys
import gstplayer as g
from gi.repository import Gst
Gst.init(None)
def run(prime):
    p = Gst.parse_launch(g.VIDEO_OUTPUTS[0][1].format(decoder="avdec_h264", sink="fakesink name=sink signal-handoffs=true sync=false"))
    p.get_by_name("src").set_property("location", sys.argv[1])
    pts = []
    p.get_by_name("sink").connect("handoff", lambda s, b, pad: pts.append(b.pts))
    if prime:
        g.prime_decoder(p)
    p.set_state(Gst.State.PLAYING)
    msg = p.get_bus().timed_pop_filtered(10 * Gst.SECOND, Gst.MessageType.EOS | Gst.MessageType.ERROR)
    p.set_state(Gst.State.NULL)
    assert msg and msg.type == Gst.MessageType.EOS, msg and msg.parse_error()
    return pts
plain, primed = run(False), run(True)
assert plain and primed == plain, (len(plain), len(primed), primed[:3])

# 抵消 kmssink 比例校正的那一级（capssetter）：只改 caps 里的像素宽高比，帧照常流到底、宽高不变
p = Gst.parse_launch(g.VIDEO_OUTPUTS[0][1].format(decoder="avdec_h264",
    sink='capssetter caps="video/x-raw,pixel-aspect-ratio=11/10" ! fakesink name=sink sync=false'))
p.get_by_name("src").set_property("location", sys.argv[1])
p.set_state(Gst.State.PLAYING)
msg = p.get_bus().timed_pop_filtered(10 * Gst.SECOND, Gst.MessageType.EOS | Gst.MessageType.ERROR)
caps = p.get_by_name("sink").get_static_pad("sink").get_current_caps()
p.set_state(Gst.State.NULL)
assert msg and msg.type == Gst.MessageType.EOS, msg and msg.parse_error()
s = caps.to_string()
assert "pixel-aspect-ratio=(fraction)11/10" in s and "format=(string)I420" in s, s
`, video).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// 播放脚本按顺序循环播放：图片按时长、视频播完即切，坏文件跳过，单张图片一直显示。
func TestPlayerScriptSequencing(t *testing.T) {
	py := gstPython(t)
	script := scriptPath(t)
	dir := t.TempDir()
	img1, img2, video := testMedia(t, dir)

	cmd := exec.Command(py, "-u", script)
	cmd.Env = append(os.Environ(), "DISPLAY_PLAYER_SINK=fakesink")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	var mu sync.Mutex
	var events []map[string]any
	replies := map[float64]map[string]any{}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			mu.Lock()
			if id, ok := m["id"].(float64); ok {
				replies[id] = m
			} else {
				events = append(events, m)
			}
			mu.Unlock()
		}
	}()
	send := func(m map[string]any) {
		b, _ := json.Marshal(m)
		stdin.Write(append(b, '\n'))
	}
	played := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, e := range events {
			if e["event"] == "playing" {
				out = append(out, filepath.Base(e["path"].(string)))
			}
		}
		return out
	}

	items := []map[string]any{
		{"path": img1, "type": "image", "duration": 1},
		{"path": filepath.Join(dir, "missing.jpg"), "type": "image", "duration": 1},
		{"path": img2, "type": "image", "duration": 1},
	}
	want := []string{"a.jpg", "b.jpg"}
	if video != "" {
		items = append(items, map[string]any{"path": video, "type": "video"})
		want = append(want, "c.mp4")
	}
	want = append(want, "a.jpg") // 循环回到第一项

	send(map[string]any{"id": 1, "cmd": "config", "width": 1440, "height": 900, "fade": 0.2})
	send(map[string]any{"id": 2, "cmd": "load", "items": items, "overlay": nil})
	testutil.WaitFor(t, 15*time.Second, "按顺序播一轮", func() bool { return len(played()) >= len(want) })
	if got := played()[:len(want)]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("播放顺序 %v，期望 %v\n%s", got, want, stderr.String())
	}
	mu.Lock()
	var failed bool
	for _, e := range events {
		failed = failed || (e["event"] == "error" && e["index"] == 1.0)
	}
	mu.Unlock()
	if !failed {
		t.Fatal("坏文件应报 error 事件并跳过")
	}
	if video != "" {
		send(map[string]any{"id": 3, "cmd": "stats"})
		testutil.WaitFor(t, 3*time.Second, "状态", func() bool { mu.Lock(); defer mu.Unlock(); return replies[3] != nil })
		mu.Lock()
		st := replies[3]
		mu.Unlock()
		if st["hwdec"] == "" {
			t.Fatalf("放过视频后应报告解码方式：%v", st)
		}
	}
	send(map[string]any{"id": 5, "cmd": "brightness", "percent": 30, "media": true})
	testutil.WaitFor(t, 3*time.Second, "亮度", func() bool { mu.Lock(); defer mu.Unlock(); return replies[5] != nil })
	mu.Lock()
	if r := replies[5]; r["error"] != nil {
		t.Fatalf("亮度命令应被接受：%v", r)
	}
	mu.Unlock()

	// 停播媒体区：当前项淡出后停掉，之后不再切换；恢复后接着播
	send(map[string]any{"id": 6, "cmd": "brightness", "percent": 30, "media": false})
	time.Sleep(time.Second) // 淡出（测试里 fade 也是默认的 0.6 秒）
	n0 := len(played())
	time.Sleep(2500 * time.Millisecond)
	if len(played()) != n0 {
		t.Fatalf("停播媒体区期间不应再播放：%v", played()[n0:])
	}
	send(map[string]any{"id": 7, "cmd": "brightness", "percent": 100, "media": true})
	testutil.WaitFor(t, 3*time.Second, "恢复播放", func() bool { return len(played()) > n0 })

	// 换成单张图片：一直显示，不再切换
	send(map[string]any{"id": 4, "cmd": "load", "items": []map[string]any{{"path": img2, "type": "image", "duration": 1}}, "overlay": nil})
	n := len(played())
	testutil.WaitFor(t, 3*time.Second, "切到新画面", func() bool { return len(played()) > n })
	n = len(played())
	time.Sleep(2500 * time.Millisecond)
	if len(played()) != n {
		t.Fatalf("单张图片不应反复切换：%v", played()[n:])
	}

	// 视频应当用首选的输出方式放起来，而不是靠退路（退路会掩盖管线描述写错之类的问题）
	cmd.Process.Kill()
	cmd.Wait()
	if video != "" && !strings.Contains(stderr.String(), "via cover") {
		t.Fatalf("视频没有用 cover 方式播放：\n%s", stderr.String())
	}
}

// cover 裁剪要真的在管线里算出来并设上（gst-python 1.26 改了 caps 结构的取法，曾在这里崩过）。
// 320×200 的图放进 720×900 的竖条：保留宽 160，左右各裁 80。
func TestPlayerScriptAppliesCoverCrop(t *testing.T) {
	py := gstPython(t)
	script := scriptPath(t)
	dir := t.TempDir()
	img, _, _ := testMedia(t, dir)
	overlay := filepath.Join(dir, "ovl.bgra")
	os.WriteFile(overlay, make([]byte, 1440*900*4), 0o644)

	cmd := exec.Command(py, "-u", script)
	cmd.Env = append(os.Environ(), "DISPLAY_PLAYER_SINK=fakesink")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	crops := make(chan map[string]any, 4)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil && m["event"] == "crop" {
				crops <- m
			}
		}
	}()
	for _, m := range []map[string]any{
		{"id": 1, "cmd": "config", "width": 1440, "height": 900, "fade": 0.1},
		{"id": 2, "cmd": "load", "items": []map[string]any{{"path": img, "type": "image", "duration": 5}},
			"overlay": overlay, "media": []int{720, 0, 720, 900}},
	} {
		b, _ := json.Marshal(m)
		stdin.Write(append(b, '\n'))
	}
	select {
	case m := <-crops:
		if !jsonEq(m["size"], []int{320, 200}) || !jsonEq(m["crop"], []int{80, 80, 0, 0}) {
			t.Fatalf("裁剪不对：%v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("没有算出裁剪\n%s", stderr.String())
	}
}

// 暂停时播放进程退出（释放显示屏）且不被守护重启；期间 Load 只记下画面，恢复后重新拉起并播放最新画面。
func TestPauseStopsPlayerAndResumeReloads(t *testing.T) {
	p, logPath := startFake(t, 1440, 900)
	if err := p.Load(Scene{Items: []Item{{Path: "/m/a.jpg", Type: "image", Duration: 5}}}); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, 5*time.Second, "首次下发画面", func() bool { return len(cmds(logPath, "load")) == 1 })

	p.SetPaused(true)
	testutil.WaitFor(t, 5*time.Second, "暂停后播放进程退出", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.proc == nil
	})
	if err := p.Load(Scene{Items: []Item{{Path: "/m/b.jpg", Type: "image", Duration: 5}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // 远超 restartDelay：暂停期间不应被重启
	if n := len(cmds(logPath, "config")); n != 1 {
		t.Fatalf("暂停期间不应重启播放进程，config 次数 %d", n)
	}

	p.SetPaused(false)
	testutil.WaitFor(t, 5*time.Second, "恢复后重新下发画面", func() bool { return len(cmds(logPath, "load")) == 2 })
	loads := cmds(logPath, "load")
	if got := loads[1]["items"].([]any)[0].(map[string]any)["path"]; got != "/m/b.jpg" {
		t.Fatalf("恢复后应播放暂停期间收到的最新画面，得到 %v", got)
	}
}
