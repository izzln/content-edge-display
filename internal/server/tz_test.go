package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 服务端时区名称：server.json 设了就用它；没设时从系统找（TZ、/etc/localtime 链接、/etc/timezone），找不到为空。
func TestZoneName(t *testing.T) {
	dir := t.TempDir()
	f := zoneFiles{localtime: filepath.Join(dir, "localtime"), timezone: filepath.Join(dir, "timezone")}
	t.Setenv("TZ", "")
	if got := zoneName("Europe/Berlin", f); got != "Europe/Berlin" {
		t.Fatalf("应用 server.json 里的时区：%q", got)
	}
	if got := zoneName("", f); got != "" {
		t.Fatalf("什么都找不到时应为空：%q", got)
	}
	os.WriteFile(f.timezone, []byte("Asia/Tokyo\n"), 0o644)
	if got := zoneName("", f); got != "Asia/Tokyo" {
		t.Fatalf("应读 /etc/timezone：%q", got)
	}
	// macOS 的链接形如 /var/db/timezone/zoneinfo/Asia/Shanghai，Linux 是 /usr/share/zoneinfo/Asia/Shanghai
	os.Symlink("/var/db/timezone/zoneinfo/Asia/Shanghai", f.localtime)
	if got := zoneName("", f); got != "Asia/Shanghai" {
		t.Fatalf("应从 /etc/localtime 链接取名称：%q", got)
	}
	t.Setenv("TZ", "America/New_York")
	if got := zoneName("", f); got != "America/New_York" {
		t.Fatalf("TZ 环境变量优先：%q", got)
	}
	t.Setenv("TZ", "Not/AZone")
	if got := zoneName("", f); got != "Asia/Shanghai" {
		t.Fatalf("无效的 TZ 应跳过：%q", got)
	}
}

// 设备响应带时区头（设备据此设系统时区）；没有时区名称时不带。
func TestDeviceResponsesAnnounceTimezone(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedRequest("GET", "/api/v1/device/manifest", nil))
		return w
	}
	s.zone = "Asia/Shanghai"
	if w := get(); w.Code != http.StatusOK || w.Header().Get("X-Timezone") != "Asia/Shanghai" {
		t.Fatalf("应下发时区：%d %v", w.Code, w.Header())
	}
	s.zone = ""
	if _, ok := get().Header()["X-Timezone"]; ok {
		t.Fatal("没有时区名称时不应带这个头")
	}
}
