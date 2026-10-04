package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
	"github.com/izzln/content-edge-display/internal/web"
)

// registerAdmin 挂载管理后台 UI 与管理 API。
// 读接口在 admin_token 未配置时开放（便于起步）；写接口必须配置 token 才可用。
func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.handleAdminUI)
	mux.HandleFunc("GET /admin/{$}", s.handleAdminUI)

	mux.HandleFunc("GET /api/v1/admin/info", s.adminRead(s.handleInfo))
	mux.HandleFunc("GET /api/v1/admin/devices", s.adminRead(s.handleAdminDevices))
	mux.HandleFunc("DELETE /api/v1/admin/devices/{id}", s.adminWrite(s.handleDeleteDevice))
	mux.HandleFunc("PUT /api/v1/admin/devices/{id}/attributes", s.adminWrite(s.handlePutAttrs))
	mux.HandleFunc("POST /api/v1/admin/devices/{id}/test", s.adminWrite(s.handleTest))
	mux.HandleFunc("POST /api/v1/admin/devices/{id}/rekey", s.adminWrite(s.handleRekey))
	mux.HandleFunc("PUT /api/v1/admin/devices/{id}/display", s.adminWrite(s.handlePutDisplay))
	mux.HandleFunc("GET /api/v1/admin/devices/{id}/media", s.adminRead(s.handleListDeviceMedia))
	mux.HandleFunc("POST /api/v1/admin/devices/{id}/media", s.adminWrite(s.handleUploadDeviceMedia))
	mux.HandleFunc("PUT /api/v1/admin/devices/{id}/media", s.adminWrite(s.handleReorderDeviceMedia))
	mux.HandleFunc("GET /api/v1/admin/devices/{id}/media/{file}/thumb", s.adminRead(s.handleDeviceMediaThumb))
	mux.HandleFunc("DELETE /api/v1/admin/devices/{id}/media/{file}", s.adminWrite(s.handleDeleteDeviceMedia))
	mux.HandleFunc("GET /api/v1/admin/templates", s.adminRead(s.handleListTemplates))
	mux.HandleFunc("POST /api/v1/admin/templates", s.adminWrite(s.handlePutTemplate))
	mux.HandleFunc("PUT /api/v1/admin/templates/{id}", s.adminWrite(s.handlePutTemplate))
	mux.HandleFunc("DELETE /api/v1/admin/templates/{id}", s.adminWrite(s.handleDeleteTemplate))
	mux.HandleFunc("GET /api/v1/admin/templates/{id}/preview", s.adminRead(s.handlePreview))
	mux.HandleFunc("GET /api/v1/admin/global", s.adminRead(s.handleGetGlobal))
	mux.HandleFunc("PUT /api/v1/admin/global", s.adminWrite(s.handlePutGlobal))
	mux.HandleFunc("GET /api/v1/admin/schedules", s.adminRead(s.handleGetSchedules))
	mux.HandleFunc("PUT /api/v1/admin/schedules", s.adminWrite(s.handlePutSchedules))
	mux.HandleFunc("GET /api/v1/admin/firmware", s.adminRead(s.handleListFirmware))
	mux.HandleFunc("POST /api/v1/admin/firmware", s.adminWrite(s.handleUploadFirmware))
	mux.HandleFunc("DELETE /api/v1/admin/firmware/{version}", s.adminWrite(s.handleDeleteFirmware))
	mux.HandleFunc("PUT /api/v1/admin/rollout", s.adminWrite(s.handleRollout))
}

