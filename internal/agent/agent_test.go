package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/server"
)

const (
	testDeviceID = "dev-001"
	// 注册接口要求密钥不短于 32 字节（真实设备用 32 字节随机数的 hex）
	testSecret = "0123456789abcdef0123456789abcdef"
	testEnroll = "enroll-me"
)

// rangeRecorder 记录媒体请求携带的 Range 头，用于断言续传确实发生。
type rangeRecorder struct {
	http.Handler
	mu     sync.Mutex
	ranges []string
}

func (rr *rangeRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/media/") {
		rr.mu.Lock()
		rr.ranges = append(rr.ranges, r.Header.Get("Range"))
		rr.mu.Unlock()
	}
	rr.Handler.ServeHTTP(w, r)
}

// newTestEnv 启动真实服务端(httptest) + null 播放器的代理，返回两端与媒体目录。
func newTestEnv(t *testing.T) (*Agent, *player.Null, *server.Server, *rangeRecorder, string) {
	t.Helper()
	mediaRoot := t.TempDir()
	srvCfg := &server.Config{
		MediaRoot:   mediaRoot,
		DataDir:     t.TempDir(),
		EnrollToken: testEnroll,
	}
	srv, err := server.New(srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	rr := &rangeRecorder{Handler: srv.Handler()}
	ts := httptest.NewServer(rr)
	t.Cleanup(ts.Close)
	// 设备只有自注册这一条路径，先把测试设备注册进去
	registerTestDevice(t, ts.URL, testDeviceID, testSecret)

	cfg := &Config{
		ServerURL: ts.URL,
		DeviceID:  testDeviceID,
		Secret:    testSecret,
		CacheDir:  t.TempDir(),
		Player:    "null",
	}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatal(err)
	}
	p := player.NewNull()
	a := New(cfg, p)
	if err := os.MkdirAll(a.mediaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	devDir := filepath.Join(mediaRoot, testDeviceID)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return a, p, srv, rr, devDir
}

// registerTestDevice 走真实的注册接口登记一台设备。
func registerTestDevice(t *testing.T, baseURL, id, secret string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"device_id": id, "secret": secret, "enroll_token": testEnroll,
	})
	resp, err := http.Post(baseURL+"/api/v1/device/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("注册测试设备失败: %s", resp.Status)
	}
}

func TestEndToEnd(t *testing.T) {
	a, p, _, _, devDir := newTestEnv(t)
	ctx := context.Background()

	// 1. 空目录：拿到空清单
	changed, err := a.PollOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || a.Version() == "" {
		t.Fatalf("expected initial apply, changed=%v version=%q", changed, a.Version())
	}

	// 2. 运营方放入文件 → 版本变化 → 下载并切换
	os.WriteFile(filepath.Join(devDir, "01_intro.jpg"), []byte("image-content"), 0o644)
	changed, err = a.PollOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected manifest change after adding file")
	}
	if got := p.NowPlaying(); !strings.HasSuffix(got, "_01_intro.jpg") {
		t.Fatalf("player not loaded: now playing %q", got)
	}
	if data, err := os.ReadFile(p.NowPlaying()); err != nil || string(data) != "image-content" {
		t.Fatalf("cached file wrong: %v %q", err, data)
	}

	// 3. 无变化 → 304 → changed=false
	changed, err = a.PollOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected 304/no change")
	}

	// 4. 心跳 → 服务端管理接口可见
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}

	// 5. 内容替换 → 旧缓存被清理
	oldCached := p.NowPlaying()
	os.Remove(filepath.Join(devDir, "01_intro.jpg"))
	os.WriteFile(filepath.Join(devDir, "02_video.mp4"), []byte("video-content"), 0o644)
	changed, err = a.PollOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change after replacing content")
	}
	if _, err := os.Stat(oldCached); !os.IsNotExist(err) {
		t.Fatalf("old cache not cleaned up: %v", err)
	}
	if got := p.NowPlaying(); !strings.HasSuffix(got, "_02_video.mp4") {
		t.Fatalf("unexpected now playing: %q", got)
	}
}

func TestHeartbeatVisibleInAdmin(t *testing.T) {
	a, _, srv, _, _ := newTestEnv(t)
	ctx := context.Background()
	if _, err := a.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/admin/devices", nil))
	var statuses []server.DeviceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].Online {
		t.Fatalf("device not online in admin view: %+v", statuses)
	}
	if statuses[0].Heartbeat.Version != a.Version() {
		t.Fatalf("version mismatch: admin=%q agent=%q", statuses[0].Heartbeat.Version, a.Version())
	}
}

