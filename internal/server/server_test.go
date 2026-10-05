package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
)

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

	body := strings.NewReader(`{"uptime":42,"disk_free_mb":100,"agent_version":"0.1.0"}`)
	r := signedRequest("POST", "/api/v1/device/heartbeat", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("heartbeat expected 204, got %d: %s", w.Code, w.Body.String())
	}

	w = do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
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
	if dev1 == nil || !dev1.Online || dev1.Heartbeat == nil || dev1.Heartbeat.AgentVersion != "0.1.0" || dev1.Heartbeat.UptimeS != 42 {
		t.Fatalf("unexpected device status: %+v", dev1)
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
	s, h := newAdminTestServer(t)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	t.Setenv("TZ", "UTC")
	fixed := time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)
	render := func(loc *time.Location) string {
		s.loc = loc
		s.mu.Lock()
		clear(s.sync) // 运行中不会换时区：清掉按旧时区生成的清单缓存
		s.mu.Unlock()
		do(t, h, adminReq("POST", "/api/v1/admin/devices/"+testDeviceID+"/test", map[string]int{"duration_s": 60}), http.StatusNoContent)
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
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", map[string]string{"room": "999"}), http.StatusNoContent)
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

	// 只看这台设备自己的内容：改别的设备、新建一个没人用的模板，都不能让它变成"等待刷新"
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/dev-002/attributes", map[string]string{"room": "1"}), http.StatusNoContent)
	do(t, h, adminReq("POST", "/api/v1/admin/templates", mediaTemplate("")), http.StatusOK)
	if got := state(); got != syncLatest {
		t.Fatalf("别的设备/没用到的模板改动不应影响本设备，得到 %s", got)
	}
	// 给它传了新图片：等待刷新
	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"new.jpg", tinyPNG(t)}))
	if got := state(); got != syncWaiting {
		t.Fatalf("播放列表变了应为等待刷新，得到 %s", got)
	}
}

// 指纹（content.key）必须覆盖清单的全部输入：任何一次让清单版本变化的改动，指纹都得跟着变，
// 否则后台会把还没刷新的设备显示成"已显示最新内容"。逐项改一遍核对。
func TestContentKeyCoversManifestInputs(t *testing.T) {
	s, h := newAdminTestServer(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	snapshot := func() (string, string) {
		c, err := s.content(testDeviceID, now)
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.buildManifest(testDeviceID, c)
		if err != nil {
			t.Fatal(err)
		}
		return m.Version, c.key()
	}
	gid := globalTemplateID(t, s)
	put := func(path string, body any) {
		if w := do2(t, h, adminReq("PUT", path, body)); w.Code >= 300 {
			t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	steps := []struct {
		desc   string
		change func()
	}{
		{"属性", func() { put("/api/v1/admin/devices/"+testDeviceID+"/attributes", map[string]string{"room": "302"}) }},
		{"上传图片", func() {
			parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"a.png", tinyPNG(t)}, upload{"b.png", tinyPNG(t)}))
		}},
		{"调整顺序", func() { put("/api/v1/admin/devices/"+testDeviceID+"/media", []string{"b.png", "a.png"}) }},
		{"同名文件内容变了", func() {
			img := image.NewRGBA(image.Rect(0, 0, 8, 8))
			putMediaFile(t, s, "a.png", encodePNG(t, img))
			future := now.Add(time.Minute)
			os.Chtimes(filepath.Join(s.deviceMediaDir(testDeviceID), "a.png"), future, future)
		}},
		{"左右对调", func() {
			put("/api/v1/admin/devices/"+testDeviceID+"/display", map[string]any{"mirror": true})
			// 回归：后台"保存模板设置"不能冲掉排好的播放顺序
			if got := state(s).Displays[testDeviceID].Playlist; strings.Join(got, ",") != "b.png,a.png" {
				t.Errorf("保存模板设置后播放顺序被改成了 %v", got)
			}
		}},
		{"改全局模板内容", func() {
			tpl := mediaTemplate(gid)
			tpl["image_duration_s"] = 25
			put("/api/v1/admin/templates/"+gid, tpl)
		}},
		{"时段计划到点", func() {
			w := do(t, h, adminReq("POST", "/api/v1/admin/templates", splitTemplate()), http.StatusOK)
			var created store.Template
			json.Unmarshal(w.Body.Bytes(), &created)
			loc := now.In(s.loc)
			start := loc.Add(time.Hour).Format("15:04")
			end := loc.Add(2 * time.Hour).Format("15:04")
			put("/api/v1/admin/schedules", []store.Schedule{{TemplateID: created.ID, Start: start, End: end}})
			now = now.Add(time.Hour + time.Minute) // 时间走到时段里：没有任何写入，清单也会变
		}},
		{"测试屏", func() {
			do(t, h, adminReq("POST", "/api/v1/admin/devices/"+testDeviceID+"/test", map[string]int{"duration_s": 60}), http.StatusNoContent)
		}},
		{"测试屏到期", func() { now = now.Add(2 * time.Minute) }},
	}
	ver, key := snapshot()
	for _, st := range steps {
		st.change()
		v2, k2 := snapshot()
		if v2 != ver && k2 == key {
			t.Errorf("%s：清单版本变了但指纹没变", st.desc)
		}
		if v2 == ver {
			t.Errorf("%s：这一步本应改变清单（测试写错了？）", st.desc)
		}
		ver, key = v2, k2
	}
}