// handleInfo 返回服务端能力，后台据此提示（例如没装 ffmpeg 时不能上传视频）。
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	spec := transcode.DefaultSpec()
	info := map[string]any{
		// 服务器时间：时段计划、测试屏到期、定时下发都按它算。后台拿它和浏览器时间比，
		// 差得多就提示——离线环境下服务器没有 NTP，时钟漂移不会有人察觉。
		"server_time": s.now().UnixMilli(),
		"timezone":    s.loc.String(),
		"transcode":   false,
		"video_spec": map[string]int{
			"max_w": spec.MaxW, "max_h": spec.MaxH, "max_fps": spec.MaxFPS,
			"crf": spec.CRF, "max_bitrate_k": spec.MaxBitrateK,
		},
	}
	if enc := s.videoEncoder(); enc != nil {
		info["transcode"], info["ffmpeg"] = true, enc.Version()
	} else {
		s.encMu.Lock()
		info["ffmpeg_error"] = s.encoderErr
		s.encMu.Unlock()
	}
	writeJSON(w, info)
}

func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(web.AdminHTML)
}

func (s *Server) adminAuthed(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Token")), []byte(s.cfg.AdminToken)) == 1
}

func (s *Server) adminRead(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) adminWrite(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 写接口在未配置 admin_token 时一律拒绝，防止管理面裸奔。
		if s.cfg.AdminToken == "" {
			http.Error(w, "admin_token not configured", http.StatusForbidden)
			return
		}
		if !s.adminAuthed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// 上传接口自己逐个文件记日志，这里不重复
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/media") {
			h(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		log.Printf("admin %s %s -> %d (%s)", r.Method, strings.TrimPrefix(r.URL.Path, "/api/v1/admin"),
			rec.status, time.Since(start).Round(time.Millisecond))
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing JSON response failed: %v", err)
	}
}

// decodeJSON 读取至多 limit 字节的 JSON 请求体；格式不对时回 400 并返回 false。
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return false
	}
	return true
}

// update 修改并持久化状态；写盘失败时回 500 并返回 false。
func (s *Server) update(w http.ResponseWriter, fn func(*store.State)) bool {
	if err := s.store.Update(func(st *store.State) error { fn(st); return nil }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	return true
}

// sortedValues 返回 map 的全部值并排序；空 map 返回 []（JSON 输出 [] 而不是 null）。
func sortedValues[K comparable, V any](m map[K]V, cmp func(a, b V) int) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	slices.SortFunc(out, cmp)
	return out
}

func (s *Server) pathDevice(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	dev, ok := s.store.Device(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
	}
	return dev, ok
}

// ---- 设备列表 / 删除 / 属性 / 测试 / 显示配置 / 播放列表 ----

func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	devices := s.allDevices()
	statuses := make([]DeviceStatus, 0, len(devices))
	for _, d := range devices {
		c, _ := s.content(d.ID, now) // 读状态与文件元数据，放在 s.mu 外面
		st := DeviceStatus{
			ID:           d.ID,
			Attrs:        c.Attrs,
			Display:      s.store.Display(d.ID),
			ActiveSource: c.Source,
			ActiveTpl:    c.Template.ID,
		}
		if !c.TestUntil.IsZero() {
			st.TestUntil, st.ActiveTpl = &c.TestUntil, ""
		}
		s.mu.Lock()
		offline := s.offlineAfter()
		st.PollS, st.OfflineS = s.cfg.PollIntervalS, int(offline/time.Second)
		if seen, ok := s.lastSeen[d.ID]; ok {
			st.LastSeen = &seen
			st.Online = now.Sub(seen) <= offline
		}
		if hb, ok := s.lastHB[d.ID]; ok {
			st.Heartbeat = &hb
		}
		st.Sync = s.syncState(d.ID, st.Online, c.key())
		s.mu.Unlock()
		if u, ok := s.store.UpdateTarget(d.ID); ok {
			st.UpdateTarget = &u
		}
		d.Secret = "" // 不向后台暴露密钥
		if d.Rekey != nil {
			rk := *d.Rekey
			rk.Secret = ""
			d.Rekey = &rk
		}
		st.HW = &d
		statuses = append(statuses, st)
	}
	writeJSON(w, statuses)
}

