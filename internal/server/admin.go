package server

import (
	"fmt"
	"image/png"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
	"github.com/izzln/content-edge-display/internal/web"
)

// registerAdmin 挂载管理后台 UI 与管理 API（一律凭 admin_token）。
func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.handleAdminUI)
	mux.HandleFunc("GET /admin/{$}", s.handleAdminUI)

	for route, h := range map[string]http.HandlerFunc{
		"GET /api/v1/admin/info":                            s.handleInfo,
		"GET /api/v1/admin/devices":                         s.handleAdminDevices,
		"DELETE /api/v1/admin/devices/{id}":                 s.handleDeleteDevice,
		"PUT /api/v1/admin/devices/{id}/attributes":         s.handlePutAttrs,
		"POST /api/v1/admin/devices/{id}/test":              s.handleTest,
		"POST /api/v1/admin/devices/{id}/rekey":             s.handleRekey,
		"PUT /api/v1/admin/devices/{id}/display":            s.handlePutDisplay,
		"GET /api/v1/admin/devices/{id}/media":              s.handleListDeviceMedia,
		"PUT /api/v1/admin/devices/{id}/media":              s.handleReorderDeviceMedia,
		"GET /api/v1/admin/devices/{id}/media/{file}/thumb": s.handleDeviceMediaThumb,
		"DELETE /api/v1/admin/devices/{id}/media/{file}":    s.handleDeleteDeviceMedia,
		"GET /api/v1/admin/templates":                       s.handleListTemplates,
		"POST /api/v1/admin/templates":                      s.handlePutTemplate,
		"PUT /api/v1/admin/templates/{id}":                  s.handlePutTemplate,
		"DELETE /api/v1/admin/templates/{id}":               s.handleDeleteTemplate,
		"GET /api/v1/admin/templates/{id}/preview":          s.handlePreview,
		"POST /api/v1/admin/templates/{id}/background":      s.handleUploadBackground,
		"DELETE /api/v1/admin/templates/{id}/background":    s.handleDeleteBackground,
		"GET /api/v1/admin/templates/{id}/guide":            s.handleBackgroundGuide,
		"GET /api/v1/admin/backgrounds/{file}":              s.handleGetBackground,
		"GET /api/v1/admin/global":                          s.handleGetGlobal,
		"PUT /api/v1/admin/global":                          s.handlePutGlobal,
		"GET /api/v1/admin/schedules":                       s.handleGetSchedules,
		"PUT /api/v1/admin/schedules":                       s.handlePutSchedules,
		"GET /api/v1/admin/packages":                        s.handleListPackages,
		"POST /api/v1/admin/packages":                       s.handleUploadPackage,
		"DELETE /api/v1/admin/packages/{version}":           s.handleDeletePackage,
		"GET /api/v1/admin/deps":                            s.handleListDeps,
		"DELETE /api/v1/admin/deps/{codename}":              s.handleDeleteDeps,
		"PUT /api/v1/admin/rollout":                         s.handleRollout,
		"GET /api/v1/admin/token":                           s.handleGetAdminToken,
		"PUT /api/v1/admin/token":                           s.handlePutAdminToken,
		"GET /api/v1/admin/access":                          s.handleGetAccess,
		"PUT /api/v1/admin/access":                          s.handlePutAccess,
		"GET /api/v1/admin/cache":                           s.handleGetCache,
		"PUT /api/v1/admin/cache":                           s.handlePutCache,
	} {
		mux.HandleFunc(route, s.admin(h, true))
	}
	// 上传接口自己逐个文件记日志
	mux.HandleFunc("POST /api/v1/admin/devices/{id}/media", s.admin(s.handleUploadDeviceMedia, false))
}

