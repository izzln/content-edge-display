package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/server"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/testutil"
)

func TestDeriveDeviceID(t *testing.T) {
	cases := []struct {
		hw   HardwareInfo
		want string
	}{
		{HardwareInfo{Hostname: "scr-0017"}, "scr-0017"},
		{HardwareInfo{Hostname: "SCR-0017"}, "scr-0017"},
		{HardwareInfo{Hostname: "orangepione", HWSerial: "0123456789ABCDEF"}, "opi-89abcdef"},
		{HardwareInfo{Hostname: "armbian", MAC: "02:42:ac:11:00:02"}, "opi-ac110002"},
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
	dir := t.TempDir()
	id1, err := loadOrCreateIdentity(dir, HardwareInfo{Hostname: "orangepione", HWSerial: "0123456789abcdef"}) // 默认主机名不能当编号
	if err != nil {
		t.Fatal(err)
	}
	if id1.DeviceID != "opi-89abcdef" || len(id1.Secret) < 32 {
		t.Fatalf("unexpected identity: %+v", id1)
	}
	// 再次加载：即使主机名变了也沿用已持久化的编号与密钥
	if id2, err := loadOrCreateIdentity(dir, HardwareInfo{Hostname: "renamed-later"}); err != nil || id2 != id1 {
		t.Fatalf("identity not stable: %+v vs %+v (%v)", id1, id2, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity.json should exist with 0600: %v", err)
	}
}

// 身份文件损坏时不能悄悄换一把新密钥了事：坏文件留作证据，日志里说清楚要去后台接受新密钥。
func TestIdentityCorruptFileIsKeptAndReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	os.WriteFile(path, []byte("{truncated"), 0o600)
	id, err := loadOrCreateIdentity(dir, HardwareInfo{Hostname: "scr-0017"})
	if err != nil || id.DeviceID != "scr-0017" || id.Secret == "" {
		t.Fatalf("应生成新身份：%+v %v", id, err)
	}
	if bad, _ := filepath.Glob(path + ".bad-*"); len(bad) != 1 {
		t.Fatalf("坏文件应另存一份留作现场证据，得到 %v", bad)
	}
}

// 读不了（不是"不存在"）时不能重建：那只会得到一把服务端不认的新密钥。
func TestIdentityUnreadableIsAnError(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "identity.json"), 0o755) // 读它会报 EISDIR
	if _, err := loadOrCreateIdentity(dir, HardwareInfo{Hostname: "scr-0017"}); err == nil {
		t.Fatal("身份文件读不了时应报错，而不是生成新密钥")
	}
}

// 配置在加载时校验：设备只走 HTTPS 并固定证书指纹；没有注册口令永远注册不进来。
func TestConfigValidation(t *testing.T) {
	fp := strings.Repeat("AB:", 31) + "AB"
	for _, c := range []struct {
		cfg Config
		err string
	}{
		{Config{ServerURL: "http://x:9000", TLSFingerprint: fp, EnrollToken: "t"}, "https"},
		{Config{ServerURL: "https://x:9001", EnrollToken: "t"}, "tls_fingerprint"},
		{Config{ServerURL: "https://x:9001", TLSFingerprint: fp}, "enroll_token"},
		{Config{ServerURL: "https://x:9001", TLSFingerprint: fp, EnrollToken: "t", DisplayMode: "1440x900@60"}, "display_mode"},
		{Config{ServerURL: "https://x:9001", TLSFingerprint: fp, EnrollToken: "t", Player: "mpv"}, "player"},
	} {
		if err := c.cfg.fillDefaults(); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%+v: 应报 %s 相关错误，得到 %v", c.cfg, c.err, err)
		}
	}
	c := Config{ServerURL: "https://x:9001/", TLSFingerprint: fp, EnrollToken: "t", DisplayMode: "1440x900"}
	if err := c.fillDefaults(); err != nil || c.TLSFingerprint != strings.Repeat("ab", 32) || c.ServerURL != "https://x:9001" ||
		c.CacheDir != "/var/lib/display-agent" || c.Player != "gst" {
		t.Fatalf("合法配置应规范化并填默认值：%+v %v", c, err)
	}
}

