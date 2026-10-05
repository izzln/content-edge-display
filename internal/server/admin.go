package server

import (
	"cmp"
	"fmt"
	"image"
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
	"github.com/izzln/content-edge-display/internal/render"
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
		"POST /api/v1/admin/devices/{id}/media":             s.handleUploadDeviceMedia,
	} {
		mux.HandleFunc(route, s.admin(h))
	}
}

// admin 校验管理口令（猜错多了按来源 IP 锁定，见 adminauth.go），并把写操作（非 GET）记进日志。
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
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
			http.Error(w, "口令不对", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
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
		writeError(w, errNotFound("设备"))
	}
	return dev, ok
}

// ---- 设备列表 / 删除 / 属性 / 测试 / 显示配置 ----

// DeviceView 是后台看到的一台设备：注册信息与设置（不含密钥）+ 运行状态。
type DeviceView struct {
	ID           string    `json:"id"`
	RegisteredAt time.Time `json:"registered_at"`
	store.Hardware
	Rekey        *rekeyView          `json:"rekey,omitempty"`
	Attrs        map[string]string   `json:"attrs"`
	Display      store.DisplayConfig `json:"display"`
	UpdateTarget *store.UpdateTarget `json:"update_target,omitempty"`

	Online       bool                `json:"online"`
	LastSeen     *time.Time          `json:"last_seen,omitempty"` // 最近一次任何请求（轮询/心跳/下载）
	PollS        int                 `json:"poll_interval_s"`     // 设备多久该来一次
	OfflineS     int                 `json:"offline_after_s"`     // 多久没来算离线
	Heartbeat    *manifest.Heartbeat `json:"heartbeat,omitempty"`
	TestUntil    *time.Time          `json:"test_until,omitempty"`
	ActiveSource string              `json:"active_source"` // test/override/schedule/global
	ActiveTpl    string              `json:"active_template,omitempty"`
	Sync         string              `json:"sync"` // offline/waiting/syncing/latest，见 syncState
}

// rekeyView 是待确认的换密钥请求（不含新密钥本身）。
type rekeyView struct {
	Fingerprint string    `json:"fingerprint"`
	At          time.Time `json:"at"`
	store.Hardware
}

func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	views := []DeviceView{}
	s.store.View(func(st *store.State) {
		for _, d := range sortedByID(st.Devices) {
			v := DeviceView{ID: d.ID, RegisteredAt: d.RegisteredAt, Hardware: d.Hardware,
				Attrs: maps.Clone(d.Attrs), Display: d.Display, UpdateTarget: d.Update}
			if rk := d.Rekey; rk != nil {
				v.Rekey = &rekeyView{Fingerprint: rk.Fingerprint, At: rk.At, Hardware: rk.Hardware}
			}
			views = append(views, v)
		}
	})
	keys := make([]string, len(views))
	for i := range views { // 读状态与文件元数据，放在 s.mu 外面
		v := &views[i]
		c, _ := s.content(v.ID, now)
		v.ActiveSource, v.ActiveTpl, keys[i] = c.Source, c.Template.ID, c.key()
		if !c.TestUntil.IsZero() {
			v.TestUntil, v.ActiveTpl = &c.TestUntil, ""
		}
	}
	offline := s.offlineAfter()
	s.mu.Lock()
	for i := range views {
		v, rt := &views[i], s.devices[views[i].ID]
		v.PollS, v.OfflineS = s.cfg.PollIntervalS, int(offline/time.Second)
		if rt != nil && !rt.lastSeen.IsZero() {
			seen := rt.lastSeen
			v.LastSeen, v.Online, v.Heartbeat = &seen, now.Sub(seen) <= offline, rt.hb
		}
		v.Sync = s.syncState(rt, v.Online, keys[i])
	}
	s.mu.Unlock()
	writeJSON(w, views)
}

// handleDeleteDevice 删除设备及其全部设置与播放内容（设备仍在运行的话会重新注册成一台新设备）。
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.update(w, func(st *store.State) error {
		if st.Devices[id] == nil {
			return errNotFound("设备")
		}
		delete(st.Devices, id)
		return nil
	}) {
		return
	}
	s.mu.Lock()
	delete(s.devices, id)
	s.mu.Unlock()
	s.jobs.removeDevice(id)
	files, _ := manifest.ListMedia(s.deviceMediaDir(id))
	s.unlinkMedia(id, manifest.Names(files)...)
	os.RemoveAll(s.deviceMediaDir(id))
	os.RemoveAll(filepath.Join(s.renderedDir(), id))
	w.WriteHeader(http.StatusNoContent)
}