func TestDownloadResume(t *testing.T) {
	a, _, _, rr, devDir := newTestEnv(t)
	ctx := context.Background()

	content := strings.Repeat("0123456789", 1000) // 10KB
	os.WriteFile(filepath.Join(devDir, "big.mp4"), []byte(content), 0o644)

	m, err := manifest.BuildFromDir(devDir, testDeviceID, 10, manifest.NewHashCache())
	if err != nil {
		t.Fatal(err)
	}
	item := m.Items[0]

	// 预置半截 .part，模拟上次下载中断
	dst := a.localPath(item)
	half := len(content) / 2
	if err := os.WriteFile(dst+".part", []byte(content[:half]), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.download(ctx, item, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != content {
		t.Fatalf("resumed file corrupt: err=%v len=%d", err, len(data))
	}

	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.ranges) == 0 || rr.ranges[len(rr.ranges)-1] == "" {
		t.Fatalf("expected Range request, got %v", rr.ranges)
	}
}

func TestDownloadRejectsBadChecksum(t *testing.T) {
	a, _, _, _, devDir := newTestEnv(t)
	ctx := context.Background()

	os.WriteFile(filepath.Join(devDir, "a.jpg"), []byte("real-content"), 0o644)
	m, err := manifest.BuildFromDir(devDir, testDeviceID, 10, manifest.NewHashCache())
	if err != nil {
		t.Fatal(err)
	}
	item := m.Items[0]
	item.SHA256 = strings.Repeat("f", 64) // 篡改期望校验和

	dst := filepath.Join(a.mediaDir(), "ffffffffffff_a.jpg")
	if err := a.download(ctx, item, dst); err == nil {
		t.Fatal("expected checksum error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("corrupt file must not be kept")
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatal("corrupt .part must be removed")
	}
}

func TestRestoreFromLocalCache(t *testing.T) {
	a, p, _, _, devDir := newTestEnv(t)
	ctx := context.Background()

	os.WriteFile(filepath.Join(devDir, "a.jpg"), []byte("cached"), 0o644)
	if _, err := a.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ver := a.Version()

	// 模拟重启：同一缓存目录新建 agent + 新播放器，不联网恢复
	p2 := player.NewNull()
	a2 := New(a.cfg, p2)
	if err := a2.LoadCurrent(); err != nil {
		t.Fatalf("restore from cache failed: %v", err)
	}
	if a2.Version() != ver {
		t.Fatalf("restored version mismatch: %q vs %q", a2.Version(), ver)
	}
	if !strings.HasSuffix(p2.NowPlaying(), "_a.jpg") {
		t.Fatalf("player not restored: %q", p2.NowPlaying())
	}
	_ = p
}

// 模板承载视频：服务端下发 layout，设备端要把叠加图下载下来、解码成 mpv 能用的
// BGRA，并把播放区限制在媒体区——而不是让视频铺满整屏盖掉属性。
func TestLayoutBecomesOverlayScene(t *testing.T) {
	a, p, _, _, devDir := newTestEnv(t)
	ctx := context.Background()

	// 媒体区还没内容：整屏图，没有叠加层
	if _, err := a.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if sc := p.Scene(); sc.Overlay != nil || len(sc.Items) != 1 {
		t.Fatalf("媒体区空时应整屏播放一张模板图：%+v", sc)
	}

	os.WriteFile(filepath.Join(devDir, "clip.mp4"), []byte("video-bytes"), 0o644)
	if _, err := a.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sc := p.Scene()
	if sc.Overlay == nil {
		t.Fatal("清单带 layout 时必须合成叠加层")
	}
	if len(sc.Items) != 1 || !strings.HasSuffix(sc.Items[0].Path, "_clip.mp4") {
		t.Fatalf("播放列表应是媒体文件本身：%+v", sc.Items)
	}
	if sc.CanvasW != 1440 || sc.CanvasH != 900 {
		t.Fatalf("画布尺寸错误：%dx%d", sc.CanvasW, sc.CanvasH)
	}
	if sc.Media != (player.Rect{X: 720, Y: 0, W: 720, H: 900}) {
		t.Fatalf("媒体区应是右半屏，得到 %+v", sc.Media)
	}
	// 叠加层必须是解好的 BGRA 原始像素：w*h*4 字节，mpv 直接 mmap 用
	if sc.Overlay.W != 1440 || sc.Overlay.H != 900 {
		t.Fatalf("叠加层尺寸错误：%dx%d", sc.Overlay.W, sc.Overlay.H)
	}
	fi, err := os.Stat(sc.Overlay.Path)
	if err != nil {
		t.Fatalf("叠加层数据文件不存在：%v", err)
	}
	if want := int64(sc.Overlay.W * sc.Overlay.H * 4); fi.Size() != want {
		t.Fatalf("叠加层数据 %d 字节，期望 %d", fi.Size(), want)
	}
	// 媒体区那块必须是全透明的，否则视频透不出来
	raw, err := os.ReadFile(sc.Overlay.Path)
	if err != nil {
		t.Fatal(err)
	}
	center := ((sc.Media.Y+sc.Media.H/2)*sc.Overlay.W + sc.Media.X + sc.Media.W/2) * 4
	if raw[center+3] != 0 {
		t.Fatalf("媒体区中心的 alpha = %d，应为 0（全透明）", raw[center+3])
	}
	if left := ((450)*sc.Overlay.W + 100) * 4; raw[left+3] != 0xFF {
		t.Fatalf("属性区的 alpha = %d，应为 255（完全不透明）", raw[left+3])
	}

	// 缓存清理不能顺手把叠加图和它的解码产物删掉，否则每次巡检都要重下重解
	data, err := os.ReadFile(a.currentPath())
	if err != nil {
		t.Fatal(err)
	}
	var cur manifest.Manifest
	if err := json.Unmarshal(data, &cur); err != nil {
		t.Fatal(err)
	}
	a.cleanup(&cur)
	for _, path := range []string{sc.Overlay.Path, strings.TrimSuffix(sc.Overlay.Path, overlaySuffix)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("清理时误删了仍被引用的文件 %s：%v", filepath.Base(path), err)
		}
	}

	// 断网重启：从本地缓存恢复时叠加层也要一起恢复
	p2 := player.NewNull()
	a2 := New(a.cfg, p2)
	if err := a2.LoadCurrent(); err != nil {
		t.Fatalf("从缓存恢复失败：%v", err)
	}
	if p2.Scene().Overlay == nil {
		t.Fatal("重启恢复后叠加层丢失，画面会变成整屏视频")
	}
}
