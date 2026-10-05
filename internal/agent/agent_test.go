package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/server"
)

func TestEndToEnd(t *testing.T) {
	e := newEnv(t, true)
	a, p, ctx := e.a, e.p, context.Background()

	// 1. 空目录：拿到整屏模板图
	if err := a.poll(ctx); err != nil || a.manifestVer == "" {
		t.Fatalf("expected initial apply: %v %q", err, a.manifestVer)
	}

	// 2. 运营方放入文件 → 版本变化 → 下载并切换
	os.WriteFile(filepath.Join(e.devDir, "01_intro.jpg"), []byte("image-content"), 0o644)
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := nowPlaying(p); !strings.HasSuffix(got, "_01_intro.jpg") {
		t.Fatalf("player not loaded: now playing %q", got)
	}
	if data, err := os.ReadFile(nowPlaying(p)); err != nil || string(data) != "image-content" {
		t.Fatalf("cached file wrong: %v %q", err, data)
	}

	// 3. 无变化 → 304，版本不变
	ver := a.manifestVer
	if err := a.poll(ctx); err != nil || a.manifestVer != ver {
		t.Fatalf("expected 304/no change: %v", err)
	}

	// 4. 心跳 → 服务端管理接口可见
	if err := a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	var statuses []server.DeviceView
	json.Unmarshal(e.admin(t, "GET", "/api/v1/admin/devices", "").Body.Bytes(), &statuses)
	if len(statuses) != 1 || !statuses[0].Online || statuses[0].Heartbeat == nil || statuses[0].Heartbeat.AgentVersion != Version {
		t.Fatalf("device not online with heartbeat in admin view: %+v", statuses)
	}
	if statuses[0].IP == "" {
		t.Fatal("服务端应从连接上取到设备 IP")
	}

	// 5. 内容替换 → 旧缓存被清理
	oldCached := nowPlaying(p)
	os.Remove(filepath.Join(e.devDir, "01_intro.jpg"))
	os.WriteFile(filepath.Join(e.devDir, "02_video.mp4"), []byte("video-content"), 0o644)
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldCached); !os.IsNotExist(err) {
		t.Fatalf("old cache not cleaned up: %v", err)
	}
	if got := nowPlaying(p); !strings.HasSuffix(got, "_02_video.mp4") {
		t.Fatalf("unexpected now playing: %q", got)
	}
}

func TestDownloadResume(t *testing.T) {
	e := newEnv(t, true)
	content := strings.Repeat("0123456789", 1000) // 10KB
	os.WriteFile(filepath.Join(e.devDir, "big.mp4"), []byte(content), 0o644)
	item := e.mediaItem(t, "big.mp4")

	// 预置半截 .part，模拟上次下载中断
	dst := e.a.localPath(item)
	os.WriteFile(dst+".part", []byte(content[:len(content)/2]), 0o644)
	if err := e.a.downloadFile(context.Background(), item.URL, item.SHA256, item.Size, dst); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != content {
		t.Fatalf("resumed file corrupt: err=%v len=%d", err, len(data))
	}
	e.ranges.mu.Lock()
	defer e.ranges.mu.Unlock()
	if len(e.ranges.ranges) == 0 || e.ranges.ranges[len(e.ranges.ranges)-1] == "" {
		t.Fatalf("expected Range request, got %v", e.ranges.ranges)
	}
}

// 下载完、改名前断电：.part 已经是完整文件。不能再去请求 bytes=<大小>-（服务端回 416），直接校验改名。
func TestDownloadCompletesFullPart(t *testing.T) {
	e := newEnv(t, true)
	os.WriteFile(filepath.Join(e.devDir, "a.jpg"), []byte("whole file"), 0o644)
	item := e.mediaItem(t, "a.jpg")
	dst := e.a.localPath(item)
	os.WriteFile(dst+".part", []byte("whole file"), 0o644)
	if err := e.a.downloadFile(context.Background(), item.URL, item.SHA256, item.Size, dst); err != nil {
		t.Fatalf("完整的 .part 应直接完成：%v", err)
	}
	if len(e.ranges.ranges) != 0 {
		t.Fatalf("不应再发请求：%v", e.ranges.ranges)
	}
}