var attrKeyPattern = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,32}$`)

func (s *Server) handlePutAttrs(w http.ResponseWriter, r *http.Request) {
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
	if s.editDevice(w, r.PathValue("id"), func(d *store.Device) error { d.Attrs = attrs; return nil }) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleTest 让设备全屏显示测试卡 duration_s 秒；duration_s <= 0 取消。
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DurationS int `json:"duration_s"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	var until time.Time
	if req.DurationS > 0 {
		until = s.now().Add(time.Duration(req.DurationS) * time.Second)
	}
	if s.editDevice(w, r.PathValue("id"), func(d *store.Device) error { d.TestUntil = until; return nil }) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePutDisplay 设置设备的专属模板（空 = 跟随全局）与左右对调。播放顺序归播放列表接口管。
func (s *Server) handlePutDisplay(w http.ResponseWriter, r *http.Request) {
	var req store.DisplayConfig
	if !decodeJSON(w, r, 64<<10, &req) {
		return
	}
	id := r.PathValue("id")
	if s.update(w, func(st *store.State) error {
		d, ok := st.Devices[id]
		switch {
		case !ok:
			return errNotFound("设备")
		case req.TemplateID != "" && st.Templates[req.TemplateID].ID == "":
			return errBadRequest("模板 %q 不存在", req.TemplateID)
		}
		req.Playlist = d.Display.Playlist // 整个替换会把排好的顺序冲掉
		d.Display = req
		return nil
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
	if s.update(w, func(st *store.State) error {
		if _, ok := st.Templates[g.TemplateID]; !ok {
			return errBadRequest("模板不存在")
		}
		st.Global = g
		return nil
	}) {
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
	if s.update(w, func(st *store.State) error {
		if err := store.ValidateSchedules(list, st.Templates); err != nil {
			return errBadRequest("%v", err)
		}
		st.Schedules = list
		return nil
	}) {
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
// 底图只由底图接口改：修改时保留原来的，新建（包括复制别的模板）时没有底图。
func (s *Server) handlePutTemplate(w http.ResponseWriter, r *http.Request) {
	var t store.Template
	if !decodeJSON(w, r, 256<<10, &t) {
		return
	}
	t.ID = cmp.Or(r.PathValue("id"), t.ID, store.NewTemplateID())
	if s.update(w, func(st *store.State) error {
		old := st.Templates[t.ID]
		t.BackgroundImage, t.BackgroundImageMirror = old.BackgroundImage, old.BackgroundImageMirror
		if err := store.ValidateTemplate(&t); err != nil {
			return errBadRequest("%v", err)
		}
		st.Templates[t.ID] = t
		return nil
	}) {
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
	if s.update(w, func(st *store.State) error {
		var inUse []string
		for _, d := range sortedByID(st.Devices) {
			if d.Display.TemplateID == id {
				inUse = append(inUse, "设备 "+d.ID)
			}
		}
		for _, sc := range st.Schedules {
			if sc.TemplateID == id {
				inUse = append(inUse, "时段 "+sc.ID)
			}
		}
		switch _, ok := st.Templates[id]; {
		case !ok:
			return errNotFound("模板")
		case len(st.Templates) == 1:
			return errConflict("这是最后一个模板，不能删除（系统始终需要一个全局默认模板）")
		case len(inUse) > 0:
			return errConflict("模板使用中: %s", strings.Join(inUse, ", "))
		}
		delete(st.Templates, id)
		store.EnsureGlobalTemplate(st)
		return nil
	}) {
		s.kickCache() // 维护协程回收它的底图
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePreview 渲染模板预览图。?mirror=1 单独预览对调后的版式。
// ?device= 用某台设备的属性、左右对调与播放列表：把播放列表里 ?item=（默认第一项）的一帧按设备的方式铺进媒体区，
// 再盖上模板，与设备上看到的一致（含压在媒体区上的底图装饰）。没有播放内容时出整屏图（媒体区填自己的底色）。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var (
		tpl    store.Template
		ok     bool
		attrs  map[string]string
		disp   store.DisplayConfig
		dev    = q.Get("device")
		mirror = q.Get("mirror") == "1"
	)
	s.store.View(func(st *store.State) {
		tpl, ok = st.Templates[r.PathValue("id")]
		if d := st.Devices[dev]; d != nil {
			attrs, disp = maps.Clone(d.Attrs), d.Display
			mirror = mirror || disp.Mirror
		}
	})
	if !ok {
		writeError(w, errNotFound("模板"))
		return
	}
	var still image.Image
	media, hasMedia := tpl.MediaRect(mirror)
	if hasMedia && dev != "" {
		still = s.stillImage(r.Context(), dev, disp, q.Get("item"))
	}
	img, err := s.renderer.Render(tpl, attrs, mirror, still != nil)
	if err != nil {
		writeError(w, err)
		return
	}
	if still != nil {
		img = render.Preview(img, media, still)
	}
	w.Header().Set("Content-Type", "image/png")
	if err := png.Encode(w, img); err != nil {
		log.Printf("writing preview failed: %v", err)
	}
}
