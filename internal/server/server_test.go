package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/sign"
)

const (
	testDeviceID = "dev-001"
	testSecret   = "test-secret"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	mediaRoot := t.TempDir()
	cfg := &Config{
		Listen:         ":0",
		MediaRoot:      mediaRoot,
		DataDir:        t.TempDir(),
		ImageDurationS: 10,
		Devices: []DeviceConfig{
			{ID: testDeviceID, Secret: testSecret, Name: "客户A"},
			{ID: "dev-002", Secret: "other-secret", Name: "客户B"},
		},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, mediaRoot
}

func signedRequest(method, path string, body *strings.Reader) *http.Request {
	return signedRequestAt(time.Now(), method, path, body)
}

// signedRequestAt 以指定时刻签名（配合注入的 s.now 使用，避免落出时间窗）。
func signedRequestAt(now time.Time, method, path string, body *strings.Reader) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, body)
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	r.Header.Set(sign.HeaderDeviceID, testDeviceID)
	r.Header.Set(sign.HeaderTimestamp, ts)
	r.Header.Set(sign.HeaderSign, sign.Sign(testSecret, ts, method, r.URL.Path))
	return r
}

func TestManifestRequiresAuth(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	// 无认证头
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/device/manifest", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// 签名错误
	r := signedRequest("GET", "/api/v1/device/manifest", nil)
	r.Header.Set(sign.HeaderSign, "bogus")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad sign, got %d", w.Code)
	}
}

func TestManifestAndETag(t *testing.T) {
	s, mediaRoot := newTestServer(t)
	h := s.Handler()
	devDir := filepath.Join(mediaRoot, testDeviceID)
	os.MkdirAll(devDir, 0o755)
	os.WriteFile(filepath.Join(devDir, "a.jpg"), []byte("img"), 0o644)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest("GET", "/api/v1/device/manifest", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	var m struct {
		Version string `json:"version"`
		Items   []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Items) != 1 || m.Items[0].Name != "a.jpg" {
		t.Fatalf("unexpected manifest: %+v", m)
	}

	// If-None-Match 命中 → 304
	r := signedRequest("GET", "/api/v1/device/manifest", nil)
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", w.Code)
	}
}

func TestMediaAccessControl(t *testing.T) {
	s, mediaRoot := newTestServer(t)
	h := s.Handler()
	devDir := filepath.Join(mediaRoot, testDeviceID)
	os.MkdirAll(devDir, 0o755)
	os.WriteFile(filepath.Join(devDir, "a.jpg"), []byte("0123456789"), 0o644)

	// 正常下载
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest("GET", "/media/"+testDeviceID+"/a.jpg", nil))
	if w.Code != http.StatusOK || w.Body.String() != "0123456789" {
		t.Fatalf("download failed: %d %q", w.Code, w.Body.String())
	}

	// Range 断点续传
	r := signedRequest("GET", "/media/"+testDeviceID+"/a.jpg", nil)
	r.Header.Set("Range", "bytes=4-")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "456789" {
		t.Fatalf("range failed: %d %q", w.Code, w.Body.String())
	}

	// 访问他人目录 → 403
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest("GET", "/media/dev-002/a.jpg", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-device access, got %d", w.Code)
	}

	// 路径穿越（编码斜杠落到单段 PathValue 内）→ 400
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest("GET", "/media/"+testDeviceID+"/..%2Fsecret", nil))
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Fatalf("expected 400/404 for traversal, got %d", w.Code)
	}
}

func TestHeartbeatAndAdmin(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	body := strings.NewReader(`{"version":"abc","uptime":42,"disk_free_mb":100,"playing":"x.jpg","player_ver":"0.1.0"}`)
	r := signedRequest("POST", "/api/v1/device/heartbeat", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("heartbeat expected 204, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/admin/devices", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("admin expected 200, got %d", w.Code)
	}
	var statuses []DeviceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(statuses))
	}
	var dev1 *DeviceStatus
	for i := range statuses {
		if statuses[i].ID == testDeviceID {
			dev1 = &statuses[i]
		}
	}
	if dev1 == nil || !dev1.Online || dev1.Heartbeat == nil || dev1.Heartbeat.Version != "abc" {
		t.Fatalf("unexpected device status: %+v", dev1)
	}
}

func TestAdminTokenRequired(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.AdminToken = "tok"
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/admin/devices", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	r := httptest.NewRequest("GET", "/api/v1/admin/devices", nil)
	r.Header.Set("X-Admin-Token", "tok")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", w.Code)
	}
}
