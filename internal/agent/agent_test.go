package agent

import (
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
	testSecret   = "test-secret"
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
		MediaRoot:      mediaRoot,
		DataDir:        t.TempDir(),
		ImageDurationS: 10,
		Devices:        []server.DeviceConfig{{ID: testDeviceID, Secret: testSecret, Name: "客户A"}},
	}
	srv, err := server.New(srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	rr := &rangeRecorder{Handler: srv.Handler()}
	ts := httptest.NewServer(rr)
	t.Cleanup(ts.Close)

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
