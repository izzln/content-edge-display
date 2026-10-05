package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/server"
)

// 各测试共用：真实服务端（HTTPS，设备按指纹固定证书）+ null 播放器的代理。

const (
	testEnroll = "enroll-me"
	testAdmin  = "admin"
)

type testEnv struct {
	a      *Agent
	p      *player.Null
	srv    *server.Server
	h      http.Handler // 服务端路由（管理接口直接调）
	ranges *rangeRecorder
	devDir string // 这台设备在服务端的媒体目录
}

// newEnv 起服务端与代理并解析设备身份；register 为真时顺带走一遍真实的注册接口。
func newEnv(t *testing.T, register bool, tweak ...func(*server.Config)) *testEnv {
	t.Helper()
	scfg := &server.Config{MediaRoot: t.TempDir(), DataDir: t.TempDir(), AdminToken: testAdmin, EnrollToken: testEnroll}
	for _, f := range tweak {
		f(scfg)
	}
	srv, err := server.New(scfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	rr := &rangeRecorder{Handler: srv.Handler()}
	ts := httptest.NewUnstartedServer(rr)
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	cfg := &Config{ServerURL: ts.URL, TLSFingerprint: srv.CertFingerprint(), EnrollToken: testEnroll, CacheDir: t.TempDir(), Player: "null"}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatal(err)
	}
	p := player.NewNull()
	a := New(cfg, p)
	// 测试绝不能改到本机的 root 密码与 SSH 配置
	accDir := t.TempDir()
	a.acc = accessTarget{authorizedKeys: filepath.Join(accDir, "authorized_keys"),
		sshdDropIn: filepath.Join(accDir, "sshd_config.d", "00-display-agent.conf"),
		run:        func(string, string, ...string) error { return nil }}
	if err := os.MkdirAll(a.mediaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.resolveIdentity(); err != nil {
		t.Fatal(err)
	}
	if register {
		if err := a.register(context.Background()); err != nil {
			t.Fatal(err)
		}
		a.registered = true
	}
	devDir := filepath.Join(scfg.MediaRoot, a.identity.DeviceID)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &testEnv{a: a, p: p, srv: srv, h: srv.Handler(), ranges: rr, devDir: devDir}
}

// admin 以管理口令调一次服务端管理接口。
func (e *testEnv) admin(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Admin-Token", testAdmin)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// mediaItem 为设备媒体目录里的一个文件构建清单条目。
func (e *testEnv) mediaItem(t *testing.T, name string) manifest.Item {
	t.Helper()
	items, err := manifest.BuildItems(e.devDir, e.a.identity.DeviceID, []string{name}, 10, manifest.NewHashCache())
	if err != nil || len(items) != 1 {
		t.Fatalf("build item %s: %v %+v", name, err, items)
	}
	return items[0]
}

// nowPlaying 返回 null 播放器当前画面的第一个条目路径。
func nowPlaying(p *player.Null) string {
	if items := p.Scene().Items; len(items) > 0 {
		return items[0].Path
	}
	return ""
}

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

// listenNotify 起一个假的 systemd NOTIFY_SOCKET，统计收到的 WATCHDOG=1。
func listenNotify(t *testing.T) func() int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "n.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	var mu sync.Mutex
	n := 0
	go func() {
		buf := make([]byte, 64)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			if string(buf[:k]) == "WATCHDOG=1" {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}
	}()
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// fastWatchdog 把喂狗间隔调短（测试结束恢复）。
func fastWatchdog(t *testing.T) {
	old := watchdogInterval
	watchdogInterval = 20 * time.Millisecond
	t.Cleanup(func() { watchdogInterval = old })
}
