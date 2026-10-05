package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/testutil"
)

func registerBody(id, secret, token string) map[string]string {
	return map[string]string{
		"device_id": id, "secret": secret, "enroll_token": token,
		"hostname": "scr-0017", "hw_serial": "0123456789abcdef", "agent_version": "1.0.0",
	}
}

func TestRegisterFlow(t *testing.T) {
	_, h := newAdminTestServer(t)
	secret := strings.Repeat("ab", 32)

	// 错误 token
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", secret, "wrong")), http.StatusUnauthorized)
	// 新设备 → 201
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", secret, enrollToken)), http.StatusCreated)
	// 幂等 → 200
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", secret, enrollToken)), http.StatusOK)
	// 同 id 不同密钥 → 409
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", strings.Repeat("cd", 32), enrollToken)), http.StatusConflict)
	// 非法 id / 短密钥
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("bad id", secret, enrollToken)), http.StatusBadRequest)
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0018", "short", enrollToken)), http.StatusBadRequest)

	// 注册后即可用签名访问 manifest
	do(t, h, signedAs("scr-0017", secret, "GET", "/api/v1/device/manifest"), http.StatusOK)
	do(t, h, signedAs("scr-0017", "wrong", "GET", "/api/v1/device/manifest"), http.StatusUnauthorized)

	// 管理列表可见且标记为自注册；含硬件信息、不含密钥
	w := do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
	var statuses []DeviceStatus
	json.Unmarshal(w.Body.Bytes(), &statuses)
	var found *DeviceStatus
	for i := range statuses {
		if statuses[i].ID == "scr-0017" {
			found = &statuses[i]
		}
	}
	if found == nil || found.HW.Hostname != "scr-0017" || found.HW.Secret != "" || found.HW.IP == "" {
		t.Fatalf("registered device not listed properly: %+v", found)
	}

	// 删除、删除后可重新注册
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/scr-0017", nil), http.StatusNoContent)
	do(t, h, signedAs("scr-0017", secret, "GET", "/api/v1/device/manifest"), http.StatusUnauthorized)
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", strings.Repeat("cd", 32), enrollToken)), http.StatusCreated)

	// 设备只有自注册这一种，任何设备都能删除
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID, nil), http.StatusNoContent)

	// 改名功能已移除：设备名由设备自己上报，后台不再提供改名入口
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/scr-0017/name", map[string]string{"name": "3楼大堂"}), http.StatusNotFound)
}

// testAgentVersion 是测试用代理二进制里注入的版本号。
const testAgentVersion = "9.9.9"

// buildTestAgent 构建一个 linux/arm 的真实代理二进制（注入 testAgentVersion）。
// 上传接口会校验构建信息，所以固件测试必须用真二进制而非任意字节。
// 只构建一次，结果在内存里复用。
var buildTestAgent = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "agentbin")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "display-agent-armv7")
	cmd := exec.Command("go", "build",
		"-ldflags", "-X github.com/izzln/content-edge-display/internal/agent.Version="+testAgentVersion,
		"-o", out, "github.com/izzln/content-edge-display/cmd/display-agent")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm", "GOARM=7", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go build: %v: %s", err, b)
	}
	return os.ReadFile(out)
})

func agentBinaryFixture(t *testing.T) []byte {
	t.Helper()
	b, err := buildTestAgent()
	if err != nil {
		t.Skipf("无法交叉编译 ARM 测试二进制，跳过：%v", err)
	}
	return b
}

// agentPackage 现造一个设备程序包（与 make package 同样的结构）：VERSION + 程序 +（可选）update.sh 与 install-agent.sh。
func agentPackage(t *testing.T, version string, bin []byte, withUpdate bool) []byte {
	files := []testutil.File{{Name: "VERSION", Body: version + "\n"}, {Name: "display-agent", Body: string(bin), Exec: true}}
	if withUpdate {
		files = append(files, testutil.File{Name: "update.sh", Body: "#!/bin/sh\nexit 0\n", Exec: true},
			testutil.File{Name: "install-agent.sh", Body: "#!/bin/sh\n", Exec: true})
	}
	return testutil.Package(version, files...)
}