func TestDownloadRejectsBadChecksum(t *testing.T) {
	e := newEnv(t, true)
	os.WriteFile(filepath.Join(e.devDir, "a.jpg"), []byte("real-content"), 0o644)
	item := e.mediaItem(t, "a.jpg")
	dst := filepath.Join(e.a.mediaDir(), "ffffffffffff_a.jpg")
	if err := e.a.downloadFile(context.Background(), item.URL, strings.Repeat("f", 64), item.Size, dst); err == nil {
		t.Fatal("expected checksum error")
	}
	for _, p := range []string{dst, dst + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("corrupt download must not be kept: %s", p)
		}
	}
}

func TestRestoreFromLocalCache(t *testing.T) {
	e := newEnv(t, true)
	os.WriteFile(filepath.Join(e.devDir, "a.jpg"), []byte("cached"), 0o644)
	if err := e.a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 模拟重启：同一缓存目录新建 agent + 新播放器，不联网恢复
	p2 := player.NewNull()
	a2 := New(e.a.cfg, p2)
	if err := a2.loadCurrent(); err != nil {
		t.Fatalf("restore from cache failed: %v", err)
	}
	if a2.manifestVer != e.a.manifestVer || !strings.HasSuffix(nowPlaying(p2), "_a.jpg") {
		t.Fatalf("player not restored: %q %q", a2.manifestVer, nowPlaying(p2))
	}
}

// 模板承载视频：服务端下发 layout，设备端要把叠加图下载下来，并把播放区限制在媒体区——
// 而不是让视频铺满整屏盖掉属性。
func TestLayoutBecomesOverlayScene(t *testing.T) {
	e := newEnv(t, true)
	a, p, ctx := e.a, e.p, context.Background()

	// 媒体区还没内容：整屏图，没有叠加层
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if sc := p.Scene(); sc.OverlayPNG != "" || len(sc.Items) != 1 {
		t.Fatalf("媒体区空时应整屏播放一张模板图：%+v", sc)
	}

	os.WriteFile(filepath.Join(e.devDir, "clip.mp4"), []byte("video-bytes"), 0o644)
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	sc := p.Scene()
	if sc.OverlayPNG == "" || len(sc.Items) != 1 || !strings.HasSuffix(sc.Items[0].Path, "_clip.mp4") {
		t.Fatalf("清单带 layout 时应是叠加层 + 媒体文件本身：%+v", sc)
	}
	if sc.CanvasW != 1440 || sc.CanvasH != 900 || sc.Media != (manifest.Rect{X: 720, Y: 0, W: 720, H: 900}) {
		t.Fatalf("画布或媒体区错误：%+v", sc)
	}
	if fi, err := os.Stat(sc.OverlayPNG); err != nil || fi.Size() == 0 {
		t.Fatalf("叠加图没有下载下来：%v", err)
	}

	// 缓存清理不能顺手把叠加图和它的光栅化产物删掉；不再被引用的派生文件要清掉
	derived := sc.OverlayPNG + ".1920x1080.bgra"
	stale := filepath.Join(a.mediaDir(), "deadbeef_old.png.1440x900.bgra")
	os.WriteFile(derived, []byte("raw"), 0o644)
	os.WriteFile(stale, []byte("raw"), 0o644)
	var cur manifest.Manifest
	data, _ := os.ReadFile(a.currentPath())
	json.Unmarshal(data, &cur)
	a.cleanup(&cur)
	for _, path := range []string{sc.OverlayPNG, derived} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("清理时误删了仍被引用的文件 %s：%v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("不再被引用的派生文件应当被清理")
	}

	// 断网重启：从本地缓存恢复时叠加层也要一起恢复
	p2 := player.NewNull()
	if err := New(a.cfg, p2).loadCurrent(); err != nil || p2.Scene().OverlayPNG == "" {
		t.Fatalf("重启恢复后叠加层丢失：%v", err)
	}
}

// 设备只能经 HTTPS 访问服务端（HTTP 端口只有装机入口）。
func TestRequestsUseHTTPS(t *testing.T) {
	e := newEnv(t, true)
	if !strings.HasPrefix(e.a.cfg.ServerURL, "https://") {
		t.Fatal("测试环境应走 HTTPS")
	}
	req, _ := e.a.newRequest(context.Background(), http.MethodGet, "/api/v1/device/manifest", nil)
	resp, err := e.a.api.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil {
		t.Fatal("应是 TLS 连接")
	}
}
