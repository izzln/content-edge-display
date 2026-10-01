package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
)

const adminToken = "test-admin-token"

func newAdminTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _ := newTestServer(t)
	s.cfg.AdminToken = adminToken
	return s, s.Handler()
}

func adminReq(method, path string, body any) *http.Request {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("X-Admin-Token", adminToken)
	return r
}

func do(t *testing.T, h http.Handler, r *http.Request, wantCode int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != wantCode {
		t.Fatalf("%s %s: expected %d, got %d: %s", r.Method, r.URL.Path, wantCode, w.Code, w.Body.String())
	}
	return w
}

// deviceManifest 以设备身份拉取 manifest。
func deviceManifest(t *testing.T, h http.Handler) manifest.Manifest {
	return deviceManifestAt(t, h, time.Now())
}

// deviceManifestAt 以指定时刻签名拉取 manifest（配合注入的 s.now）。
func deviceManifestAt(t *testing.T, h http.Handler, now time.Time) manifest.Manifest {
	t.Helper()
	w := do(t, h, signedRequestAt(now, "GET", "/api/v1/device/manifest", nil), http.StatusOK)
	var m manifest.Manifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 列表接口在数据为空时必须返回 []，不能是 null：管理后台拿到 null 会在渲染时
// 抛 TypeError，整页按钮失效（曾因 schedules 返回 null 导致“上传图片按钮无效”）。
func TestAdminListEndpointsReturnEmptyArrayNotNull(t *testing.T) {
	_, h := newAdminTestServer(t)
	for _, ep := range []string{"schedules", "templates", "firmware", "devices"} {
		w := do(t, h, adminReq("GET", "/api/v1/admin/"+ep, nil), http.StatusOK)
		body := strings.TrimSpace(w.Body.String())
		if body == "null" {
			t.Errorf("%s: 返回 null，应为 []", ep)
			continue
		}
		var list []any
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Errorf("%s: 不是 JSON 数组: %s", ep, body)
		}
	}
}

func TestAdminWriteRequiresToken(t *testing.T) {
	s, _ := newTestServer(t) // AdminToken 为空
	h := s.Handler()

	// 未配置 token：写接口一律 403
	r := httptest.NewRequest("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", strings.NewReader("{}"))
	do(t, h, r, http.StatusForbidden)

	// 配置了 token：无 token 401，有 token 放行
	s.cfg.AdminToken = adminToken
	r = httptest.NewRequest("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", strings.NewReader("{}"))
	do(t, h, r, http.StatusUnauthorized)
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", map[string]string{}), http.StatusOK)
}

func TestAttributesRoundTrip(t *testing.T) {
	_, h := newAdminTestServer(t)

	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes",
		map[string]string{"room": "302", "楼层": "3"}), http.StatusOK)

	// 非法 key
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes",
		map[string]string{"bad key!": "x"}), http.StatusBadRequest)
	// 未知设备
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/nope/attributes",
		map[string]string{}), http.StatusNotFound)

	// 属性随设备列表返回（后台就靠这个显示和编辑，没有单独的读取接口）
	w := do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
	var statuses []DeviceStatus
	json.Unmarshal(w.Body.Bytes(), &statuses)
	for _, st := range statuses {
		if st.ID == testDeviceID && (st.Attrs["room"] != "302" || st.Attrs["楼层"] != "3") {
			t.Fatalf("device list missing attrs: %+v", st)
		}
	}
}

// splitTemplate 左半属性、右半固定文字：没有媒体区，整块渲染成一张整屏图。
func splitTemplate() map[string]any {
	return map[string]any{
		"id": "split", "name": "左右分屏",
		"regions": []map[string]any{
			{"id": "left", "x": 0, "y": 0, "w": 720, "h": 900, "type": "attribute", "key": "room", "font_size": 160, "bg": "#1E3A8A"},
			{"id": "right", "x": 720, "y": 0, "w": 720, "h": 900, "type": "text", "key": "欢迎"},
		},
	}
}

// mediaTemplate 左半属性、右半媒体区（播放列表由设备端播放，可以是视频）。
func mediaTemplate(id string) map[string]any {
	return map[string]any{
		"id": id, "name": "左右分屏(媒体)", "image_duration_s": 10,
		"regions": []map[string]any{
			{"id": "left", "x": 0, "y": 0, "w": 720, "h": 900, "type": "attribute", "key": "room", "font_size": 160, "bg": "#1E3A8A"},
			{"id": "right", "x": 720, "y": 0, "w": 720, "h": 900, "type": "media"},
		},
	}
}

// globalTemplateID 返回首启播种出来的默认模板 ID。
func globalTemplateID(t *testing.T, s *Server) string {
	t.Helper()
	id := s.store.Global().TemplateID
	if id == "" {
		t.Fatal("首次启动应当已播种默认模板并设为全局默认")
	}
	return id
}

