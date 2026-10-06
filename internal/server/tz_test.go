package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izzln/content-edge-display/internal/store"
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

// 分时段亮度：后台设置 → 每个设备响应带 X-Brightness；不合法的设置被拒；设全局模板不会冲掉亮度计划。
func TestBrightnessSchedule(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	header := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedRequest("GET", "/api/v1/device/manifest", nil))
		return w.Header().Get("X-Brightness")
	}
	if got := header(); got != "none" {
		t.Fatalf("没设置时应为 none：%q", got)
	}
	body := map[string]any{"periods": []map[string]any{
		{"start": "22:00", "end": "07:00", "percent": 30}, {"start": "18:00", "end": "22:00", "percent": 70}}}
	do(t, h, adminReq("PUT", "/api/v1/admin/brightness", body), http.StatusNoContent)
	if got := header(); got != "18:00-22:00 70;22:00-07:00 30" {
		t.Fatalf("应下发亮度计划（按起始时间排序）：%q", got)
	}
	bad := map[string]any{"periods": []map[string]any{
		{"start": "22:00", "end": "07:00", "percent": 30}, {"start": "06:00", "end": "08:00", "percent": 50}}}
	if w := do(t, h, adminReq("PUT", "/api/v1/admin/brightness", bad), http.StatusBadRequest); !strings.Contains(w.Body.String(), "重叠") {
		t.Fatalf("重叠的时段应被拒：%s", w.Body.String())
	}
	var tpl string
	s.store.View(func(st *store.State) { tpl = st.Global.TemplateID })
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]any{"template_id": tpl}), http.StatusNoContent)
	if got := header(); got != "18:00-22:00 70;22:00-07:00 30" {
		t.Fatalf("设全局模板不应冲掉亮度计划：%q", got)
	}
	if w := do(t, h, adminReq("GET", "/api/v1/admin/global", nil), http.StatusOK); !strings.Contains(w.Body.String(), `"percent":30`) {
		t.Fatalf("后台应能读回亮度计划：%s", w.Body.String())
	}
	do(t, h, adminReq("PUT", "/api/v1/admin/brightness", map[string]any{"periods": []any{}}), http.StatusNoContent)
	if got := header(); got != "none" {
		t.Fatalf("清空后应为 none：%q", got)
	}
}
