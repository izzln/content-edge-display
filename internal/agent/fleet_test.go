package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/server"
	"github.com/izzln/content-edge-display/internal/sign"
)

func TestDeriveDeviceID(t *testing.T) {
	cases := []struct {
		hw   HardwareInfo
		want string
	}{
		{HardwareInfo{Hostname: "scr-0017"}, "scr-0017"},
		{HardwareInfo{Hostname: "SCR-0017"}, "scr-0017"},
		{HardwareInfo{Hostname: "orangepione", HWSerial: "0123456789ABCDEF"}, "opi-89abcdef"},
		{HardwareInfo{Hostname: "armbian", HWSerial: "", MAC: "02:42:ac:11:00:02"}, "opi-ac110002"},
		{HardwareInfo{Hostname: "bad host name", HWSerial: "0123456789abcdef"}, "opi-89abcdef"},
	}
	for _, c := range cases {
		if got := deriveDeviceID(c.hw); got != c.want {
			t.Errorf("%+v: got %q want %q", c.hw, got, c.want)
		}
	}
	if got := deriveDeviceID(HardwareInfo{Hostname: "localhost"}); !strings.HasPrefix(got, "opi-") || len(got) != 12 {
		t.Errorf("fallback random id malformed: %q", got)
	}
}

func TestIdentityPersistence(t *testing.T) {
	cfg := &Config{CacheDir: t.TempDir()}
	hw := HardwareInfo{Hostname: "orangepione", HWSerial: "0123456789abcdef"} // 默认主机名不能当编号

	id1, err := loadOrCreateIdentity(cfg, hw)
	if err != nil {
		t.Fatal(err)
	}
	if id1.DeviceID != "opi-89abcdef" || len(id1.Secret) != 64 {
		t.Fatalf("unexpected identity: %+v", id1)
	}
	// 再次加载：即使主机名变了也沿用已持久化的编号与密钥
	id2, err := loadOrCreateIdentity(cfg, HardwareInfo{Hostname: "renamed-later"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("identity not stable: %+v vs %+v", id1, id2)
	}
	// 配置显式值覆盖
	cfg.DeviceID = "explicit"
	id3, _ := loadOrCreateIdentity(cfg, hw)
	if id3.DeviceID != "explicit" || id3.Secret != id1.Secret {
		t.Fatalf("explicit device_id should override, secret kept: %+v", id3)
	}
	if fi, err := os.Stat(filepath.Join(cfg.CacheDir, "identity.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity.json should exist with 0600: %v", err)
	}
}

// 设备只能自注册加入，没有 enroll_token 的代理永远不会被服务端认出来——加载配置时就报错。
func TestConfigRequiresEnrollToken(t *testing.T) {
	c := &Config{ServerURL: "http://x", DeviceID: "explicit"}
	if err := c.fillDefaults(); err == nil {
		t.Fatal("expected error without enroll_token")
	}
	c = &Config{ServerURL: "http://x", EnrollToken: "tok"}
	if err := c.fillDefaults(); err != nil {
		t.Fatalf("enroll_token alone should suffice: %v", err)
	}
}

// newEnrollEnv 起一个启用自注册的服务端和一个无预置身份的代理。
func newEnrollEnv(t *testing.T) (*Agent, *server.Server, http.Handler) {
	t.Helper()
	srvCfg := &server.Config{
		MediaRoot:   t.TempDir(),
		DataDir:     t.TempDir(),
		EnrollToken: "enroll-me",
		AdminToken:  "admin",
		// 轮询间隔由服务端规定：调到最短，让跑 Run 主循环的测试不必久等
		PollIntervalS: 1,
	}
	srv, err := server.New(srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	cfg := &Config{ServerURL: ts.URL, EnrollToken: "enroll-me", CacheDir: t.TempDir(), Player: "null"}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatal(err)
	}
	a := New(cfg, player.NewNull())
	os.MkdirAll(a.mediaDir(), 0o755)
	return a, srv, h
}

func TestRegisterThenPoll(t *testing.T) {
	a, _, h := newEnrollEnv(t)
	ctx := context.Background()

	if err := a.ResolveIdentity(); err != nil {
		t.Fatal(err)
	}
	if a.DeviceID() == "" || a.identity.Secret == "" {
		t.Fatal("identity not resolved")
	}
	// 注册前访问被拒
	if _, err := a.PollOnce(ctx); err == nil {
		t.Fatal("poll before register should fail with 401")
	}
	if err := a.Register(ctx); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if err := a.Register(ctx); err != nil {
		t.Fatalf("second register should be idempotent: %v", err)
	}
	if _, err := a.PollOnce(ctx); err != nil {
		t.Fatalf("poll after register failed: %v", err)
	}
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}

	// 管理后台可见且在线
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/admin/devices", nil)
	r.Header.Set("X-Admin-Token", "admin")
	h.ServeHTTP(w, r)
	var statuses []server.DeviceStatus
	json.Unmarshal(w.Body.Bytes(), &statuses)
	if len(statuses) != 1 || statuses[0].ID != a.DeviceID() || !statuses[0].Online {
		t.Fatalf("device not online in admin: %+v", statuses)
	}
	if statuses[0].HW.AgentVersion != Version {
		t.Fatalf("agent version not reported: %+v", statuses[0])
	}
}