// 控制台记录服务端自己的工作（上传、转码、下发），不再逐条打印设备心跳。
func TestConsoleLogsWorkNotHeartbeats(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	// 转码进度按时间间隔报：调成 0，每次进度回调都报（在服务端创建前改、关闭后再恢复，避免与转码协程竞争）
	old := progressLogInterval
	progressLogInterval = 0
	t.Cleanup(func() { progressLogInterval = old })

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
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", map[string]string{"room": "1"}), http.StatusNoContent)

	out := buf.String()
	for _, want := range []string{
		"device dev-001 online", "upload started: device dev-001, a.jpg", "upload done: device dev-001, a.jpg",
		"queued for processing as b.mp4", "upload rejected: device dev-001, c.txt", "transcode started: device dev-001, b.mp4", "transcoding: device dev-001, b.mp4 50%",
		"transcode done: device dev-001, b.mp4", "new content pushed: device dev-001", "admin PUT /devices/dev-001/attributes -> 204",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("控制台应有 %q\n%s", want, out)
		}
	}
	if strings.Count(out, " online ") != 1 || strings.Contains(out, "heartbeat") {
		t.Errorf("心跳不应逐条记日志（上线只记一次）：\n%s", out)
	}
}

// 在线判断：任何签名通过的请求都算联系过（不只心跳），约 3 个轮询周期没来就算离线；
// 轮询/心跳间隔由服务端规定，随每个设备响应下发。
func TestOnlineFromAnyRequestAndServerDefinedIntervals(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.AdminToken = adminToken
	s.cfg.PollIntervalS, s.cfg.HeartbeatIntervalS = 10, 60
	h := s.Handler()
	now := time.Now()
	s.now = func() time.Time { return now }
	status := func() DeviceStatus {
		w := do(t, h, adminReq("GET", "/api/v1/admin/devices", nil), http.StatusOK)
		var list []DeviceStatus
		json.Unmarshal(w.Body.Bytes(), &list)
		for _, d := range list {
			if d.ID == testDeviceID {
				return d
			}
		}
		t.Fatal("device missing")
		return DeviceStatus{}
	}
	send := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = signedRequestAt(now, method, path, nil)
		} else {
			r = signedRequestAt(now, method, path, strings.NewReader(body))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// 只轮询、还没心跳过（如服务端刚重启）：也算在线
	w := send("GET", "/api/v1/device/manifest", "")
	if w.Header().Get("X-Poll-Interval") != "10" || w.Header().Get("X-Heartbeat-Interval") != "60" {
		t.Fatalf("响应头应下发间隔：%v", w.Header())
	}
	if st := status(); !st.Online || st.Heartbeat != nil {
		t.Fatalf("轮询过就应在线（且没有伪造的空心跳）：%+v", st)
	}
	// 设备照服务端规定的 10 秒轮询 → 30 秒没来就离线
	if st := status(); st.PollS != 10 || st.OfflineS != 30 {
		t.Fatalf("离线阈值应为 3 个轮询周期：%+v", st)
	}
	now = now.Add(25 * time.Second)
	if !status().Online {
		t.Fatal("25 秒没来（不到 3 个轮询周期）仍应在线")
	}
	// 304 也算联系过，并且同样带着间隔
	w = send("GET", "/api/v1/device/manifest", "")
	etag := w.Header().Get("ETag")
	r := signedRequestAt(now, "GET", "/api/v1/device/manifest", nil)
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified || w.Header().Get("X-Poll-Interval") != "10" {
		t.Fatalf("304 也要带间隔：%d %v", w.Code, w.Header())
	}
	now = now.Add(31 * time.Second)
	if st := status(); st.Online || st.Sync != syncOffline {
		t.Fatalf("31 秒没有任何请求应判离线：%+v", st)
	}

	// 注册响应也带间隔：设备注册后马上就照办
	b, _ := json.Marshal(map[string]string{"device_id": "dev-new", "secret": "s", "enroll_token": "enroll-me"})
	rr := httptest.NewRequest("POST", "/api/v1/device/register", strings.NewReader(string(b)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, rr)
	if w.Header().Get("X-Poll-Interval") != "10" {
		t.Fatalf("注册响应应带间隔：%d %v", w.Code, w.Header())
	}
}

func TestIntervalConfigBounds(t *testing.T) {
	ok := Config{MediaRoot: "m", DataDir: "d", AdminToken: "a", EnrollToken: "e"}
	for _, bad := range []func(*Config){
		func(c *Config) { c.PollIntervalS = 301 },
		func(c *Config) { c.HeartbeatIntervalS = 5 },
		func(c *Config) { c.PollIntervalS = -1 },
	} {
		c := ok
		bad(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%+v 应被拒绝", c)
		}
	}
	c := ok
	if err := c.validate(); err != nil || c.PollIntervalS != 10 || c.HeartbeatIntervalS != 60 {
		t.Fatalf("默认值应为 10/60：%+v %v", c, err)
	}
}

// 设备每 10 秒轮询一次：显示输入没变时直接复用上次生成的清单（不再渲染、编码、写盘），一变就重新生成。
func TestManifestCachedUntilContentChanges(t *testing.T) {
	s, h := newAdminTestServer(t)
	first := deviceManifest(t, h)
	png := filepath.Join(s.renderedDir(), testDeviceID, first.Items[0].Name)
	os.Remove(png) // 缓存命中时不会重新渲染，文件就不会回来
	if again := deviceManifest(t, h); again.Version != first.Version {
		t.Fatal("输入没变，清单版本应不变")
	}
	if _, err := os.Stat(png); !os.IsNotExist(err) {
		t.Fatal("输入没变时不应重新渲染")
	}
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes", map[string]string{"room": "101"}), http.StatusNoContent)
	if changed := deviceManifest(t, h); changed.Version == first.Version {
		t.Fatal("属性变了，清单应重新生成")
	}
}

// 上传完成与转码完成可能同时往播放列表末尾追加：读改写在同一把锁里，谁都不能丢。
func TestConcurrentPlaylistAppends(t *testing.T) {
	s, mediaRoot := newTestServer(t)
	dir := filepath.Join(mediaRoot, testDeviceID)
	os.MkdirAll(dir, 0o755)
	var names []string
	for i := range 20 {
		n := fmt.Sprintf("f%02d.jpg", i)
		os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644)
		names = append(names, n)
	}
	var wg sync.WaitGroup
	for i := len(names) - 1; i >= 0; i-- { // 倒序追加：结果应是追加顺序，而不是按文件名
		wg.Add(1)
		go func(n string) { defer wg.Done(); s.appendPlaylist(testDeviceID, n) }(names[i])
	}
	wg.Wait()
	if got := state(s).Displays[testDeviceID].Playlist; len(got) != len(names) {
		t.Fatalf("并发追加丢了条目：%d/%d %v", len(got), len(names), got)
	}
}