// uploadPackageRaw 发起一次程序包上传，不对状态码做断言。
func uploadPackageRaw(t *testing.T, h http.Handler, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("notes", "test build")
	fw, _ := mw.CreateFormFile("file", "display-agent.tar.gz")
	fw.Write(content)
	mw.Close()
	r := httptest.NewRequest("POST", "/api/v1/admin/packages", &buf)
	r.Header.Set("X-Admin-Token", adminToken)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func uploadPackage(t *testing.T, h http.Handler, content []byte) store.Package {
	t.Helper()
	w := uploadPackageRaw(t, h, content)
	if w.Code != http.StatusOK {
		t.Fatalf("上传程序包失败: %d %s", w.Code, w.Body.String())
	}
	var meta store.Package
	json.Unmarshal(w.Body.Bytes(), &meta)
	return meta
}

// 上传接口必须挡住"传错文件"——这个包会分发到所有屏，错了要等三次启动失败才回滚。
func TestPackageUploadRejectsWrongFile(t *testing.T) {
	_, h := newAdminTestServer(t)
	bin := agentBinaryFixture(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	native, err := os.ReadFile(self) // 测试程序本身就是一个本机架构的 Go 程序
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		desc string
		pkg  []byte
		want string
	}{
		{"误传裸程序而不是整包", bin, "不是有效的设备程序包"},
		{"包里是本机架构的程序", agentPackage(t, testAgentVersion, native, true), "目标平台"},
		{"VERSION 与程序内置版本不一致", agentPackage(t, "1.0.0", bin, true), "内置版本"},
		{"缺 update.sh", agentPackage(t, testAgentVersion, bin, false), "update.sh"},
	} {
		if w := uploadPackageRaw(t, h, c.pkg); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s：应被拒绝并提示 %q，得到 %d %s", c.desc, c.want, w.Code, w.Body.String())
		}
	}

	pkg := agentPackage(t, testAgentVersion, bin, true)
	fw := uploadPackage(t, h, pkg)
	if fw.Version != testAgentVersion || fw.Size != int64(len(pkg)) {
		t.Fatalf("正确的整包应当上传成功，版本从包里读：%+v", fw)
	}
	// 被拒绝的上传都不得落库，列表里只应有刚才那一个版本
	if w := do(t, h, adminReq("GET", "/api/v1/admin/packages", nil), http.StatusOK); strings.Count(w.Body.String(), `"version"`) != 1 {
		t.Fatalf("程序包列表应只有一个版本：%s", w.Body.String())
	}
}

func heartbeatAs(t *testing.T, h http.Handler, agentVer string) {
	t.Helper()
	body := strings.NewReader(`{"uptime":1,"disk_free_mb":1,"agent_version":"` + agentVer + `"}`)
	r := signedRequest("POST", "/api/v1/device/heartbeat", body)
	r.Header.Set("Content-Type", "application/json")
	do(t, h, r, http.StatusNoContent)
}