// setupInstallLayout 在临时目录模拟 OTA 安装布局，返回根目录。
func setupInstallLayout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	versions := filepath.Join(root, "versions")
	os.MkdirAll(versions, 0o755)
	old := filepath.Join(versions, "display-agent-old")
	os.WriteFile(old, []byte("old-binary"), 0o755)
	if err := os.Symlink(old, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestApplyUpdateSwitchesSymlinks(t *testing.T) {
	a, _, _, _, devDir := newTestEnv(t)
	ctx := context.Background()
	a.cfg.InstallDir = setupInstallLayout(t)

	// 固件下载与媒体下载共用签名下载器：用设备媒体目录里的文件充当"固件"取得 URL/sha。
	content := []byte("brand-new-binary")
	os.WriteFile(filepath.Join(devDir, "fw.mp4"), content, 0o644)
	fw := mediaItem(t, devDir, "fw.mp4")
	cmd := manifest.Command{Type: "update", Version: "2.0.0", URL: fw.URL, SHA256: fw.SHA256, Size: fw.Size}

	err := a.handleCommands(ctx, []manifest.Command{cmd})
	if !errors.Is(err, ErrRestartForUpdate) {
		t.Fatalf("expected ErrRestartForUpdate, got %v", err)
	}
	layout := installLayout{root: a.cfg.InstallDir}
	cur, _ := os.Readlink(layout.current())
	prev, _ := os.Readlink(layout.previous())
	if cur != layout.binary("2.0.0") {
		t.Fatalf("current not switched: %s", cur)
	}
	if !strings.HasSuffix(prev, "display-agent-old") {
		t.Fatalf("previous not set: %s", prev)
	}
	data, _ := os.ReadFile(cur)
	if string(data) != string(content) {
		t.Fatal("new binary content wrong")
	}
	if fi, err := os.Stat(cur); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatal("new binary not executable")
	}
	pv, err := os.ReadFile(layout.pendingVerify())
	if err != nil || !strings.Contains(string(pv), "version=2.0.0") {
		t.Fatalf("pending-verify missing: %v %q", err, pv)
	}

	// 心跳成功 → 提交（删除 pending-verify）
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.pendingVerify()); !os.IsNotExist(err) {
		t.Fatal("pending-verify should be cleared after successful heartbeat")
	}

	// 同版本命令忽略；坏校验和不切换
	if err := a.handleCommands(ctx, []manifest.Command{{Type: "update", Version: Version}}); err != nil {
		t.Fatalf("same-version command should be a no-op: %v", err)
	}
	bad := cmd
	bad.Version = "3.0.0"
	bad.SHA256 = strings.Repeat("0", 64)
	if err := a.handleCommands(ctx, []manifest.Command{bad}); err != nil {
		t.Fatalf("bad checksum should be logged, not returned: %v", err)
	}
	if cur2, _ := os.Readlink(layout.current()); cur2 != cur {
		t.Fatal("current must not change on failed update")
	}
}

func TestApplyUpdateIgnoredWhenNotInstalled(t *testing.T) {
	a, _, _, _, _ := newTestEnv(t)
	a.cfg.InstallDir = "" // 开发环境
	err := a.handleCommands(context.Background(), []manifest.Command{{Type: "update", Version: "9.9.9", URL: "/x", SHA256: "y", Size: 1}})
	if err != nil {
		t.Fatalf("update without install layout should be ignored: %v", err)
	}
}

