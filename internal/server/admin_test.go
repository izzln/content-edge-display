package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
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

	w := do(t, h, adminReq("GET", "/api/v1/admin/devices/"+testDeviceID+"/attributes", nil), http.StatusOK)
	var attrs map[string]string
	json.Unmarshal(w.Body.Bytes(), &attrs)
	if attrs["room"] != "302" || attrs["楼层"] != "3" {
		t.Fatalf("attrs wrong: %v", attrs)
	}

	// 非法 key
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes",
		map[string]string{"bad key!": "x"}), http.StatusBadRequest)
	// 未知设备
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/nope/attributes",
		map[string]string{}), http.StatusNotFound)

	// 设备列表带出属性
	w = do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
	var statuses []DeviceStatus
	json.Unmarshal(w.Body.Bytes(), &statuses)
	for _, st := range statuses {
		if st.ID == testDeviceID && st.Attrs["room"] != "302" {
			t.Fatalf("device list missing attrs: %+v", st)
		}
	}
}

func splitTemplate() map[string]any {
	return map[string]any{
		"id": "split", "name": "左右分屏",
		"regions": []map[string]any{
			{"id": "left", "x": 0, "y": 0, "w": 720, "h": 900, "type": "attribute", "key": "room", "font_size": 160, "bg": "#1E3A8A"},
			{"id": "right", "x": 720, "y": 0, "w": 720, "h": 900, "type": "image"},
		},
	}
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
		map[string]any{"mode": "playlist"}), http.StatusOK)
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/split", nil), http.StatusNoContent)
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

	// 切回 playlist → 回到目录清单
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "playlist"}), http.StatusOK)
	if m3 := deviceManifest(t, h); len(m3.Items) != 0 {
		t.Fatalf("expected empty playlist manifest, got %+v", m3)
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

func TestUploadAndBindingFlow(t *testing.T) {
	_, h := newAdminTestServer(t)

	// 上传一张 1×1 PNG
	var pngBuf bytes.Buffer
	tiny := image.NewRGBA(image.Rect(0, 0, 1, 1))
	tiny.SetRGBA(0, 0, color.RGBA{0xFF, 0xFF, 0xFF, 0xFF})
	if err := png.Encode(&pngBuf, tiny); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "logo.png")
	fw.Write(pngBuf.Bytes())
	mw.Close()
	r := httptest.NewRequest("POST", "/api/v1/admin/uploads", &buf)
	r.Header.Set("X-Admin-Token", adminToken)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	do(t, h, r, http.StatusOK)

	w := do(t, h, adminReq("GET", "/api/v1/admin/uploads", nil), http.StatusOK)
	if !strings.Contains(w.Body.String(), "logo.png") {
		t.Fatalf("upload not listed: %s", w.Body.String())
	}

	// 绑定到模板 image 区域并出图
	do(t, h, adminReq("POST", "/api/v1/admin/templates", splitTemplate()), http.StatusOK)
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "template", "template_id": "split",
			"bindings": map[string]string{"right": "logo.png"}}), http.StatusOK)
	m := deviceManifest(t, h)
	if len(m.Items) != 1 {
		t.Fatalf("expected rendered manifest: %+v", m)
	}

	// 绑定不存在的区域被拒
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "template", "template_id": "split",
			"bindings": map[string]string{"nope": "logo.png"}}), http.StatusBadRequest)
}
