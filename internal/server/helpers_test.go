package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/testutil"
)

// 各测试共用的服务端、请求与等待工具。

const (
	testDeviceID = "dev-001"
	testSecret   = "test-secret"
	adminToken   = "test-admin-token"
	enrollToken  = "enroll-me"
)

// newTestServer 起一个服务端：默认按"没装 ffmpeg、没装 poppler"跑，结果不依赖测试机环境（相关测试自己注入），
// 并直接写进两台设备（设备只有自注册这一条路径，测试里省去逐个走注册接口）。
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := &Config{Listen: ":9001", MediaRoot: t.TempDir(), DataDir: t.TempDir(), AdminToken: adminToken, EnrollToken: enrollToken}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(cfg, tools{encErr: "not available in tests", pdfErr: "not available in tests"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	addTestDevice(t, s, testDeviceID, testSecret)
	addTestDevice(t, s, "dev-002", "other-secret")
	return s, cfg.MediaRoot
}

// newAdminTestServer 同 newTestServer，返回路由。
func newAdminTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _ := newTestServer(t)
	return s, s.Handler()
}

func addTestDevice(t *testing.T, s *Server, id, secret string) {
	t.Helper()
	if err := s.store.Update(func(st *store.State) {
		st.Devices[id] = store.Device{ID: id, Secret: secret, RegisteredAt: s.now()}
	}); err != nil {
		t.Fatal(err)
	}
}

// setEncoder / setPDFRenderer 注入外部程序（nil 表示没有）；须在用到它们的上传之前调用。
func (s *Server) setEncoder(enc videoEncoder)  { s.tools.enc = enc }
func (s *Server) setPDFRenderer(r pdfRenderer) { s.tools.pdf = r }

// signed 以设备 id/secret 在 now 时刻签名一个请求。
func signed(id, secret string, now time.Time, method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	ts := strconv.FormatInt(now.Unix(), 10)
	r.Header.Set(sign.HeaderDeviceID, id)
	r.Header.Set(sign.HeaderTimestamp, ts)
	r.Header.Set(sign.HeaderSign, sign.Sign(secret, ts, method, r.URL.Path))
	return r
}

func signedRequest(method, path string, body *strings.Reader) *http.Request {
	return signedRequestAt(time.Now(), method, path, body)
}

// signedRequestAt 以 testDeviceID 在指定时刻签名（配合注入的 s.now 使用，避免落出时间窗）。
func signedRequestAt(now time.Time, method, path string, body *strings.Reader) *http.Request {
	if body == nil {
		return signed(testDeviceID, testSecret, now, method, path, nil)
	}
	return signed(testDeviceID, testSecret, now, method, path, body)
}

func signedAs(id, secret, method, path string) *http.Request {
	return signed(id, secret, time.Now(), method, path, nil)
}

func jsonReq(method, path string, body any) *http.Request {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func adminReq(method, path string, body any) *http.Request {
	r := jsonReq(method, path, body)
	r.Header.Set("X-Admin-Token", adminToken)
	return r
}

func do(t *testing.T, h http.Handler, r *http.Request, wantCode int) *httptest.ResponseRecorder {
	t.Helper()
	w := do2(t, h, r)
	if w.Code != wantCode {
		t.Fatalf("%s %s: expected %d, got %d: %s", r.Method, r.URL.Path, wantCode, w.Code, w.Body.String())
	}
	return w
}

// do2 发请求，不断言状态码。
func do2(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func deviceManifest(t *testing.T, h http.Handler) manifest.Manifest {
	return deviceManifestAt(t, h, time.Now())
}

func deviceManifestAt(t *testing.T, h http.Handler, now time.Time) manifest.Manifest {
	t.Helper()
	return decodeManifest(t, do(t, h, signedRequestAt(now, "GET", "/api/v1/device/manifest", nil), http.StatusOK))
}

// manifestFor 以指定设备身份拉取清单。
func manifestFor(t *testing.T, h http.Handler, dev, secret string) manifest.Manifest {
	t.Helper()
	return decodeManifest(t, do(t, h, signedAs(dev, secret, "GET", "/api/v1/device/manifest"), http.StatusOK))
}

func decodeManifest(t *testing.T, w *httptest.ResponseRecorder) manifest.Manifest {
	t.Helper()
	var m manifest.Manifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func waitForCond(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	testutil.WaitFor(t, 30*time.Second, desc, cond)
}

// state 返回状态的快照（只读检查用）。
func state(s *Server) store.State {
	var out store.State
	s.store.View(func(st *store.State) { out = *st })
	return out
}
