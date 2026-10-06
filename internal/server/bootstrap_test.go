package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// HTTP 装机端口：install.sh 填好 HTTPS 地址与证书指纹、不含口令；程序包凭注册口令下载；其余跳到 HTTPS。
func TestBootstrapEndpoints(t *testing.T) {
	s, h := newAdminTestServer(t)
	s.cfg.Listen = ":9001" // 测试服务端默认是 :0
	boot := s.BootstrapHandler()
	get := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://10.0.0.5:9000"+path, nil)
		if token != "" {
			r.Header.Set("X-Enroll-Token", token)
		}
		w := httptest.NewRecorder()
		boot.ServeHTTP(w, r)
		return w
	}

	w := get("/install.sh", "")
	script := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(script, `SERVER_URL="https://10.0.0.5:9001"`) ||
		!strings.Contains(script, `TLS_FINGERPRINT="`+s.CertFingerprint()+`"`) || len(s.CertFingerprint()) != 64 {
		t.Fatalf("install.sh 应填好 HTTPS 地址与证书指纹：%d\n%s", w.Code, script)
	}
	if strings.Contains(script, s.cfg.EnrollToken) {
		t.Fatal("公开的 install.sh 里不能带注册口令")
	}

	if w := get("/bootstrap/agent.tar.gz", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("口令不对应 401：%d", w.Code)
	}
	if w := get("/bootstrap/agent.tar.gz", s.cfg.EnrollToken); w.Code != http.StatusNotFound {
		t.Fatalf("还没上传程序包应 404：%d", w.Code)
	}
	pkg := agentPackage(t, testAgentVersion, agentBinaryFixture(t), true)
	uploadPackage(t, h, pkg)
	if w := get("/bootstrap/agent.tar.gz", s.cfg.EnrollToken); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), pkg) {
		t.Fatalf("应下载到当前的程序包：%d，%d 字节", w.Code, w.Body.Len())
	}

	// 管理后台、设备接口都不在 HTTP 端口上：跳到 HTTPS
	w = get("/admin?x=1", "")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "https://10.0.0.5:9001/admin?x=1" {
		t.Fatalf("应跳转到 HTTPS：%d %s", w.Code, w.Header().Get("Location"))
	}
	if w := get("/api/v1/device/manifest", ""); w.Code != http.StatusMovedPermanently {
		t.Fatalf("设备接口不能走 HTTP：%d", w.Code)
	}
}