func TestTemplateCRUDAndPreview(t *testing.T) {
	_, h := newAdminTestServer(t)

	do(t, h, adminReq("POST", "/api/v1/admin/templates", splitTemplate()), http.StatusOK)

	w := do(t, h, adminReq("GET", "/api/v1/admin/templates", nil), http.StatusOK)
	if !strings.Contains(w.Body.String(), `"split"`) {
		t.Fatalf("template not listed: %s", w.Body.String())
	}

	// 校验失败
	bad := splitTemplate()
	bad["id"] = "bad id!"
	do(t, h, adminReq("POST", "/api/v1/admin/templates", bad), http.StatusBadRequest)

	// 预览出 PNG
	w = do(t, h, adminReq("GET", "/api/v1/admin/templates/split/preview", nil), http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("preview content-type: %s", ct)
	}
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatal("preview is not a PNG")
	}

	// 被设备引用时禁止删除
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "template", "template_id": "split"}), http.StatusOK)
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/split", nil), http.StatusConflict)

	// 解除引用后可删除
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "global"}), http.StatusOK)
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/split", nil), http.StatusOK)
}

func TestTemplateModeManifest(t *testing.T) {
	_, h := newAdminTestServer(t)

	do(t, h, adminReq("POST", "/api/v1/admin/templates", splitTemplate()), http.StatusOK)
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes",
		map[string]string{"room": "302"}), http.StatusOK)
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "template", "template_id": "split"}), http.StatusOK)

	m := deviceManifest(t, h)
	if len(m.Items) != 1 || m.Items[0].Type != "image" || !strings.HasPrefix(m.Items[0].URL, "/render/"+testDeviceID+"/") {
		t.Fatalf("unexpected template manifest: %+v", m)
	}

	// 设备能下载渲染图，且校验和一致
	w := do(t, h, signedRequest("GET", m.Items[0].URL, nil), http.StatusOK)
	if int64(w.Body.Len()) != m.Items[0].Size {
		t.Fatalf("rendered size mismatch: %d vs %d", w.Body.Len(), m.Items[0].Size)
	}

	// 属性变化 → 版本变化
	v1 := m.Version
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes",
		map[string]string{"room": "999"}), http.StatusOK)
	if m2 := deviceManifest(t, h); m2.Version == v1 {
		t.Fatal("attribute change did not change manifest version")
	}

	// 切回跟随全局 → 用首启播种的默认模板；媒体区还没有内容，渲染成整屏图而不是黑屏
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "global"}), http.StatusOK)
	m3 := deviceManifest(t, h)
	if len(m3.Items) != 1 || !strings.HasPrefix(m3.Items[0].Name, "tpl_") || m3.Layout != nil {
		t.Fatalf("媒体区没内容时应回落到整屏图：%+v", m3)
	}
}

func TestTestScreenOverridesAndExpires(t *testing.T) {
	s, h := newAdminTestServer(t)

	base := deviceManifest(t, h)

	do(t, h, adminReq("POST", "/api/v1/admin/devices/"+testDeviceID+"/test",
		map[string]int{"duration_s": 60}), http.StatusOK)

	m := deviceManifest(t, h)
	if len(m.Items) != 1 || !strings.HasPrefix(m.Items[0].Name, "test_") {
		t.Fatalf("expected test card manifest: %+v", m)
	}
	if m.Version == base.Version {
		t.Fatal("test mode must change version")
	}

	// 时间推进到过期 → 自动回落
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if m2 := deviceManifest(t, h); m2.Version != base.Version {
		t.Fatalf("expected fallback to base manifest after expiry, got %+v", m2)
	}

	// 主动取消
	s.now = time.Now
	do(t, h, adminReq("POST", "/api/v1/admin/devices/"+testDeviceID+"/test",
		map[string]int{"duration_s": 60}), http.StatusOK)
	do(t, h, adminReq("POST", "/api/v1/admin/devices/"+testDeviceID+"/test",
		map[string]int{"duration_s": 0}), http.StatusOK)
	if m3 := deviceManifest(t, h); m3.Version != base.Version {
		t.Fatal("cancel test did not restore base manifest")
	}
}

func TestRenderAccessControl(t *testing.T) {
	_, h := newAdminTestServer(t)
	do(t, h, adminReq("POST", "/api/v1/admin/devices/"+testDeviceID+"/test",
		map[string]int{"duration_s": 60}), http.StatusOK)
	m := deviceManifest(t, h)

	// 他设备身份访问 → 403（签名为 dev-001，路径伪装 dev-002）
	other := strings.Replace(m.Items[0].URL, testDeviceID, "dev-002", 1)
	do(t, h, signedRequest("GET", other, nil), http.StatusForbidden)
	// 无签名 → 401
	do(t, h, httptest.NewRequest("GET", m.Items[0].URL, nil), http.StatusUnauthorized)
}