// 身份文件损坏时不能悄悄换一把新密钥了事：坏文件留作证据，日志里说清楚要去后台接受新密钥。
func TestIdentityCorruptFileIsKeptAndReported(t *testing.T) {
	cfg := &Config{CacheDir: t.TempDir()}
	path := filepath.Join(cfg.CacheDir, "identity.json")
	os.WriteFile(path, []byte("{truncated"), 0o600)
	id, err := loadOrCreateIdentity(cfg, HardwareInfo{Hostname: "scr-0017"})
	if err != nil {
		t.Fatal(err)
	}
	if id.DeviceID != "scr-0017" || len(id.Secret) != 64 {
		t.Fatalf("应生成新身份：%+v", id)
	}
	bad, _ := filepath.Glob(path + ".bad-*")
	if len(bad) != 1 {
		t.Fatalf("坏文件应另存一份留作现场证据，得到 %v", bad)
	}
}

// 读不了（不是"不存在"）时不能重建：那只会得到一把服务端不认的新密钥。
func TestIdentityUnreadableIsAnError(t *testing.T) {
	cfg := &Config{CacheDir: t.TempDir()}
	os.Mkdir(filepath.Join(cfg.CacheDir, "identity.json"), 0o755) // 读它会报 EISDIR
	if _, err := loadOrCreateIdentity(cfg, HardwareInfo{Hostname: "scr-0017"}); err == nil {
		t.Fatal("身份文件读不了时应报错，而不是生成新密钥")
	}
}

// 同一个缓存目录只允许一个代理进程（两个进程首次启动会各生成一把密钥、互相覆盖）。
func TestCacheDirLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockCacheDir(dir); err == nil || !strings.Contains(err.Error(), "另一个 display-agent") {
		t.Fatalf("第二个进程应被拒绝：%v", err)
	}
	unlock()
	unlock2, err := lockCacheDir(dir)
	if err != nil {
		t.Fatalf("释放后应能再次加锁：%v", err)
	}
	unlock2()
}

// 相对路径按配置文件目录解析：否则 systemd（工作目录 /）与手工试跑会用到两份 identity.json。
func TestLoadConfigResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.json")
	os.WriteFile(p, []byte(`{"server_url":"http://x","enroll_token":"t","cache_dir":"cache","mpv_socket":"run/mpv.sock"}`), 0o644)
	t.Chdir(t.TempDir())
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheDir != filepath.Join(dir, "cache") || cfg.MpvSocket != filepath.Join(dir, "run/mpv.sock") {
		t.Fatalf("相对路径应按配置文件目录解析：cache=%s sock=%s", cfg.CacheDir, cfg.MpvSocket)
	}
}

// 端到端：设备丢了身份文件 → 用新密钥注册被拒 → 运营方在后台接受 → 设备重新注册成功。
func TestLostIdentityRecoversAfterAdminAccepts(t *testing.T) {
	a, srv, h := newEnrollEnv(t)
	ctx := context.Background()
	if err := a.ResolveIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(ctx); err != nil {
		t.Fatal(err)
	}
	_ = srv

	// 身份文件丢了（重装系统 / 换卡 / 误删）
	os.Remove(identityPath(a.cfg))
	if err := a.ResolveIdentity(); err != nil {
		t.Fatal(err)
	}
	err := a.Register(ctx)
	if !errors.Is(err, errKeyConflict) || !strings.Contains(err.Error(), sign.Fingerprint(a.identity.Secret)) {
		t.Fatalf("应报密钥冲突并给出本机指纹：%v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/admin/devices/"+a.DeviceID()+"/rekey", strings.NewReader(`{"accept":true}`))
	req.Header.Set("X-Admin-Token", "admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("接受新密钥失败：%d %s", w.Code, w.Body.String())
	}
	if err := a.Register(ctx); err != nil {
		t.Fatalf("接受后设备应能重新注册：%v", err)
	}
	if _, err := a.PollOnce(ctx); err != nil {
		t.Fatalf("重新注册后应能正常拉取清单：%v", err)
	}
}

// 运营方在后台删了一台正在运行的设备：设备不能一直 401 下去，要自己重新注册回来。
func TestDeletedDeviceReRegistersItself(t *testing.T) {
	a, srv, h := newEnrollEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitUntil := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("超时：%s", desc)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	registered := func() bool {
		r := httptest.NewRequest("GET", "/api/v1/admin/devices", nil)
		r.Header.Set("X-Admin-Token", "admin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return strings.Contains(w.Body.String(), `"id":"`)
	}
	waitUntil("首次注册", registered)

	req := httptest.NewRequest("DELETE", "/api/v1/admin/devices/"+a.DeviceID(), nil)
	req.Header.Set("X-Admin-Token", "admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("删除设备失败：%d", w.Code)
	}
	waitUntil("被删除后自动重新注册", registered)
	_ = srv
	cancel()
	<-done
}
