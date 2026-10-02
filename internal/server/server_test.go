package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
)

const (
	testDeviceID = "dev-001"
	testSecret   = "test-secret"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	mediaRoot := t.TempDir()
	cfg := &Config{
		Listen:      ":0",
		MediaRoot:   mediaRoot,
		DataDir:     t.TempDir(),
		EnrollToken: "enroll-me",
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	// 默认按"没装 ffmpeg"跑，结果不依赖测试机环境；转码相关测试自己注入转码器。
	s.setEncoder(nil)
	// 设备只有自注册这一条路径，测试里直接写进 store，省去逐个走注册接口。
	addTestDevice(t, s, testDeviceID, testSecret)
	addTestDevice(t, s, "dev-002", "other-secret")
	return s, mediaRoot
}

func addTestDevice(t *testing.T, s *Server, id, secret string) {
	t.Helper()
	err := s.store.Update(func(st *store.State) error {
		st.Devices[id] = store.Device{ID: id, Secret: secret, RegisteredAt: s.now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
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

// 401 要说明原因：时钟偏差、密钥不对、设备不存在的处理办法完全不同。
func TestAuthFailureExplainsWhy(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	get := func(r *http.Request) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
		return w.Body.String()
	}

	if body := get(signedRequestAt(time.Now().Add(-2*time.Hour), "GET", "/api/v1/device/manifest", nil)); !strings.Contains(body, "clock skew") || !strings.Contains(body, "server time") {
		t.Fatalf("时钟偏差应说明两边时间：%s", body)
	}
	r := signedRequest("GET", "/api/v1/device/manifest", nil)
	r.Header.Set(sign.HeaderSign, sign.Sign("wrong-key", r.Header.Get(sign.HeaderTimestamp), "GET", "/api/v1/device/manifest"))
	if body := get(r); !strings.Contains(body, "bad signature") {
		t.Fatalf("密钥不对应说明：%s", body)
	}
	r = signedRequest("GET", "/api/v1/device/manifest", nil)
	r.Header.Set(sign.HeaderDeviceID, "deleted-device")
	if body := get(r); !strings.Contains(body, "unknown device") {
		t.Fatalf("设备不存在应说明（设备端据此自动重新注册）：%s", body)
	}
	// 下载接口同样
	if body := get(signedRequestAt(time.Now().Add(time.Hour), "GET", "/media/"+testDeviceID+"/a.jpg", nil)); !strings.Contains(body, "clock skew") {
		t.Fatalf("下载接口也应说明原因：%s", body)
	}
}

// 测试卡渲染要用 server.json 配置的时区，而不是服务器操作系统的时区。
func TestTestCardUsesConfiguredTimezone(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.AdminToken = "tok"
	h := s.Handler()
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	t.Setenv("TZ", "UTC")
	fixed := time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)
	render := func(loc *time.Location) string {
		s.loc = loc
		r := httptest.NewRequest("POST", "/api/v1/admin/devices/"+testDeviceID+"/test", strings.NewReader(`{"duration_s":60}`))
		r.Header.Set("X-Admin-Token", "tok")
		h.ServeHTTP(httptest.NewRecorder(), r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedRequestAt(fixed, "GET", "/api/v1/device/manifest", nil))
		var m struct {
			Items []struct{ SHA256 string } `json:"items"`
		}
		json.Unmarshal(w.Body.Bytes(), &m)
		if len(m.Items) != 1 {
			t.Fatalf("应拿到测试卡：%d %s", w.Code, w.Body.String())
		}
		return m.Items[0].SHA256
	}
	s.now = func() time.Time { return fixed }
	if render(tokyo) == render(time.UTC) {
		t.Fatal("配置的时区不同，测试卡上的结束时间应不同")
	}
}

// 后台"当前显示"列：等待刷新 → 正在刷新 → 已显示最新内容。
func TestDeviceSyncState(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.AdminToken = adminToken
	h := s.Handler()
	state := func() string {
		w := do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
		var list []DeviceStatus
		json.Unmarshal(w.Body.Bytes(), &list)
		for _, d := range list {
			if d.ID == testDeviceID {
				return d.Sync
			}
		}
		t.Fatal("device missing")
		return ""
	}
	poll := func(applied string) string {
		r := signedRequest("GET", "/api/v1/device/manifest", nil)
		if applied != "" {
			r.Header.Set("If-None-Match", `"`+applied+`"`)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return strings.Trim(w.Header().Get("ETag"), `"`)
	}
	if got := state(); got != syncOffline {
		t.Fatalf("没有心跳时应为离线，得到 %s", got)
	}
	heartbeatAs(t, h, "1.0.0")
	if got := state(); got != syncWaiting {
		t.Fatalf("还没来取过内容时应为等待刷新，得到 %s", got)
	}
	v1 := poll("")
	if got := state(); got != syncSyncing {
		t.Fatalf("取到新清单、还没应用时应为正在刷新，得到 %s", got)
	}
	poll(v1)
	if got := state(); got != syncLatest {
		t.Fatalf("设备报告已应用最新版本时应为已显示最新内容，得到 %s", got)
	}
	// 运营方改了内容：设备下次轮询前是"等待刷新"，取到后"正在刷新"，应用后"最新"
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", map[string]string{"room": "999"}), http.StatusOK)
	if got := state(); got != syncWaiting {
		t.Fatalf("改了配置后应为等待刷新，得到 %s", got)
	}
	v2 := poll(v1)
	if v2 == v1 {
		t.Fatal("改了属性，版本号应变化")
	}
	if got := state(); got != syncSyncing {
		t.Fatalf("取到新清单后应为正在刷新，得到 %s", got)
	}
	poll(v2)
	if got := state(); got != syncLatest {
		t.Fatalf("应用后应为已显示最新内容，得到 %s", got)
	}
}

// 控制台记录服务端自己的工作（上传、转码、下发），不再逐条打印设备心跳。
func TestConsoleLogsWorkNotHeartbeats(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	s, h := newAdminTestServer(t)
	s.setEncoder(&fakeEncoder{})
	heartbeatAs(t, h, "1.0.0")
	heartbeatAs(t, h, "1.0.0")
	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"a.jpg", tinyPNG(t)}, upload{"b.mp4", fakeVideo}, upload{"c.txt", []byte("x")}))
	waitMedia(t, h, "转码完成", func(fs []MediaFile) bool {
		f, ok := byName(fs, "b.mp4")
		return ok && f.Status == mediaReady
	})
	deviceManifest(t, h)
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", map[string]string{"room": "1"}), http.StatusOK)

	out := buf.String()
	for _, want := range []string{
		"设备 dev-001 上线", "接收上传 → 设备 dev-001：a.jpg", "上传完成 ← 设备 dev-001：a.jpg",
		"已排队转码为 b.mp4", "上传被拒 ← 设备 dev-001：c.txt", "开始转码 设备 dev-001：b.mp4",
		"转码成功 设备 dev-001：b.mp4", "下发新内容 → 设备 dev-001", "后台操作 PUT /devices/dev-001/attributes → 200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("控制台应有 %q\n%s", want, out)
		}
	}
	if strings.Count(out, "上线") != 1 || strings.Contains(out, "heartbeat device=") {
		t.Errorf("心跳不应逐条记日志（上线只记一次）：\n%s", out)
	}
}
