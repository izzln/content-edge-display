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

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/server"
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
	hw := HardwareInfo{Hostname: "orangepione", HWSerial: "0123456789abcdef"}

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

func TestConfigRequiresEnrollOrStatic(t *testing.T) {
	c := &Config{ServerURL: "http://x"}
	if err := c.fillDefaults(); err == nil {
		t.Fatal("expected error without enroll_token or device_id/secret")
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
		MediaRoot:      t.TempDir(),
		DataDir:        t.TempDir(),
		ImageDurationS: 10,
		EnrollToken:    "enroll-me",
		AdminToken:     "admin",
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
	if len(statuses) != 1 || statuses[0].ID != a.DeviceID() || !statuses[0].Online || !statuses[0].Registered {
		t.Fatalf("device not online in admin: %+v", statuses)
	}
	if statuses[0].AgentVersion != Version {
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
	m, err := manifest.BuildFromDir(devDir, testDeviceID, 10, manifest.NewHashCache())
	if err != nil {
		t.Fatal(err)
	}
	cmd := manifest.Command{Type: "update", Version: "2.0.0", URL: m.Items[0].URL, SHA256: m.Items[0].SHA256, Size: m.Items[0].Size}

	err = a.handleCommands(ctx, []manifest.Command{cmd})
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
