package server

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/transcode"
)

// fakeEncoder 模拟转码：把原片拷到目标位置；可设置失败或阻塞。
type fakeEncoder struct {
	mu      sync.Mutex
	fail    error
	block   chan struct{} // 非 nil 时 Video 会等它关闭（或 ctx 取消）
	started chan struct{}
	calls   int
}

func (f *fakeEncoder) Version() string                          { return "fake ffmpeg" }
func (f *fakeEncoder) Duration(context.Context, string) float64 { return 10 }
func (f *fakeEncoder) Video(ctx context.Context, src, dst string, _ transcode.Spec, onProgress func(float64)) error {
	f.mu.Lock()
	f.calls++
	fail, block, started := f.fail, f.block, f.started
	f.mu.Unlock()
	if started != nil {
		close(started)
	}
	onProgress(5) // 50%
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail != nil {
		return fail
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func listMedia(t *testing.T, h http.Handler) []MediaFile {
	t.Helper()
	w := do(t, h, adminReq("GET", "/api/v1/admin/devices/"+testDeviceID+"/media", nil), http.StatusOK)
	var files []MediaFile
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatal(err)
	}
	return files
}

// waitMedia 等待播放列表满足条件（转码在后台进行）。
func waitMedia(t *testing.T, h http.Handler, desc string, cond func([]MediaFile) bool) []MediaFile {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		files := listMedia(t, h)
		if cond(files) {
			return files
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s，当前列表 %+v", desc, files)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func byName(files []MediaFile, name string) (MediaFile, bool) {
	for _, f := range files {
		if f.Name == name {
			return f, true
		}
	}
	return MediaFile{}, false
}

// 装了 ffmpeg 时视频一律转码：上传立即返回，后台转码，完成后自动进播放列表、下发到设备。
// H.265、mkv 也能收——反正都会被转成设备吃得消的 H.264。
func TestVideoUploadIsTranscodedInBackground(t *testing.T) {
	s, h := newAdminTestServer(t)
	enc := &fakeEncoder{block: make(chan struct{}), started: make(chan struct{})}
	s.setEncoder(enc)

	res := parseUpload(t, uploadMedia(t, h, testDeviceID,
		upload{"a.jpg", tinyPNG(t)},
		upload{"hevc.mkv", fakeVideo}, // 任何容器/编码都照收，反正会被转成 H.264
	))
	if strings.Join(res.Accepted, ",") != "a.jpg" {
		t.Fatalf("图片应立即就绪，得到 %v", res.Accepted)
	}
	if strings.Join(res.Transcoding, ",") != "hevc.mp4" || len(res.Rejected) != 0 {
		t.Fatalf("视频应进入转码队列（容器统一为 .mp4），得到 transcoding=%v rejected=%+v",
			res.Transcoding, res.Rejected)
	}

	<-enc.started
	files := waitMedia(t, h, "转码中显示进度", func(fs []MediaFile) bool {
		f, ok := byName(fs, "hevc.mp4")
		return ok && f.Status == jobRunning && f.Progress == 50
	})
	if files[0].Name != "a.jpg" || files[0].Status != mediaReady {
		t.Fatalf("就绪文件应排在转码中的文件之前：%+v", files)
	}
	// 转码中的文件不能下发给设备
	if m := deviceManifest(t, h); len(m.Items) != 1 || m.Items[0].Name != "a.jpg" {
		t.Fatalf("转码完成前设备只应拿到已就绪的文件：%+v", m.Items)
	}
	// 也不能参与排序
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/media", []string{"hevc.mp4", "a.jpg"}), http.StatusBadRequest)

	close(enc.block)
	waitMedia(t, h, "转码完成", func(fs []MediaFile) bool {
		f, ok := byName(fs, "hevc.mp4")
		return ok && f.Status == mediaReady
	})
	m := deviceManifest(t, h)
	if len(m.Items) != 2 || m.Items[1].Name != "hevc.mp4" || m.Items[1].Type != "video" {
		t.Fatalf("转码完成后应追加到播放列表末尾并下发：%+v", m.Items)
	}
	// 原片暂存区要清干净
	if entries, _ := os.ReadDir(filepath.Join(s.incomingDir(), testDeviceID)); len(entries) != 0 {
		t.Fatalf("暂存区应已清空，剩 %d 个文件", len(entries))
	}
}

// 转码失败要让运营方看到原因，并能删掉重传。
func TestVideoTranscodeFailureIsVisibleAndDeletable(t *testing.T) {
	s, h := newAdminTestServer(t)
	s.setEncoder(&fakeEncoder{fail: errors.New("转码失败：Invalid data found when processing input")})

	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"bad.mp4", []byte("不是视频")}))
	files := waitMedia(t, h, "转码失败", func(fs []MediaFile) bool {
		f, ok := byName(fs, "bad.mp4")
		return ok && f.Status == jobFailed
	})
	f, _ := byName(files, "bad.mp4")
	if !strings.Contains(f.Error, "Invalid data") {
		t.Fatalf("失败原因应显示给运营方，得到 %q", f.Error)
	}
	if m := deviceManifest(t, h); len(m.Items) != 1 || m.Layout != nil {
		t.Fatalf("失败的视频不该下发（媒体区空，应是整屏模板图）：%+v", m)
	}
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/bad.mp4", nil), http.StatusOK)
	if _, ok := byName(listMedia(t, h), "bad.mp4"); ok {
		t.Fatal("删除后失败的任务应从列表消失")
	}
}