func TestRegisterThenPoll(t *testing.T) {
	e := newEnv(t, false)
	a, ctx := e.a, context.Background()
	// 注册前访问被拒
	if err := a.poll(ctx); !errors.Is(err, errUnknownDevice) {
		t.Fatalf("poll before register should fail with unknown device: %v", err)
	}
	for range 2 { // 注册幂等
		if err := a.register(ctx); err != nil {
			t.Fatalf("register failed: %v", err)
		}
	}
	if err := a.poll(ctx); err != nil {
		t.Fatalf("poll after register failed: %v", err)
	}
	if err := a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	var statuses []server.DeviceStatus
	json.Unmarshal(e.admin(t, "GET", "/api/v1/admin/devices", "").Body.Bytes(), &statuses)
	if len(statuses) != 1 || statuses[0].ID != a.identity.DeviceID || !statuses[0].Online || statuses[0].HW.AgentVersion != Version {
		t.Fatalf("device not online in admin: %+v", statuses)
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

// 端到端：设备丢了身份文件 → 用新密钥注册被拒 → 运营方在后台接受 → 设备重新注册成功。
func TestLostIdentityRecoversAfterAdminAccepts(t *testing.T) {
	e := newEnv(t, true)
	a, ctx := e.a, context.Background()
	os.Remove(filepath.Join(a.cfg.CacheDir, "identity.json")) // 重装系统 / 换卡 / 误删
	if err := a.resolveIdentity(); err != nil {
		t.Fatal(err)
	}
	err := a.register(ctx)
	if !errors.Is(err, errKeyConflict) || !strings.Contains(err.Error(), sign.Fingerprint(a.identity.Secret)) {
		t.Fatalf("应报密钥冲突并给出本机指纹：%v", err)
	}
	if w := e.admin(t, "POST", "/api/v1/admin/devices/"+a.identity.DeviceID+"/rekey", `{"accept":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("接受新密钥失败：%d %s", w.Code, w.Body.String())
	}
	if err := a.register(ctx); err != nil {
		t.Fatalf("接受后设备应能重新注册：%v", err)
	}
	if err := a.poll(ctx); err != nil {
		t.Fatalf("重新注册后应能正常拉取清单：%v", err)
	}
}

// 运营方在后台删了一台正在运行的设备：设备不能一直 401 下去，要自己重新注册回来。
func TestDeletedDeviceReRegistersItself(t *testing.T) {
	// 轮询间隔由服务端规定：调到最短，让跑 Run 主循环的测试不必久等
	e := newEnv(t, false, func(c *server.Config) { c.PollIntervalS = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.a.Run(ctx) }()
	registered := func() bool {
		return strings.Contains(e.admin(t, "GET", "/api/v1/admin/devices", "").Body.String(), `"id":"`)
	}
	testutil.WaitFor(t, 15*time.Second, "首次注册", registered)
	if w := e.admin(t, "DELETE", "/api/v1/admin/devices/"+e.a.identity.DeviceID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("删除设备失败：%d", w.Code)
	}
	testutil.WaitFor(t, 15*time.Second, "被删除后自动重新注册", registered)
	cancel()
	<-done
}

// ---- 程序更新（整包 OTA） ----

// otaEnv 在临时目录模拟 OTA 安装布局（versions/1.0.0 + current），返回已注册的代理环境。
func otaEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t, true)
	root := t.TempDir()
	old := filepath.Join(root, "versions", "1.0.0")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "display-agent"), []byte("old-binary"), 0o755)
	if err := os.Symlink(old, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	e.a.install = installLayout(root)
	return e
}

// offer 把程序包放进设备媒体目录（下载走同一个签名下载器），并作为清单里的待执行更新交给代理。
func (e *testEnv) offer(t *testing.T, version, pkgVersion, updateScript string) manifest.Update {
	t.Helper()
	pkg := testutil.Package(pkgVersion,
		testutil.File{Name: "VERSION", Body: pkgVersion + "\n"},
		testutil.File{Name: "display-agent", Body: "new-binary-" + pkgVersion, Exec: true},
		testutil.File{Name: "update.sh", Body: updateScript, Exec: true},
		testutil.File{Name: "install-agent.sh", Body: "#!/bin/sh\n", Exec: true},
		testutil.File{Name: "check-display.sh", Body: "#!/bin/sh\n", Exec: true})
	name := "pkg-" + version + ".mp4"
	os.WriteFile(filepath.Join(e.devDir, name), pkg, 0o644)
	it := e.mediaItem(t, name)
	u := manifest.Update{Version: version, URL: it.URL, SHA256: it.SHA256, Size: it.Size}
	e.a.setUpdate(&u)
	return u
}

