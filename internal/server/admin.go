package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/render"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/web"
)

// registerAdmin 挂载管理后台 UI 与管理 API。
// 读接口在 admin_token 未配置时开放（便于起步）；写接口必须配置 token 才可用。
func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.handleAdminUI)
	mux.HandleFunc("GET /admin/{$}", s.handleAdminUI)

	mux.HandleFunc("GET /api/v1/admin/devices", s.adminRead(s.handleAdminDevices))
	mux.HandleFunc("PUT /api/v1/admin/devices/{id}/name", s.adminWrite(s.handleRenameDevice))
	mux.HandleFunc("DELETE /api/v1/admin/devices/{id}", s.adminWrite(s.handleDeleteDevice))
	mux.HandleFunc("GET /api/v1/admin/devices/{id}/attributes", s.adminRead(s.handleGetAttrs))
	mux.HandleFunc("PUT /api/v1/admin/devices/{id}/attributes", s.adminWrite(s.handlePutAttrs))
	mux.HandleFunc("POST /api/v1/admin/devices/{id}/test", s.adminWrite(s.handleTest))
	mux.HandleFunc("PUT /api/v1/admin/devices/{id}/display", s.adminWrite(s.handlePutDisplay))
	mux.HandleFunc("GET /api/v1/admin/templates", s.adminRead(s.handleListTemplates))
	mux.HandleFunc("POST /api/v1/admin/templates", s.adminWrite(s.handlePutTemplate))
	mux.HandleFunc("PUT /api/v1/admin/templates/{id}", s.adminWrite(s.handlePutTemplate))
	mux.HandleFunc("DELETE /api/v1/admin/templates/{id}", s.adminWrite(s.handleDeleteTemplate))
	mux.HandleFunc("GET /api/v1/admin/templates/{id}/preview", s.adminRead(s.handlePreview))
	mux.HandleFunc("GET /api/v1/admin/global", s.adminRead(s.handleGetGlobal))
	mux.HandleFunc("PUT /api/v1/admin/global", s.adminWrite(s.handlePutGlobal))
	mux.HandleFunc("GET /api/v1/admin/schedules", s.adminRead(s.handleGetSchedules))
	mux.HandleFunc("PUT /api/v1/admin/schedules", s.adminWrite(s.handlePutSchedules))
	mux.HandleFunc("GET /api/v1/admin/uploads", s.adminRead(s.handleListUploads))
	mux.HandleFunc("POST /api/v1/admin/uploads", s.adminWrite(s.handleUpload))
	mux.HandleFunc("GET /api/v1/admin/firmware", s.adminRead(s.handleListFirmware))
	mux.HandleFunc("POST /api/v1/admin/firmware", s.adminWrite(s.handleUploadFirmware))
	mux.HandleFunc("DELETE /api/v1/admin/firmware/{version}", s.adminWrite(s.handleDeleteFirmware))
	mux.HandleFunc("PUT /api/v1/admin/rollout", s.adminWrite(s.handleRollout))
}

func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(web.AdminHTML)
}

func (s *Server) adminAuthed(r *http.Request) bool {
	return s.cfg.AdminToken == "" || r.Header.Get("X-Admin-Token") == s.cfg.AdminToken
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
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("admin: encode response failed: %v", err)
	}
}

func (s *Server) pathDevice(w http.ResponseWriter, r *http.Request) (DeviceConfig, bool) {
	dev, ok := s.deviceByID(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
	}
	return dev, ok
}

// ---- 设备列表 / 改名 / 删除 / 属性 / 测试 / 显示配置 ----

func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	devices := s.allDevices()
	statuses := make([]DeviceStatus, 0, len(devices))
	s.mu.Lock()
	for _, d := range devices {
		_, static := s.devices[d.ID]
		st := DeviceStatus{ID: d.ID, Name: d.Name, Registered: !static}
		if seen, ok := s.lastSeen[d.ID]; ok {
			seenCopy := seen
			st.LastSeen = &seenCopy
			st.Online = now.Sub(seen) <= OnlineWindow
			hb := s.lastHB[d.ID]
			st.Heartbeat = &hb
		}
		statuses = append(statuses, st)
	}
	s.mu.Unlock()
	for i := range statuses {
		id := statuses[i].ID
		statuses[i].Attrs = s.store.Attrs(id)
		statuses[i].Display = s.store.Display(id)
		statuses[i].AgentVersion = s.agentVersion(id)
		if until := s.store.TestUntil(id); now.Before(until) {
			u := until
			statuses[i].TestUntil = &u
			statuses[i].ActiveSource = "test"
		} else if tpl, source, ok := s.resolveTemplate(id, now); ok {
			statuses[i].ActiveSource, statuses[i].ActiveTpl = source, tpl.ID
		} else {
			statuses[i].ActiveSource = "playlist"
		}
		if u, ok := s.store.UpdateTarget(id); ok {
			statuses[i].UpdateTarget = &u
		}
		if d, ok := s.store.Device(id); ok {
			d.Secret = "" // 不向后台暴露密钥
			statuses[i].HW = &d
		}
	}
	writeJSON(w, statuses)
}

