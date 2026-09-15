package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
)

const enrollToken = "enroll-me"

func newEnrollTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _ := newTestServer(t)
	s.cfg.AdminToken = adminToken
	s.cfg.EnrollToken = enrollToken
	return s, s.Handler()
}

func registerBody(id, secret, token string) map[string]string {
	return map[string]string{
		"device_id": id, "secret": secret, "enroll_token": token,
		"hostname": "scr-0017", "hw_serial": "0123456789abcdef", "agent_version": "1.0.0",
	}
}

func jsonReq(method, path string, body any) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// signedAs 以任意设备身份签名。
func signedAs(id, secret, method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	ts := sign.Now()
	r.Header.Set(sign.HeaderDeviceID, id)
	r.Header.Set(sign.HeaderTimestamp, ts)
	r.Header.Set(sign.HeaderSign, sign.Sign(secret, ts, method, r.URL.Path))
	return r
}

func TestRegisterFlow(t *testing.T) {
	_, h := newEnrollTestServer(t)
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
	if found == nil || !found.Registered || found.HW == nil || found.HW.Hostname != "scr-0017" || found.HW.Secret != "" {
		t.Fatalf("registered device not listed properly: %+v", found)
	}

	// 改名、删除、删除后可重新注册
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/scr-0017/name", map[string]string{"name": "3楼大堂"}), http.StatusNoContent)
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/scr-0017", nil), http.StatusNoContent)
	do(t, h, signedAs("scr-0017", secret, "GET", "/api/v1/device/manifest"), http.StatusUnauthorized)
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("scr-0017", strings.Repeat("cd", 32), enrollToken)), http.StatusCreated)

	// 静态设备不能删
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID, nil), http.StatusConflict)
}

func TestRegisterDisabledWithoutToken(t *testing.T) {
	_, h := newAdminTestServer(t) // 无 EnrollToken
	do(t, h, jsonReq("POST", "/api/v1/device/register", registerBody("x-1", strings.Repeat("ab", 32), "")), http.StatusForbidden)
}

func uploadFirmware(t *testing.T, h http.Handler, version string, content []byte) store.Firmware {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("version", version)
	mw.WriteField("notes", "test build")
	fw, _ := mw.CreateFormFile("file", "display-agent-armv7")
	fw.Write(content)
	mw.Close()
	r := httptest.NewRequest("POST", "/api/v1/admin/firmware", &buf)
	r.Header.Set("X-Admin-Token", adminToken)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := do(t, h, r, http.StatusOK)
	var meta store.Firmware
	json.Unmarshal(w.Body.Bytes(), &meta)
	return meta
}

func heartbeatAs(t *testing.T, h http.Handler, agentVer string) {
	t.Helper()
	body := strings.NewReader(`{"version":"v","uptime":1,"disk_free_mb":1,"playing":"","player_ver":"` + agentVer + `"}`)
	r := signedRequest("POST", "/api/v1/device/heartbeat", body)
	r.Header.Set("Content-Type", "application/json")
	do(t, h, r, http.StatusNoContent)
}