func TestRolloutAndUpdate(t *testing.T) {
	s, h := newAdminTestServer(t)
	pkg := agentPackage(t, testAgentVersion, agentBinaryFixture(t), true)
	heartbeatAs(t, h, "1.0.0")

	fw := uploadPackage(t, h, pkg)
	if fw.Size != int64(len(pkg)) || len(fw.SHA256) != 64 {
		t.Fatalf("package meta wrong: %+v", fw)
	}

	base := deviceManifest(t, h)
	if base.Update != nil {
		t.Fatalf("no rollout yet, expected no update: %+v", base.Update)
	}

	// 立即下发全部设备
	do(t, h, adminReq("PUT", "/api/v1/admin/rollout", map[string]any{"version": testAgentVersion}), http.StatusNoContent)
	m := deviceManifest(t, h)
	if m.Update == nil || m.Update.Version != testAgentVersion || m.Update.SHA256 != fw.SHA256 {
		t.Fatalf("expected update: %+v", m.Update)
	}
	if m.Version == base.Version {
		t.Fatal("update must change manifest version")
	}

	// 设备能下载程序包
	w := do(t, h, signedRequest("GET", m.Update.URL, nil), http.StatusOK)
	if !bytes.Equal(w.Body.Bytes(), pkg) {
		t.Fatalf("package download wrong: got %d bytes, want %d", w.Body.Len(), len(pkg))
	}
	do(t, h, httptest.NewRequest("GET", m.Update.URL, nil), http.StatusUnauthorized)

	// 设备上报目标版本后更新消失，版本回到 base
	heartbeatAs(t, h, testAgentVersion)
	if m2 := deviceManifest(t, h); m2.Update != nil || m2.Version != base.Version {
		t.Fatalf("update should disappear after device reports target version: %+v", m2)
	}

	// 定时下发：时间未到不下发，到了才下发
	heartbeatAs(t, h, "1.0.0")
	later := s.now().Add(time.Hour)
	do(t, h, adminReq("PUT", "/api/v1/admin/rollout", map[string]any{"version": testAgentVersion, "not_before": later}), http.StatusNoContent)
	if m3 := deviceManifest(t, h); m3.Update != nil {
		t.Fatalf("scheduled rollout must not fire early: %+v", m3.Update)
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if m4 := deviceManifestAt(t, h, s.now()); m4.Update == nil {
		t.Fatal("scheduled rollout should fire after not_before")
	}
	s.now = time.Now

	// 删除仍是目标的程序包被拒；取消目标后可删
	do(t, h, adminReq("DELETE", "/api/v1/admin/packages/"+testAgentVersion, nil), http.StatusConflict)
	do(t, h, adminReq("PUT", "/api/v1/admin/rollout", map[string]any{"version": ""}), http.StatusNoContent)
	do(t, h, adminReq("DELETE", "/api/v1/admin/packages/"+testAgentVersion, nil), http.StatusNoContent)

	// 未知版本/未知设备
	do(t, h, adminReq("PUT", "/api/v1/admin/rollout", map[string]any{"version": "9.9.9"}), http.StatusBadRequest)
}

func TestGlobalTemplateAndSchedules(t *testing.T) {
	s, h := newAdminTestServer(t)
	day := splitTemplate()
	day["id"] = "day"
	night := splitTemplate()
	night["id"] = "night"
	night["background"] = "#111111"
	do(t, h, adminReq("POST", "/api/v1/admin/templates", day), http.StatusOK)
	do(t, h, adminReq("POST", "/api/v1/admin/templates", night), http.StatusOK)

	// 首启已播种默认模板并设为全局默认，所以开箱就有版式（而不是黑屏）
	seeded := globalTemplateID(t, s)
	if m := deviceManifest(t, h); len(m.Items) != 1 || !strings.HasPrefix(m.Items[0].Name, "tpl_") {
		t.Fatalf("首启应当已有可用的全局模板：%+v", m.Items)
	}
	if seeded == "day" || seeded == "night" {
		t.Fatalf("播种的模板 ID 应当是自动生成的，得到 %q", seeded)
	}

	// 设全局模板 → 所有设备走模板
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]string{"template_id": "day"}), http.StatusNoContent)
	global := deviceManifest(t, h)
	if len(global.Items) != 1 || !strings.HasPrefix(global.Items[0].Name, "tpl_") {
		t.Fatalf("expected global template manifest: %+v", global.Items)
	}
	other := do(t, h, signedAs("dev-002", "other-secret", "GET", "/api/v1/device/manifest"), http.StatusOK)
	var m2 manifest.Manifest
	json.Unmarshal(other.Body.Bytes(), &m2)
	if len(m2.Items) != 1 || m2.Items[0].SHA256 != global.Items[0].SHA256 {
		t.Fatal("second device should render same global template (no attrs)")
	}

	// 不同设备属性不同 → 渲染图不同
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/dev-002/attributes", map[string]string{"room": "999"}), http.StatusNoContent)
	other = do(t, h, signedAs("dev-002", "other-secret", "GET", "/api/v1/device/manifest"), http.StatusOK)
	json.Unmarshal(other.Body.Bytes(), &m2)
	if m2.Items[0].SHA256 == global.Items[0].SHA256 {
		t.Fatal("per-device attribute must change rendered image")
	}

	// 时段计划：注入"周三 23:00"→ 命中 night；"周三 12:00"→ 无命中回落 global(day)
	do(t, h, adminReq("PUT", "/api/v1/admin/schedules", []map[string]any{
		{"template_id": "night", "start": "22:00", "end": "06:00"},
	}), http.StatusNoContent)
	wed := time.Date(2026, 9, 16, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return wed }
	noon := deviceManifestAt(t, h, s.now())
	s.now = func() time.Time { return wed.Add(11 * time.Hour) }
	late := deviceManifestAt(t, h, s.now())
	if noon.Items[0].SHA256 == late.Items[0].SHA256 {
		t.Fatal("schedule hit at night should render a different template than daytime global")
	}
	if noon.Items[0].SHA256 != global.Items[0].SHA256 {
		t.Fatal("outside schedule should fall back to global template")
	}
	s.now = time.Now

	// 管理列表显示来源
	w := do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
	var statuses []DeviceStatus
	json.Unmarshal(w.Body.Bytes(), &statuses)
	if statuses[0].ActiveSource != "global" && statuses[0].ActiveSource != "schedule" {
		t.Fatalf("active source not reported: %+v", statuses[0])
	}

	// 时段引用的模板不可删除；非法时段被拒
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/night", nil), http.StatusConflict)
	do(t, h, adminReq("PUT", "/api/v1/admin/schedules", []map[string]any{
		{"template_id": "nope", "start": "22:00", "end": "06:00"},
	}), http.StatusBadRequest)
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]string{"template_id": "nope"}), http.StatusBadRequest)

	// 设备级覆盖优先于全局
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"template_id": "night"}), http.StatusNoContent)
	if ov := deviceManifest(t, h); ov.Items[0].SHA256 == global.Items[0].SHA256 {
		t.Fatal("device override should win over global template")
	}
	_ = s
}