// 转码进行中删除：ffmpeg 要被取消，产物不能再冒出来。
func TestDeleteWhileTranscodingCancels(t *testing.T) {
	s, h := newAdminTestServer(t)
	enc := &fakeEncoder{block: make(chan struct{}), started: make(chan struct{})}
	s.setEncoder(enc)

	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"long.mp4", fakeVideo}))
	<-enc.started
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/long.mp4", nil), http.StatusOK)
	time.Sleep(100 * time.Millisecond) // 给工作协程收尾的时间
	if _, ok := byName(listMedia(t, h), "long.mp4"); ok {
		t.Fatal("删除后不应再出现在列表里")
	}
	if _, err := os.Stat(filepath.Join(s.deviceMediaDir(testDeviceID), "long.mp4")); !os.IsNotExist(err) {
		t.Fatal("被取消的转码不应留下产物")
	}
}

// 没装 ffmpeg 时后台要知道（会醒目提示），装了要显示版本。
func TestInfoReportsTranscodeCapability(t *testing.T) {
	s, h := newAdminTestServer(t)
	var info serverInfo
	json.Unmarshal(do(t, h, adminReq("GET", "/api/v1/admin/info", nil), http.StatusOK).Body.Bytes(), &info)
	if info.Transcode || info.FFmpegError == "" {
		t.Fatalf("未装 ffmpeg 时应报告不可转码及原因：%+v", info)
	}
	s.setEncoder(&fakeEncoder{})
	info = serverInfo{}
	json.Unmarshal(do(t, h, adminReq("GET", "/api/v1/admin/info", nil), http.StatusOK).Body.Bytes(), &info)
	if !info.Transcode || info.Video.MaxBitrateK <= 0 || info.Limits.VideoMB != maxVideoUploadBytes>>20 {
		t.Fatalf("装了 ffmpeg 时应报告转码参数与上传限制：%+v", info)
	}
}

// 上传的大图在服务端缩到画布以内：设备永远只解 1440×900 的图。
func TestImageUploadIsShrunk(t *testing.T) {
	s, h := newAdminTestServer(t)
	big := image.NewRGBA(image.Rect(0, 0, 3000, 2000))
	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"photo.png", encodePNG(t, big)}))
	f, err := os.Open(filepath.Join(s.deviceMediaDir(testDeviceID), "photo.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 1350 || cfg.Height != 900 {
		t.Fatalf("3000×2000 应缩到 1350×900，得到 %d×%d", cfg.Width, cfg.Height)
	}
}

// 用真实 ffmpeg 走一遍：上传一段高码率 H.265，设备拿到的是 H.264。
func TestRealFFmpegEndToEnd(t *testing.T) {
	enc, err := transcode.Find("")
	if err != nil {
		t.Skipf("ffmpeg 不可用：%v", err)
	}
	s, h := newAdminTestServer(t)
	s.setEncoder(enc)

	src := filepath.Join(t.TempDir(), "hot.mp4")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=1",
		"-c:v", "libx265", "-b:v", "20M", src).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg 缺 libx265：%v %s", err, out)
	}
	data, _ := os.ReadFile(src)
	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"hot.mp4", data}))
	waitMedia(t, h, "真实转码完成", func(fs []MediaFile) bool {
		f, ok := byName(fs, "hot.mp4")
		if ok && f.Status == jobFailed {
			t.Fatalf("转码失败：%s", f.Error)
		}
		return ok && f.Status == mediaReady
	})
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-i", filepath.Join(s.deviceMediaDir(testDeviceID), "hot.mp4")).CombinedOutput()
	if !strings.Contains(string(out), "Video: h264") {
		t.Fatalf("转码产物应当是设备能硬解的 H.264：\n%s", out)
	}
}

// ffmpeg 不可用的原因要能在后台看到（原因在启动时检测一次）。
func TestInfoReportsFFmpegError(t *testing.T) {
	s, h := newAdminTestServer(t)
	s.tools.encErr = "在 PATH 里找不到 ffmpeg（服务进程的 PATH=/usr/bin，运行用户 display）"
	var info struct {
		Transcode bool   `json:"transcode"`
		Err       string `json:"ffmpeg_error"`
	}
	json.Unmarshal(do(t, h, adminReq("GET", "/api/v1/admin/info", nil), http.StatusOK).Body.Bytes(), &info)
	if info.Transcode || !strings.Contains(info.Err, "运行用户 display") {
		t.Fatalf("后台应拿到具体原因：%+v", info)
	}
}

// 后台要拿服务器时间与浏览器比对：离线环境下服务器没有 NTP，时钟漂移会让时段计划按错误时间切换。
func TestInfoReportsServerTime(t *testing.T) {
	s, h := newAdminTestServer(t)
	fixed := time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	var info struct {
		ServerTime int64  `json:"server_time"`
		Timezone   string `json:"timezone"`
	}
	json.Unmarshal(do(t, h, adminReq("GET", "/api/v1/admin/info", nil), http.StatusOK).Body.Bytes(), &info)
	if info.ServerTime != fixed.UnixMilli() || info.Timezone == "" {
		t.Fatalf("应返回服务器时间与时区：%+v", info)
	}
}