func TestFirmwareRolloutAndUpdateCommand(t *testing.T) {
	s, h := newAdminTestServer(t)
	heartbeatAs(t, h, "1.0.0")

	fw := uploadFirmware(t, h, "1.1.0", []byte("new-binary-bytes"))
	if fw.Size != int64(len("new-binary-bytes")) || len(fw.SHA256) != 64 {
		t.Fatalf("firmware meta wrong: %+v", fw)
	}

	base := deviceManifest(t, h)
	if len(base.Commands) != 0 {
		t.Fatalf("no rollout yet, expected no commands: %+v", base.Commands)
	}

	// 立即下发全部设备
	do(t, h, adminReq("PUT", "/api/v1/admin/rollout", map[string]any{"version": "1.1.0"}), http.StatusOK)
	m := deviceManifest(t, h)
	if len(m.Commands) != 1 || m.Commands[0].Type != "update" || m.Commands[0].Version != "1.1.0" || m.Commands[0].SHA256 != fw.SHA256 {
		t.Fatalf("expected update command: %+v", m.Commands)
	}
	if m.Version == base.Version {
		t.Fatal("update command must change manifest version")
	}

	// 设备能下载固件
	w := do(t, h, signedRequest("GET", m.Commands[0].URL, nil), http.StatusOK)
	if w.Body.String() != "new-binary-bytes" {
		t.Fatalf("firmware download wrong: %q", w.Body.String())
	}
	do(t, h, httptest.NewRequest("GET", m.Commands[0].URL, nil), http.StatusUnauthorized)

	// 设备上报目标版本后指令消失，版本回到 base
	heartbeatAs(t, h, "1.1.0")
	if m2 := deviceManifest(t, h); len(m2.Commands) != 0 || m2.Version != base.Version {
		t.Fatalf("command should disappear after device reports target version: %+v", m2)
	}

	// 定时下发：时间未到不下发，到了才下发
	heartbeatAs(t, h, "1.0.0")
	later := s.now().Add(time.Hour)
	do(t, h, adminReq("PUT", "/api/v1/admin/rollout", map[string]any{"version": "1.1.0", "not_before": later}), http.StatusOK)
	if m3 := deviceManifest(t, h); len(m3.Commands) != 0 {
		t.Fatalf("scheduled rollout must not fire early: %+v", m3.Commands)
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if m4 := deviceManifestAt(t, h, s.now()); len(m4.Commands) != 1 {
		t.Fatalf("scheduled rollout should fire after not_before: %+v", m4.Commands)
	}
	s.now = time.Now

	// 删除仍是目标的固件被拒；取消目标后可删
	do(t, h, adminReq("DELETE", "/api/v1/admin/firmware/1.1.0", nil), http.StatusConflict)
	do(t, h, adminReq("PUT", "/api/v1/admin/rollout", map[string]any{"version": ""}), http.StatusOK)
	do(t, h, adminReq("DELETE", "/api/v1/admin/firmware/1.1.0", nil), http.StatusNoContent)

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

	// 无全局模板：目录轮播（空）
	if m := deviceManifest(t, h); len(m.Items) != 0 {
		t.Fatalf("expected playlist mode: %+v", m.Items)
	}

	// 设全局模板 → 所有设备走模板
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]string{"template_id": "day"}), http.StatusOK)
	global := deviceManifest(t, h)
	if len(global.Items) != 1 || !strings.HasPrefix(global.Items[0].Name, "tpl_") {
		t.Fatalf("expected global template manifest: %+v", global.Items)
	}
	other := do(t, h, signedAs("dev-002", "other-secret", "GET", "/api/v1/device/manifest"), http.StatusOK)
	var m2 manifest.Manifest
	json.Unmarshal(other.Body.Bytes(), &m2)
	if len(m2.Items) != 1 || m2.Items[0].SHA256 != global.Items[0].SHA256 {
		t.Fatal("second device should render same global template (no bindings/attrs)")
	}

	// 不同设备属性不同 → 渲染图不同
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/dev-002/attributes", map[string]string{"room": "999"}), http.StatusOK)
	other = do(t, h, signedAs("dev-002", "other-secret", "GET", "/api/v1/device/manifest"), http.StatusOK)
	json.Unmarshal(other.Body.Bytes(), &m2)
	if m2.Items[0].SHA256 == global.Items[0].SHA256 {
		t.Fatal("per-device attribute must change rendered image")
	}

	// 时段计划：注入"周三 23:00"→ 命中 night；"周三 12:00"→ 无命中回落 global(day)
	do(t, h, adminReq("PUT", "/api/v1/admin/schedules", []map[string]any{
		{"template_id": "night", "start": "22:00", "end": "06:00"},
	}), http.StatusOK)
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

	// 全局/时段引用的模板不可删除；非法时段被拒
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/day", nil), http.StatusConflict)
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/night", nil), http.StatusConflict)
	do(t, h, adminReq("PUT", "/api/v1/admin/schedules", []map[string]any{
		{"template_id": "nope", "start": "22:00", "end": "06:00"},
	}), http.StatusBadRequest)
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]string{"template_id": "nope"}), http.StatusBadRequest)

	// 设备级覆盖优先于全局
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "template", "template_id": "night"}), http.StatusOK)
	if ov := deviceManifest(t, h); ov.Items[0].SHA256 == global.Items[0].SHA256 {
		t.Fatal("device override should win over global template")
	}
	_ = s
}
