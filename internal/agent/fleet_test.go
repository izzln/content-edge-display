package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	c = &Config{ServerURL: "https://x:9001", EnrollToken: "tok"}
	if err := c.fillDefaults(); err == nil || !strings.Contains(err.Error(), "tls_fingerprint") {
		t.Fatalf("https 地址没配证书指纹应报错：%v", err)
	}
	c = &Config{ServerURL: "https://x:9001", EnrollToken: "tok", TLSFingerprint: strings.Repeat("AB:", 31) + "AB"}
	if err := c.fillDefaults(); err != nil || c.TLSFingerprint != strings.Repeat("ab", 32) {
		t.Fatalf("带冒号、大写的指纹应被规范化：%q %v", c.TLSFingerprint, err)
	}
}

// 真实 HTTPS：服务端用自己生成的证书，设备按指纹固定，注册、拉清单都走加密连接。
func TestRegisterOverPinnedHTTPS(t *testing.T) {
	srv, err := server.New(&server.Config{MediaRoot: t.TempDir(), DataDir: t.TempDir(), EnrollToken: "enroll-me"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	cfg := &Config{ServerURL: ts.URL, EnrollToken: "enroll-me", CacheDir: t.TempDir(), Player: "null",
		TLSFingerprint: srv.CertFingerprint()}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatal(err)
	}
	a := New(cfg, player.NewNull())
	os.MkdirAll(a.mediaDir(), 0o755)
	if err := a.ResolveIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(context.Background()); err != nil {
		t.Fatalf("HTTPS 注册：%v", err)
	}
	if _, err := a.PollOnce(context.Background()); err != nil {
		t.Fatalf("HTTPS 轮询：%v", err)
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
	old := filepath.Join(root, "versions", "1.0.0")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "display-agent"), []byte("old-binary"), 0o755)
	if err := os.Symlink(old, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return root
}

// testPackage 造一个设备程序包（与 make package 同样的结构），update.sh 的内容由调用方给。
func testPackage(t *testing.T, version, updateScript string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{
		{"VERSION", version + "\n", 0o644},
		{"display-agent", "new-binary-" + version, 0o755},
		{"update.sh", updateScript, 0o755},
		{"check-display.sh", "#!/bin/sh\n", 0o755},
	} {
		tw.WriteHeader(&tar.Header{Name: "display-agent-" + version + "/" + f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// updateCommandFor 把整包放进设备媒体目录充当"固件"（固件与媒体共用签名下载器），返回对应的更新指令。
func updateCommandFor(t *testing.T, devDir, version string, pkg []byte) manifest.Command {
	t.Helper()
	name := "fw-" + version + ".mp4"
	os.WriteFile(filepath.Join(devDir, name), pkg, 0o644)
	fw := mediaItem(t, devDir, name)
	return manifest.Command{Type: "update", Version: version, URL: fw.URL, SHA256: fw.SHA256, Size: fw.Size}
}

// 整包 OTA：下载校验 → 解到版本目录 → 执行包内 update.sh → 切换 current/previous → 待确认；
// 首个心跳成功后确认，并清掉 current、previous 以外的旧版本。
func TestApplyUpdateSwitchesSymlinks(t *testing.T) {
	a, _, _, _, devDir := newTestEnv(t)
	ctx := context.Background()
	a.cfg.InstallDir = setupInstallLayout(t)
	layout := installLayout{root: a.cfg.InstallDir}
	os.MkdirAll(layout.versionDir("0.9.0"), 0o755) // 更早的版本：确认后应被清掉

	script := "#!/bin/sh\nset -e\necho \"installing into $1\"\ntouch \"$1/update-ran\"\n"
	cmd := updateCommandFor(t, devDir, "2.0.0", testPackage(t, "2.0.0", script))
	if err := a.handleCommands(ctx, []manifest.Command{cmd}); !errors.Is(err, ErrRestartForUpdate) {
		t.Fatalf("expected ErrRestartForUpdate, got %v", err)
	}
	cur, _ := os.Readlink(layout.current())
	prev, _ := os.Readlink(layout.previous())
	if cur != layout.versionDir("2.0.0") || prev != layout.versionDir("1.0.0") {
		t.Fatalf("current/previous 应切到新/旧版本目录：%s / %s", cur, prev)
	}
	if data, _ := os.ReadFile(filepath.Join(cur, "display-agent")); string(data) != "new-binary-2.0.0" {
		t.Fatal("新版本目录里的程序不对")
	}
	if fi, err := os.Stat(filepath.Join(cur, "display-agent")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatal("程序应可执行")
	}
	if _, err := os.Stat(filepath.Join(layout.root, "update-ran")); err != nil {
		t.Fatal("应执行了包内 update.sh（参数是安装目录）")
	}
	if pv, err := os.ReadFile(layout.pendingVerify()); err != nil || !strings.Contains(string(pv), "version=2.0.0") {
		t.Fatalf("pending-verify missing: %v %q", err, pv)
	}

	// 心跳成功 → 确认（删除 pending-verify），只留 current 与 previous
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.pendingVerify()); !os.IsNotExist(err) {
		t.Fatal("pending-verify should be cleared after successful heartbeat")
	}
	entries, _ := os.ReadDir(layout.versionsDir())
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, ",") != "1.0.0,2.0.0" {
		t.Fatalf("确认后只应保留 current 与 previous：%v", left)
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

// update.sh 失败：不切换版本，原因随心跳上报。
func TestApplyUpdateScriptFailureIsReported(t *testing.T) {
	a, _, _, _, devDir := newTestEnv(t)
	ctx := context.Background()
	a.cfg.InstallDir = setupInstallLayout(t)
	layout := installLayout{root: a.cfg.InstallDir}

	cmd := updateCommandFor(t, devDir, "2.0.0", testPackage(t, "2.0.0", "#!/bin/sh\necho 'cannot write unit file' >&2\nexit 3\n"))
	if err := a.handleCommands(ctx, []manifest.Command{cmd}); err != nil {
		t.Fatalf("失败应记录而不是返回：%v", err)
	}
	if cur, _ := os.Readlink(layout.current()); cur != layout.versionDir("1.0.0") {
		t.Fatalf("update.sh 失败不能切换版本：%s", cur)
	}
	if !strings.Contains(a.updateErr, "update.sh failed") || !strings.Contains(a.updateErr, "cannot write unit file") {
		t.Fatalf("失败原因应带上 update.sh 的输出：%q", a.updateErr)
	}
	// 版本号对不上的包也不装
	cmd = updateCommandFor(t, devDir, "4.0.0", testPackage(t, "4.0.1", "#!/bin/sh\n"))
	a.handleCommands(ctx, []manifest.Command{cmd})
	if !strings.Contains(a.updateErr, "does not match") {
		t.Fatalf("包内 VERSION 与指令不符应拒装：%q", a.updateErr)
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
	if _, err := lockCacheDir(dir); err == nil || !strings.Contains(err.Error(), "another display-agent") {
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
	os.WriteFile(p, []byte(`{"server_url":"http://x","enroll_token":"t","cache_dir":"cache","install_dir":"ota"}`), 0o644)
	t.Chdir(t.TempDir())
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheDir != filepath.Join(dir, "cache") || cfg.InstallDir != filepath.Join(dir, "ota") {
		t.Fatalf("相对路径应按配置文件目录解析：cache=%s install=%s", cfg.CacheDir, cfg.InstallDir)
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
