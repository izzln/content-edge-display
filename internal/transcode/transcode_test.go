package transcode

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// needFFmpeg 在没装 ffmpeg 的环境里跳过（服务端没有 ffmpeg 时会退化为只校验不转码，另有测试覆盖）。
func needFFmpeg(t *testing.T) *Encoder {
	t.Helper()
	e, err := Find("")
	if err != nil {
		t.Skipf("ffmpeg 不可用，跳过转码测试：%v", err)
	}
	return e
}

// makeClip 用 ffmpeg 造一段测试视频。
func makeClip(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	full = append(full, out)
	if b, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Skipf("造测试视频失败（ffmpeg 缺编码器？）：%v %s", err, b)
	}
	return out
}

// probe 读出转码产物的编码、尺寸、帧率与音轨情况。
func probe(t *testing.T, path string) string {
	t.Helper()
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-i", path).CombinedOutput()
	return string(out)
}

// 高码率、超分辨率、60fps、带音轨、H.265 的原片 → 1440×900 以内的 H.264、无音轨、码率受控。
func TestVideoNormalizesHotSource(t *testing.T) {
	e := needFFmpeg(t)
	dir := t.TempDir()
	src := makeClip(t, dir, "hot.mp4",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=60:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx265", "-b:v", "20M", "-c:a", "aac", "-shortest")

	if d := e.Duration(context.Background(), src); d < 1.5 || d > 2.5 {
		t.Fatalf("时长解析错误：%v", d)
	}
	var seen float64
	outDir := t.TempDir()
	dst := filepath.Join(outDir, "out.mp4")
	var visible []string
	err := e.Video(context.Background(), src, dst, DefaultSpec(), func(s float64) {
		seen = s
		// 回归：转码进行中，输出目录里不能出现可见的半成品（会被当成就绪内容下发给设备）
		entries, _ := os.ReadDir(outDir)
		for _, en := range entries {
			if !strings.HasPrefix(en.Name(), ".") {
				visible = append(visible, en.Name())
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) > 0 {
		t.Fatalf("转码过程中输出目录出现了可见的半成品：%v", visible)
	}
	info := probe(t, dst)
	if !strings.Contains(info, "Video: h264") {
		t.Fatalf("产物应为 H.264：\n%s", info)
	}
	if !strings.Contains(info, "1440x810") {
		t.Fatalf("1920×1080 应等比缩到 1440×810：\n%s", info)
	}
	if strings.Contains(info, "Audio:") {
		t.Fatalf("产物不应带音轨（屏幕一律静音）：\n%s", info)
	}
	if e.fpsMax && !strings.Contains(info, "30 fps") {
		t.Fatalf("帧率应压到 30fps：\n%s", info)
	}
	if seen <= 0 {
		t.Fatal("应当回调转码进度")
	}
	// 码率：2 秒的片子，按 4Mbps 上限算不该超过 ~1.5MB（留出关键帧与容器开销）
	if fi, _ := os.Stat(dst); fi.Size() > 1500*1024 {
		t.Fatalf("码率没压住：2 秒产物 %d 字节", fi.Size())
	}
	data, _ := os.ReadFile(dst)
	// fastdecode：硬解失效退化成软解时也放得动——关掉 CABAC 与环路滤波（x264 把编码参数写在码流的 SEI 里）
	if !bytes.Contains(data, []byte("cabac=0")) || !bytes.Contains(data, []byte("deblock=0")) {
		t.Fatal("产物应按 fastdecode 编码（cabac=0、deblock=0）")
	}
	// faststart：moov 应在 mdat 之前，设备边下边播、断点续传后都能立即打开
	if m, d := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat")); m < 0 || d < 0 || m > d {
		t.Fatal("产物没有做 faststart（moov 应在 mdat 之前）")
	}
}

// 简单的素材（静态画面、幻灯片式动画）不能被撑到高码率：按画质编码，码率随内容走。
// 回归：曾按固定 2.5Mbps 编码，原片 75kbps 的动画转出来大了十倍。
func TestVideoSimpleSourceStaysSmall(t *testing.T) {
	e := needFFmpeg(t)
	dir := t.TempDir()
	src := makeClip(t, dir, "simple.mp4", "-f", "lavfi", "-i", "testsrc=size=852x480:rate=30:duration=6",
		"-c:v", "libx264", "-b:v", "100k")
	dst := filepath.Join(dir, "out.mp4")
	if err := e.Video(context.Background(), src, dst, DefaultSpec(), nil); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dst)
	if kbps := fi.Size() * 8 / 1000 / 6; kbps > 600 {
		t.Fatalf("简单素材转出来 %dkbps，码率应随内容走（远低于 %dkbps 上限）", kbps, DefaultSpec().MaxBitrateK)
	}
}

// 本来就小的素材不能被放大（放大只会更糊、更费码率）。
func TestVideoDoesNotUpscale(t *testing.T) {
	e := needFFmpeg(t)
	dir := t.TempDir()
	src := makeClip(t, dir, "small.mp4", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=1",
		"-c:v", "libx264")
	dst := filepath.Join(dir, "out.mp4")
	if err := e.Video(context.Background(), src, dst, DefaultSpec(), nil); err != nil {
		t.Fatal(err)
	}
	if info := probe(t, dst); !strings.Contains(info, "640x360") {
		t.Fatalf("小尺寸素材应保持原尺寸：\n%s", info)
	}
}

func TestVideoReportsBrokenInput(t *testing.T) {
	e := needFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "broken.mp4")
	os.WriteFile(src, []byte("这不是视频"), 0o644)
	err := e.Video(context.Background(), src, filepath.Join(dir, "out.mp4"), DefaultSpec(), nil)
	if err == nil || !strings.Contains(err.Error(), "ffmpeg failed") {
		t.Fatalf("坏文件应返回可读的错误，得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.mp4")); !os.IsNotExist(err) {
		t.Fatal("失败时不应留下产物")
	}
}

func TestShrinkImage(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, w, h int, enc func(*os.File, image.Image) error) string {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y += 7 {
			for x := 0; x < w; x++ {
				img.SetRGBA(x, y, color.RGBA{0xFF, 0x80, 0, 0xFF})
			}
		}
		p := filepath.Join(dir, name)
		f, _ := os.Create(p)
		defer f.Close()
		if err := enc(f, img); err != nil {
			t.Fatal(err)
		}
		return p
	}
	size := func(p string) (int, int, string) {
		f, _ := os.Open(p)
		defer f.Close()
		cfg, format, err := image.DecodeConfig(f)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Width, cfg.Height, format
	}
	pngEnc := func(f *os.File, i image.Image) error { return png.Encode(f, i) }
	jpgEnc := func(f *os.File, i image.Image) error { return jpeg.Encode(f, i, nil) }

	big := write("big.jpg", 4000, 3000, jpgEnc)
	if shrunk, err := ShrinkImage(big, 1440, 900); err != nil || !shrunk {
		t.Fatalf("大图应被缩小：%v %v", shrunk, err)
	}
	if w, h, f := size(big); w != 1200 || h != 900 || f != "jpeg" {
		t.Fatalf("4000×3000 应等比缩到 1200×900 且保持 jpeg，得到 %d×%d %s", w, h, f)
	}

	wide := write("wide.png", 3000, 1000, pngEnc)
	ShrinkImage(wide, 1440, 900)
	if w, h, f := size(wide); w != 1440 || h != 480 || f != "png" {
		t.Fatalf("3000×1000 应缩到 1440×480 且保持 png，得到 %d×%d %s", w, h, f)
	}

	small := write("small.png", 800, 600, pngEnc)
	before, _ := os.ReadFile(small)
	if shrunk, _ := ShrinkImage(small, 1440, 900); shrunk {
		t.Fatal("小图不应被改动")
	}
	if after, _ := os.ReadFile(small); !bytes.Equal(before, after) {
		t.Fatal("小图文件内容不应变化")
	}
}