// 设备丢了身份文件（重装/换卡）后会用同编号、新密钥注册：服务端拒绝，但把请求记下来，
// 运营方核对后一键接受，设备的属性与播放列表都保留，不用删设备重来。
func TestRekeyRequestAcceptAndIgnore(t *testing.T) {
	s, h := newAdminTestServer(t)
	oldKey, newKey := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", oldKey, enrollToken)), http.StatusCreated)
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/scr-0017/attributes", map[string]string{"room": "302"}), http.StatusNoContent)

	// 没有待确认请求时不能"接受"
	do(t, h, adminReq("POST", "/api/v1/admin/devices/scr-0017/rekey", map[string]bool{"accept": true}), http.StatusConflict)

	// 新密钥注册：被拒，但请求被记下（后台可见，不含密钥本身）
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", newKey, enrollToken)), http.StatusConflict)
	w := do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
	if strings.Contains(w.Body.String(), newKey) || strings.Contains(w.Body.String(), oldKey) {
		t.Fatal("设备列表不得泄露密钥")
	}
	var statuses []DeviceStatus
	json.Unmarshal(w.Body.Bytes(), &statuses)
	var rk *store.RekeyRequest
	for _, st := range statuses {
		if st.ID == "scr-0017" {
			rk = st.HW.Rekey
		}
	}
	if rk == nil || rk.Fingerprint != sign.Fingerprint(newKey) || rk.Hostname != "scr-0017" {
		t.Fatalf("后台应能看到待确认的换密钥请求：%+v", rk)
	}
	// 未接受前：旧密钥照常可用，新密钥不行
	do(t, h, signedAs("scr-0017", oldKey, "GET", "/api/v1/device/manifest"), http.StatusOK)
	do(t, h, signedAs("scr-0017", newKey, "GET", "/api/v1/device/manifest"), http.StatusUnauthorized)

	// 忽略：请求消失，什么都不变
	do(t, h, adminReq("POST", "/api/v1/admin/devices/scr-0017/rekey", map[string]bool{"accept": false}), http.StatusNoContent)
	if d, _ := s.store.Device("scr-0017"); d.Rekey != nil || d.Secret != oldKey {
		t.Fatalf("忽略后应保持原样：%+v", d)
	}

	// 再来一次并接受：新密钥生效、旧密钥作废、属性保留，设备重试注册得到 200
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", newKey, enrollToken)), http.StatusConflict)
	do(t, h, adminReq("POST", "/api/v1/admin/devices/scr-0017/rekey", map[string]bool{"accept": true}), http.StatusNoContent)
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", newKey, enrollToken)), http.StatusOK)
	do(t, h, signedAs("scr-0017", newKey, "GET", "/api/v1/device/manifest"), http.StatusOK)
	do(t, h, signedAs("scr-0017", oldKey, "GET", "/api/v1/device/manifest"), http.StatusUnauthorized)
	if state(s).DeviceAttrs["scr-0017"]["room"] != "302" {
		t.Fatal("接受新密钥后设备属性应保留")
	}
}