// 整包 OTA：下载校验 → 解到版本目录 → 执行包内 update.sh → 切换 current/previous → 待确认；
// 首个心跳成功后确认，并清掉 current、previous 以外的旧版本。
func TestApplyUpdateSwitchesSymlinks(t *testing.T) {
	e := otaEnv(t)
	a, l, ctx := e.a, e.a.install, context.Background()
	os.MkdirAll(l.versionDir("0.9.0"), 0o755) // 更早的版本：确认后应被清掉

	e.offer(t, "2.0.0", "2.0.0", "#!/bin/sh\nset -e\ntouch \"$1/update-ran\"\n")
	if err := a.applyPendingUpdate(ctx); !errors.Is(err, ErrRestartForUpdate) {
		t.Fatalf("expected ErrRestartForUpdate, got %v (%s)", err, a.updateErr)
	}
	cur, _ := os.Readlink(l.current())
	prev, _ := os.Readlink(l.previous())
	if cur != l.versionDir("2.0.0") || prev != l.versionDir("1.0.0") {
		t.Fatalf("current/previous 应切到新/旧版本目录：%s / %s", cur, prev)
	}
	if fi, err := os.Stat(filepath.Join(cur, "display-agent")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatal("新版本目录里应有可执行的程序")
	}
	if _, err := os.Stat(l.path("update-ran")); err != nil {
		t.Fatal("应执行了包内 update.sh（参数是安装目录）")
	}
	if pv, err := os.ReadFile(l.pendingVerify()); err != nil || strings.TrimSpace(string(pv)) != "0" {
		t.Fatalf("pending-verify 应从 0 开始计数：%v %q", err, pv)
	}

	// 心跳成功 → 确认（删除 pending-verify），只留 current 与 previous
	if err := a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.pendingVerify()); !os.IsNotExist(err) {
		t.Fatal("pending-verify should be cleared after successful heartbeat")
	}
	entries, _ := os.ReadDir(l.path("versions"))
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, ",") != "1.0.0,2.0.0" {
		t.Fatalf("确认后只应保留 current 与 previous：%v", left)
	}
}