func TestParseDuration(t *testing.T) {
	if d := parseDuration("  Duration: 00:01:23.50, start: 0.000000"); d != 83.5 {
		t.Fatalf("got %v", d)
	}
	if d := parseDuration("no duration here"); d != 0 {
		t.Fatalf("got %v", d)
	}
}

// "明明装了却说找不到"：错误里必须说清楚是哪一种情况，而不是一句"未找到"。
func TestFindReportsWhy(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("PATH", dir)
	if _, err := Find(""); err == nil || !strings.Contains(err.Error(), "PATH="+dir) || !strings.Contains(err.Error(), "user ") {
		t.Fatalf("不在 PATH 里：应给出实际 PATH 与运行用户，得到 %v", err)
	}

	noexec := filepath.Join(dir, "ffmpeg-noexec")
	os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644)
	if _, err := Find(noexec); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("没有执行权限：应说明，得到 %v", err)
	}

	// 找到了但跑不起来（例如 snap 版 ffmpeg 以没有家目录的系统用户运行）
	broken := filepath.Join(dir, "ffmpeg-snap")
	os.WriteFile(broken, []byte("#!/bin/sh\necho 'cannot create user data directory: /nonexistent/snap/ffmpeg: Permission denied' >&2\nexit 1\n"), 0o755)
	if _, err := Find(broken); err == nil || !strings.Contains(err.Error(), "cannot create user data directory") {
		t.Fatalf("运行失败：应带上 ffmpeg 自己的报错，得到 %v", err)
	}
}