// admin 校验管理口令（猜错多了按来源 IP 锁定，见 adminauth.go）；logWrites 为真时把写操作（非 GET）记进日志。
func (s *Server) admin(h http.HandlerFunc, logWrites bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ok, wait := s.auth.check(r.Header.Get("X-Admin-Token"), clientIP(r), s.now())
		switch {
		case wait > 0:
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			http.Error(w, "口令错误次数过多，请 "+waitText(wait)+"后再试", http.StatusTooManyRequests)
			return
		case !ok:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !logWrites || r.Method == http.MethodGet {
			h(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		log.Printf("admin %s %s -> %d (%s)", r.Method, pathPrefix(r.URL.Path), rec.status, time.Since(start).Round(time.Millisecond))
	}
}

// statusRecorder 记下处理器写出的状态码（日志用）。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// adminCSP：页面是单文件、用 onclick 属性，去不掉 'unsafe-inline'；但只许连自己、不许被别的网页嵌入——
// 万一有 XSS 也难把口令发到外部主机。
const adminCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' blob: data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", adminCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(web.AdminHTML)
}

// serverInfo 是服务端的能力与限制，后台据此提示（例如没装 ffmpeg 时不能上传视频）、拼装机命令。
type serverInfo struct {
	// 服务器时间（毫秒）：时段计划、测试屏到期、定时下发都按它算。后台拿它和浏览器时间比，
	// 差得多就提示——离线环境下服务器没有 NTP，时钟漂移不会有人察觉。
	ServerTime     int64  `json:"server_time"`
	Timezone       string `json:"timezone"`
	Transcode      bool   `json:"transcode"`
	FFmpegError    string `json:"ffmpeg_error,omitempty"`
	PDF            bool   `json:"pdf"`
	PDFError       string `json:"pdf_error,omitempty"`
	TLSFingerprint string `json:"tls_fingerprint"`
	BootstrapPort  string `json:"bootstrap_port"`
	Limits         struct {
		ImageMB  int `json:"image_mb"`
		VideoMB  int `json:"video_mb"`
		PDFMB    int `json:"pdf_mb"`
		PDFPages int `json:"pdf_pages"`
	} `json:"limits"`
	Video transcode.Spec `json:"video"` // 视频转码目标
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	info := serverInfo{
		ServerTime: s.now().UnixMilli(), Timezone: s.loc.String(),
		Transcode: s.tools.enc != nil, FFmpegError: s.tools.encErr,
		PDF: s.tools.pdf != nil, PDFError: s.tools.pdfErr,
		TLSFingerprint: s.certFP, Video: transcode.DefaultSpec(),
	}
	_, info.BootstrapPort, _ = net.SplitHostPort(s.cfg.BootstrapListen)
	info.Limits.ImageMB, info.Limits.VideoMB = maxImageUploadBytes>>20, maxVideoUploadBytes>>20
	info.Limits.PDFMB, info.Limits.PDFPages = maxPDFUploadBytes>>20, maxPDFPages
	writeJSON(w, info)
}

func (s *Server) pathDevice(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	dev, ok := s.store.Device(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
	}
	return dev, ok
}

// ---- 设备列表 / 删除 / 属性 / 测试 / 显示配置 ----

// DeviceStatus 是管理接口返回的设备状态。
type DeviceStatus struct {
	ID           string              `json:"id"`
	Online       bool                `json:"online"`
	LastSeen     *time.Time          `json:"last_seen,omitempty"` // 最近一次任何请求（轮询/心跳/下载）
	PollS        int                 `json:"poll_interval_s"`     // 设备多久该来一次
	OfflineS     int                 `json:"offline_after_s"`     // 多久没来算离线
	Heartbeat    *manifest.Heartbeat `json:"heartbeat,omitempty"`
	Attrs        map[string]string   `json:"attrs"`
	Display      store.DisplayConfig `json:"display"`
	TestUntil    *time.Time          `json:"test_until,omitempty"`
	ActiveSource string              `json:"active_source"` // test/override/schedule/global
	ActiveTpl    string              `json:"active_template,omitempty"`
	Sync         string              `json:"sync"` // offline/waiting/syncing/latest，见 syncState
	UpdateTarget *store.UpdateTarget `json:"update_target,omitempty"`
	HW           store.Device        `json:"hw"` // 注册信息（不含密钥）
}

func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	var statuses []DeviceStatus
	s.store.View(func(st *store.State) {
		for _, d := range sortedByID(st.Devices) {
			d.Secret = "" // 不向后台暴露密钥
			if d.Rekey != nil {
				rk := *d.Rekey
				rk.Secret = ""
				d.Rekey = &rk
			}
			ds := DeviceStatus{ID: d.ID, Display: st.Displays[d.ID], HW: d}
			if u, ok := st.Updates[d.ID]; ok {
				ds.UpdateTarget = &u
			}
			statuses = append(statuses, ds)
		}
	})
	keys := make([]string, len(statuses))
	for i := range statuses { // 读状态与文件元数据，放在 s.mu 外面
		ds := &statuses[i]
		c, _ := s.content(ds.ID, now)
		ds.Attrs, ds.ActiveSource, ds.ActiveTpl, keys[i] = c.Attrs, c.Source, c.Template.ID, c.key()
		if !c.TestUntil.IsZero() {
			ds.TestUntil, ds.ActiveTpl = &c.TestUntil, ""
		}
	}
	offline := s.offlineAfter()
	s.mu.Lock()
	for i := range statuses {
		ds := &statuses[i]
		ds.PollS, ds.OfflineS = s.cfg.PollIntervalS, int(offline/time.Second)
		if seen, ok := s.lastSeen[ds.ID]; ok {
			ds.LastSeen, ds.Online = &seen, now.Sub(seen) <= offline
		}
		if hb, ok := s.lastHB[ds.ID]; ok {
			ds.Heartbeat = &hb
		}
		ds.Sync = s.syncState(ds.ID, ds.Online, keys[i])
	}
	s.mu.Unlock()
	if statuses == nil {
		statuses = []DeviceStatus{}
	}
	writeJSON(w, statuses)
}