// 新版本连续 3 次启动失败：rollback-check.sh 把 current 指回旧版本并记下失败的版本；代理不再自动重试它
// （否则下载、切换、启动失败、回滚周而复始），心跳报原因；撤销后、或换个版本号就恢复正常。
func TestRolledBackVersionIsNotRetried(t *testing.T) {
	e := otaEnv(t)
	a, l, ctx := e.a, e.a.install, context.Background()
	script, _ := os.ReadFile("../../deploy/agent/rollback-check.sh")
	os.WriteFile(l.path("rollback-check.sh"), script, 0o755)
	const countRuns = "#!/bin/sh\necho run >>\"$1/runs\"\n"

	e.offer(t, "2.0.0", "2.0.0", countRuns)
	if err := a.applyPendingUpdate(ctx); !errors.Is(err, ErrRestartForUpdate) {
		t.Fatalf("expected ErrRestartForUpdate, got %v (%s)", err, a.updateErr)
	}
	for range 3 { // 新版本三次都没能心跳确认
		if out, err := exec.Command(l.path("rollback-check.sh")).CombinedOutput(); err != nil {
			t.Fatalf("rollback-check.sh: %v %s", err, out)
		}
	}
	if cur, _ := os.Readlink(l.current()); cur != l.versionDir("1.0.0") {
		t.Fatalf("应回滚到旧版本：%s", cur)
	}

	// 代理重启后清单里还是 2.0.0：不再重试，报原因
	a.update, a.updateErr = nil, ""
	e.offer(t, "2.0.0", "2.0.0", countRuns)
	if err := a.applyPendingUpdate(ctx); err != nil || !strings.Contains(a.updateErr, "已回滚") {
		t.Fatalf("回滚掉的版本应报原因而不重试：%v %q", err, a.updateErr)
	}
	if runs, _ := os.ReadFile(l.path("runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("回滚掉的版本不应再装：%q", runs)
	}

	// 换个版本号重新下发：照常更新，回滚记录清掉
	e.offer(t, "2.0.1", "2.0.1", countRuns)
	if err := a.applyPendingUpdate(ctx); !errors.Is(err, ErrRestartForUpdate) {
		t.Fatalf("新版本号应照常更新：%v (%s)", err, a.updateErr)
	}
	if l.rolledBack() != "" {
		t.Fatal("开始装别的版本后应清掉回滚记录")
	}
}

// update.sh 失败：不切换版本，原因随心跳上报；清单不变（设备收到 304）也要按间隔重试，直到成功或撤销。
func TestFailedUpdateIsReportedAndRetried(t *testing.T) {
	e := otaEnv(t)
	a, l, ctx := e.a, e.a.install, context.Background()
	e.offer(t, "2.0.0", "2.0.0", "#!/bin/sh\necho try >>\"$1/attempts\"\necho 'cannot write unit file' >&2\nexit 3\n")
	attempts := func() int {
		data, _ := os.ReadFile(l.path("attempts"))
		return strings.Count(string(data), "try")
	}

	if err := a.applyPendingUpdate(ctx); err != nil {
		t.Fatalf("失败应记录而不是返回：%v", err)
	}
	if cur, _ := os.Readlink(l.current()); cur != l.versionDir("1.0.0") {
		t.Fatalf("update.sh 失败不能切换版本：%s", cur)
	}
	if !strings.Contains(a.updateErr, "update.sh failed") || !strings.Contains(a.updateErr, "cannot write unit file") {
		t.Fatalf("失败原因应带上 update.sh 的输出：%q", a.updateErr)
	}
	a.applyPendingUpdate(ctx) // 重试间隔未到：不再尝试
	if n := attempts(); n != 1 {
		t.Fatalf("重试间隔内不应重复尝试，尝试了 %d 次", n)
	}
	a.updateFailedAt = time.Now().Add(-updateRetryInterval) // 间隔到了：同一份清单也要再试
	a.applyPendingUpdate(ctx)
	if n := attempts(); n != 2 {
		t.Fatalf("间隔到了应重试，尝试了 %d 次", n)
	}

	// 运营方撤销更新：失败记录随之清掉
	a.setUpdate(nil)
	if a.updateErr != "" {
		t.Fatal("撤销更新后不应再上报失败")
	}
	// 版本号对不上的包不装
	e.offer(t, "4.0.0", "4.0.1", "#!/bin/sh\n")
	a.applyPendingUpdate(ctx)
	if !strings.Contains(a.updateErr, "does not match") {
		t.Fatalf("包内 VERSION 与更新不符应拒装：%q", a.updateErr)
	}
}

// update.sh 最长可跑 2 分钟，而 systemd 看门狗是 90 秒：执行期间必须持续喂狗。
func TestUpdateScriptKeepsFeedingWatchdog(t *testing.T) {
	count := listenNotify(t)
	fastWatchdog(t)
	e := otaEnv(t)
	e.offer(t, "2.0.0", "2.0.0", "#!/bin/sh\nsleep 0.4\n")
	if err := e.a.applyPendingUpdate(context.Background()); !errors.Is(err, ErrRestartForUpdate) {
		t.Fatalf("expected ErrRestartForUpdate, got %v (%s)", err, e.a.updateErr)
	}
	if n := count(); n < 5 {
		t.Fatalf("update.sh 执行期间应持续喂狗，只收到 %d 次", n)
	}
}

// 不是按 OTA 布局安装的（开发机直接运行）：不更新，原因上报到后台。
func TestUpdateRequiresInstallLayout(t *testing.T) {
	e := newEnv(t, true)
	e.a.install = ""
	e.offer(t, "2.0.0", "2.0.0", "#!/bin/sh\n")
	if err := e.a.applyPendingUpdate(context.Background()); err != nil || !strings.Contains(e.a.updateErr, "OTA layout") {
		t.Fatalf("应报告没按 OTA 布局安装：%v %q", err, e.a.updateErr)
	}
}

// 安装根目录从正在运行的程序位置推出：<root>/versions/<版本>/display-agent。
func TestDetectInstallLayout(t *testing.T) {
	if l := detectInstallLayout(); l != "" {
		t.Fatalf("测试程序不在 OTA 布局里，应返回空：%q", l)
	}
}