// handleDeleteDevice 删除自注册设备及其属性/显示配置/更新目标（设备可重新注册）。
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
	os.RemoveAll(filepath.Join(s.renderedDir(), dev.ID))
	os.RemoveAll(s.deviceMediaDir(dev.ID))
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
		writeJSON(w, attrs)
	}
}

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
	until := time.Time{} // duration<=0 表示取消测试
	if req.DurationS > 0 {
		until = s.now().Add(time.Duration(req.DurationS) * time.Second)
	}
	if s.update(w, func(st *store.State) {
		if until.IsZero() {
			delete(st.TestUntil, dev.ID)
		} else {
			st.TestUntil[dev.ID] = until
		}
	}) {
		writeJSON(w, map[string]any{"test_until": until})
	}
}

func (s *Server) handlePutDisplay(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var d store.DisplayConfig
	if !decodeJSON(w, r, 64<<10, &d) {
		return
	}
	if err := store.ValidateDisplay(&d, s.store.Template); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.update(w, func(st *store.State) {
		// 播放顺序归播放列表接口管（上传/拖动排序），这里只改模板与左右对调。
		// 整个替换的话，后台"保存模板设置"会把排好的顺序冲掉，退回按文件名排。
		d.Playlist = st.Displays[dev.ID].Playlist
		st.Displays[dev.ID] = d
	}) {
		writeJSON(w, d)
	}
}

// ---- 全局模板 / 时段计划 ----

func (s *Server) handleGetGlobal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.Global())
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
		writeJSON(w, g)
	}
}

func (s *Server) handleGetSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.Schedules())
}

// handlePutSchedules 整表替换时段计划。
func (s *Server) handlePutSchedules(w http.ResponseWriter, r *http.Request) {
	var list []store.Schedule
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
		writeJSON(w, list)
	}
}

// ---- 模板 ----

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	var out []store.Template
	s.store.View(func(st *store.State) {
		out = sortedValues(st.Templates, func(a, b store.Template) int { return strings.Compare(a.ID, b.ID) })
	})
	writeJSON(w, out)
}

// handlePutTemplate 新建或修改模板。模板 ID 由服务端生成：它只是内部标识，
// 运营方不需要关心，管理后台也不显示——新建时（POST 且未带 ID）自动分配。
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
	if s.update(w, func(st *store.State) { st.Templates[t.ID] = t }) {
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
	var (
		inUse []string
		total int
		last  bool
	)
	s.store.View(func(st *store.State) {
		for devID, d := range st.Displays {
			if d.Mode == store.ModeTemplate && d.TemplateID == id {
				inUse = append(inUse, "设备 "+devID)
			}
		}
		for _, sc := range st.Schedules {
			if sc.TemplateID == id {
				inUse = append(inUse, "时段 "+sc.ID)
			}
		}
		total = len(st.Templates)
		_, exists := st.Templates[id]
		last = exists && total == 1
	})
	if last {
		http.Error(w, "这是最后一个模板，不能删除（系统始终需要一个全局默认模板）", http.StatusConflict)
		return
	}
	if len(inUse) > 0 {
		http.Error(w, fmt.Sprintf("模板使用中: %s", strings.Join(inUse, ", ")), http.StatusConflict)
		return
	}
	var newGlobal string
	if s.update(w, func(st *store.State) {
		delete(st.Templates, id)
		store.EnsureGlobalTemplate(st)
		newGlobal = st.Global.TemplateID
	}) {
		writeJSON(w, map[string]string{"global_template_id": newGlobal})
	}
}

// handlePreview 渲染模板预览图。
// ?device= 用某台设备的属性与左右对调设置；?mirror=1 单独预览对调后的版式。
// 预览始终出整屏图（媒体区填自己的底色），这样在后台里能直接看到版式。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	tpl, ok := s.store.Template(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown template", http.StatusNotFound)
		return
	}
	attrs := map[string]string{}
	mirror := r.URL.Query().Get("mirror") == "1"
	if devID := r.URL.Query().Get("device"); devID != "" {
		attrs = s.store.Attrs(devID)
		mirror = mirror || s.store.Display(devID).Mirror
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