// handleDeleteDevice 删除设备及其属性、显示配置、更新目标与播放内容（设备仍在运行的话会重新注册）。
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	if !s.update(w, func(st *store.State) {
		delete(st.Devices, dev.ID)
		delete(st.DeviceAttrs, dev.ID)
		delete(st.Displays, dev.ID)
		delete(st.Updates, dev.ID)
		delete(st.TestUntil, dev.ID)
	}) {
		return
	}
	s.mu.Lock()
	delete(s.lastSeen, dev.ID)
	delete(s.lastHB, dev.ID)
	delete(s.sync, dev.ID)
	s.mu.Unlock()
	s.jobs.removeDevice(dev.ID)
	names, _ := manifest.ListMedia(s.deviceMediaDir(dev.ID))
	s.unlinkMedia(dev.ID, names...)
	os.RemoveAll(s.deviceMediaDir(dev.ID))
	os.RemoveAll(filepath.Join(s.renderedDir(), dev.ID))
	w.WriteHeader(http.StatusNoContent)
}

var attrKeyPattern = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,32}$`)

func (s *Server) handlePutAttrs(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var attrs map[string]string
	if !decodeJSON(w, r, 64<<10, &attrs) {
		return
	}
	for k, v := range attrs {
		if !attrKeyPattern.MatchString(k) || len(v) > 256 {
			http.Error(w, fmt.Sprintf("非法属性 %q", k), http.StatusBadRequest)
			return
		}
	}
	if s.update(w, func(st *store.State) { st.DeviceAttrs[dev.ID] = attrs }) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleTest 让设备全屏显示测试卡 duration_s 秒；duration_s <= 0 取消。顺手清掉已过期的测试记录。
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		DurationS int `json:"duration_s"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	now := s.now()
	if s.update(w, func(st *store.State) {
		for id, until := range st.TestUntil {
			if !now.Before(until) {
				delete(st.TestUntil, id)
			}
		}
		delete(st.TestUntil, dev.ID)
		if req.DurationS > 0 {
			st.TestUntil[dev.ID] = now.Add(time.Duration(req.DurationS) * time.Second)
		}
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePutDisplay 设置设备的专属模板（空 = 跟随全局）与左右对调。播放顺序归播放列表接口管。
func (s *Server) handlePutDisplay(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var d store.DisplayConfig
	if !decodeJSON(w, r, 64<<10, &d) {
		return
	}
	if err := store.ValidateDisplay(d, s.store.Template); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.update(w, func(st *store.State) {
		d.Playlist = st.Displays[dev.ID].Playlist // 整个替换会把排好的顺序冲掉
		st.Displays[dev.ID] = d
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- 全局模板 / 时段计划 ----

func (s *Server) handleGetGlobal(w http.ResponseWriter, r *http.Request) {
	var g store.GlobalConfig
	s.store.View(func(st *store.State) { g = st.Global })
	writeJSON(w, g)
}

func (s *Server) handlePutGlobal(w http.ResponseWriter, r *http.Request) {
	var g store.GlobalConfig
	if !decodeJSON(w, r, 4<<10, &g) {
		return
	}
	if _, ok := s.store.Template(g.TemplateID); !ok {
		http.Error(w, "模板不存在", http.StatusBadRequest)
		return
	}
	if s.update(w, func(st *store.State) { st.Global = g }) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleGetSchedules(w http.ResponseWriter, r *http.Request) {
	var list []store.Schedule
	s.store.View(func(st *store.State) { list = st.Schedules })
	writeJSON(w, list)
}

// handlePutSchedules 整表替换时段计划。
func (s *Server) handlePutSchedules(w http.ResponseWriter, r *http.Request) {
	list := []store.Schedule{}
	if !decodeJSON(w, r, 256<<10, &list) {
		return
	}
	if list == nil {
		list = []store.Schedule{}
	}
	if err := store.ValidateSchedules(list, s.store.Template); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.update(w, func(st *store.State) { st.Schedules = list }) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- 模板 ----

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	var out []store.Template
	s.store.View(func(st *store.State) { out = sortedByID(st.Templates) })
	writeJSON(w, out)
}

// handlePutTemplate 新建或修改模板。模板 ID 由服务端生成：它只是内部标识，
// 运营方不需要关心，管理后台也不显示——新建时（POST）自动分配。
func (s *Server) handlePutTemplate(w http.ResponseWriter, r *http.Request) {
	var t store.Template
	if !decodeJSON(w, r, 256<<10, &t) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		t.ID = id
	}
	if t.ID == "" {
		t.ID = store.NewTemplateID()
	}
	if err := store.ValidateTemplate(&t); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, f := range []string{t.BackgroundImage, t.BackgroundImageMirror} {
		if _, err := os.Stat(filepath.Join(s.backgroundsDir(), f)); f != "" && err != nil {
			http.Error(w, "底图 "+f+" 不存在（请在后台「底图」里上传）", http.StatusBadRequest)
			return
		}
	}
	if s.update(w, func(st *store.State) { st.Templates[t.ID] = t }) {
		s.gcBackgrounds()
		writeJSON(w, t)
	}
}

// handleDeleteTemplate 删除模板。
//
// 两条保护：仍被某台设备或某个时段直接引用的模板不能删（会让那些设备没有版式）；
// 最后一个模板也不能删（系统必须始终有一个全局默认模板可用）。
// 删掉的正好是全局默认模板时不拦——删除后自动改指向剩下的模板。
func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var inUse []string
	var last bool
	s.store.View(func(st *store.State) {
		for devID, d := range st.Displays {
			if d.TemplateID == id {
				inUse = append(inUse, "设备 "+devID)
			}
		}
		for _, sc := range st.Schedules {
			if sc.TemplateID == id {
				inUse = append(inUse, "时段 "+sc.ID)
			}
		}
		_, exists := st.Templates[id]
		last = exists && len(st.Templates) == 1
	})
	switch {
	case last:
		http.Error(w, "这是最后一个模板，不能删除（系统始终需要一个全局默认模板）", http.StatusConflict)
	case len(inUse) > 0:
		http.Error(w, "模板使用中: "+strings.Join(inUse, ", "), http.StatusConflict)
	case s.update(w, func(st *store.State) {
		delete(st.Templates, id)
		store.EnsureGlobalTemplate(st)
	}):
		s.gcBackgrounds()
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePreview 渲染模板预览图。
// ?device= 用某台设备的属性与左右对调设置；?mirror=1 单独预览对调后的版式。
// 预览始终出整屏图（媒体区填自己的底色），这样在后台里能直接看到版式。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var (
		tpl    store.Template
		ok     bool
		attrs  map[string]string
		mirror = r.URL.Query().Get("mirror") == "1"
	)
	s.store.View(func(st *store.State) {
		tpl, ok = st.Templates[r.PathValue("id")]
		if dev := r.URL.Query().Get("device"); dev != "" {
			attrs, mirror = maps.Clone(st.DeviceAttrs[dev]), mirror || st.Displays[dev].Mirror
		}
	})
	if !ok {
		http.Error(w, "unknown template", http.StatusNotFound)
		return
	}
	rendered, err := s.renderer.Render(tpl, attrs, mirror, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	if err := png.Encode(w, rendered.Image); err != nil {
		log.Printf("writing preview failed: %v", err)
	}
}