func (s *Server) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, static := s.devices[dev.ID]; static {
		http.Error(w, "静态配置设备请在 server.json 中改名", http.StatusConflict)
		return
	}
	err := s.store.Update(func(st *store.State) error {
		d := st.Devices[dev.ID]
		d.Name = strings.TrimSpace(req.Name)
		st.Devices[dev.ID] = d
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteDevice 删除自注册设备及其属性/显示配置/更新目标（设备可重新注册）。
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	if _, static := s.devices[dev.ID]; static {
		http.Error(w, "静态配置设备请在 server.json 中移除", http.StatusConflict)
		return
	}
	err := s.store.Update(func(st *store.State) error {
		delete(st.Devices, dev.ID)
		delete(st.DeviceAttrs, dev.ID)
		delete(st.Displays, dev.ID)
		delete(st.Updates, dev.ID)
		delete(st.TestUntil, dev.ID)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	delete(s.lastSeen, dev.ID)
	delete(s.lastHB, dev.ID)
	s.mu.Unlock()
	os.RemoveAll(filepath.Join(s.renderedDir(), dev.ID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetAttrs(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	writeJSON(w, s.store.Attrs(dev.ID))
}

var attrKeyPattern = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,32}$`)

func (s *Server) handlePutAttrs(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var attrs map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&attrs); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	for k, v := range attrs {
		if !attrKeyPattern.MatchString(k) || len(v) > 256 {
			http.Error(w, fmt.Sprintf("非法属性 %q", k), http.StatusBadRequest)
			return
		}
	}
	err := s.store.Update(func(st *store.State) error {
		st.DeviceAttrs[dev.ID] = attrs
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, attrs)
}

func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		DurationS int `json:"duration_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	until := time.Time{} // duration<=0 表示取消测试
	if req.DurationS > 0 {
		until = s.now().Add(time.Duration(req.DurationS) * time.Second)
	}
	err := s.store.Update(func(st *store.State) error {
		if until.IsZero() {
			delete(st.TestUntil, dev.ID)
		} else {
			st.TestUntil[dev.ID] = until
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"test_until": until})
}

func (s *Server) handlePutDisplay(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var d store.DisplayConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&d); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := store.ValidateDisplay(&d, s.store.Template); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.store.Update(func(st *store.State) error {
		st.Displays[dev.ID] = d
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, d)
}

// ---- 全局模板 / 时段计划 ----

func (s *Server) handleGetGlobal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.Global())
}

func (s *Server) handlePutGlobal(w http.ResponseWriter, r *http.Request) {
	var g store.GlobalConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&g); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if g.TemplateID != "" {
		if _, ok := s.store.Template(g.TemplateID); !ok {
			http.Error(w, "模板不存在", http.StatusBadRequest)
			return
		}
	}
	if err := s.store.Update(func(st *store.State) error { st.Global = g; return nil }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, g)
}

func (s *Server) handleGetSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.Schedules())
}

// handlePutSchedules 整表替换时段计划。
func (s *Server) handlePutSchedules(w http.ResponseWriter, r *http.Request) {
	var list []store.Schedule
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&list); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if list == nil {
		list = []store.Schedule{}
	}
	if err := store.ValidateSchedules(list, s.store.Template); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.Update(func(st *store.State) error { st.Schedules = list; return nil }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

// ---- 模板 ----

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	var out []store.Template
	s.store.View(func(st *store.State) {
		for _, t := range st.Templates {
			out = append(out, t)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if out == nil {
		out = []store.Template{}
	}
	writeJSON(w, out)
}

func (s *Server) handlePutTemplate(w http.ResponseWriter, r *http.Request) {
	var t store.Template
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&t); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if id := r.PathValue("id"); id != "" {
		t.ID = id
	}
	if err := store.ValidateTemplate(&t); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.store.Update(func(st *store.State) error {
		st.Templates[t.ID] = t
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 拒绝删除仍被设备/全局/时段引用的模板。
	var inUse []string
	s.store.View(func(st *store.State) {
		for devID, d := range st.Displays {
			if d.Mode == "template" && d.TemplateID == id {
				inUse = append(inUse, "设备 "+devID)
			}
		}
		if st.Global.TemplateID == id {
			inUse = append(inUse, "全局默认模板")
		}
		for _, sc := range st.Schedules {
			if sc.TemplateID == id {
				inUse = append(inUse, "时段 "+sc.ID)
			}
		}
	})
	if len(inUse) > 0 {
		http.Error(w, fmt.Sprintf("模板使用中: %s", strings.Join(inUse, ", ")), http.StatusConflict)
		return
	}
	err := s.store.Update(func(st *store.State) error {
		delete(st.Templates, id)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePreview 渲染模板预览图（可选 ?device= 用某台设备的属性与绑定）。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	tpl, ok := s.store.Template(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown template", http.StatusNotFound)
		return
	}
	attrs := map[string]string{}
	bindings := map[string]string{}
	if devID := r.URL.Query().Get("device"); devID != "" {
		attrs = s.store.Attrs(devID)
		if d := s.store.Display(devID); d.TemplateID == tpl.ID {
			bindings = d.Bindings
		}
	}
	img, err := s.renderer.Render(tpl, attrs, bindings)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	if err := render.EncodePNG(w, img); err != nil {
		log.Printf("preview encode failed: %v", err)
	}
}

// ---- 上传 ----

var uploadNamePattern = regexp.MustCompile(`^[\p{L}\p{N}_.-]{1,128}\.(?i:png|jpe?g)$`)

func (s *Server) handleListUploads(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.uploadsDir())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	writeJSON(w, names)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad multipart body", http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer f.Close()
	name := filepath.Base(hdr.Filename)
	if !uploadNamePattern.MatchString(name) {
		http.Error(w, "文件名非法（仅支持 png/jpg，字母数字._-）", http.StatusBadRequest)
		return
	}
	dst := filepath.Join(s.uploadsDir(), name)
	out, err := os.Create(dst + ".tmp")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(f, 20<<20)); err != nil {
		out.Close()
		os.Remove(dst + ".tmp")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Close()
	if err := os.Rename(dst+".tmp", dst); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"name": name})
}