// 两个口令都必填：缺了哪个都不启动（管理面不能裸奔，没有注册口令设备也接不进来）。
func TestNewRequiresTokens(t *testing.T) {
	for _, c := range []Config{
		{MediaRoot: t.TempDir(), DataDir: t.TempDir(), EnrollToken: "e"},
		{MediaRoot: t.TempDir(), DataDir: t.TempDir(), AdminToken: "a"},
	} {
		if _, err := New(&c); err == nil || !strings.Contains(err.Error(), "_token is empty") {
			t.Errorf("%+v: 应拒绝启动，得到 %v", c, err)
		}
	}
}

// 认证失败的日志节流表以请求头里的设备编号为键，谁都能随便编：表有上限，满了先清掉一分钟以前的。
func TestAuthFailureLogBounded(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := range maxAuthLogged + 50 {
		s.logAuthFailure(fmt.Sprintf("dev-%d|bad", i))
	}
	if len(s.authLogged) != maxAuthLogged {
		t.Fatalf("表应封顶在 %d：%d", maxAuthLogged, len(s.authLogged))
	}
	if s.logAuthFailure("dev-0|bad") {
		t.Fatal("同一设备同一原因一分钟内只记一次")
	}
	now = now.Add(time.Minute)
	if !s.logAuthFailure("dev-new|bad") || len(s.authLogged) != 1 {
		t.Fatalf("一分钟后应清掉旧记录再记新的：%d", len(s.authLogged))
	}
}
