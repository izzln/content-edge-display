// Package server 实现服务端 HTTP API：设备清单下发、媒体分发、心跳与管理查询。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/render"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
)

// OnlineWindow 内有心跳视为设备在线。
const OnlineWindow = 5 * time.Minute

// DeviceConfig 是一台显示屏设备的注册信息。
type DeviceConfig struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
	Name   string `json:"name"`
}

// Config 是服务端配置（JSON 文件）。
type Config struct {
	Listen         string         `json:"listen"`
	MediaRoot      string         `json:"media_root"`
	DataDir        string         `json:"data_dir"`  // state.json / uploads / rendered / firmware
	FontPath       string         `json:"font_path"` // 模板渲染字体（生产需 CJK 字体）
	AdminToken     string         `json:"admin_token"`
	EnrollToken    string         `json:"enroll_token"` // 设备自注册口令（烧进母镜像）
	Timezone       string         `json:"timezone"`     // 时段计划时区，默认系统时区
	ImageDurationS int            `json:"image_duration_s"`
	Devices        []DeviceConfig `json:"devices"` // 静态配置设备（可选，自注册设备在 state.json）
}

// LoadConfig 读取配置文件并填充默认值。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.MediaRoot == "" {
		cfg.MediaRoot = "./data/media"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.ImageDurationS <= 0 {
		cfg.ImageDurationS = 10
	}
	if len(cfg.Devices) == 0 && cfg.EnrollToken == "" {
		return nil, errors.New("config: 需要配置 devices 或 enroll_token（否则没有任何设备能接入）")
	}
	for _, d := range cfg.Devices {
		if d.ID == "" || d.Secret == "" {
			return nil, errors.New("config: device id/secret must not be empty")
		}
	}
	return &cfg, nil
}

// Heartbeat 是设备心跳上报体。
type Heartbeat struct {
	Version    string `json:"version"`
	UptimeS    int64  `json:"uptime"`
	DiskFreeMB int64  `json:"disk_free_mb"`
	TempC      int    `json:"temp_c,omitempty"`
	Playing    string `json:"playing"`
	PlayerVer  string `json:"player_ver"`
	IP         string `json:"ip,omitempty"`
}

// DeviceStatus 是管理接口返回的设备状态。
type DeviceStatus struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Registered   bool                `json:"registered"` // 自注册（可删除）还是静态配置
	Online       bool                `json:"online"`
	LastSeen     *time.Time          `json:"last_seen,omitempty"`
	Heartbeat    *Heartbeat          `json:"heartbeat,omitempty"`
	Attrs        map[string]string   `json:"attrs"`
	Display      store.DisplayConfig `json:"display"`
	TestUntil    *time.Time          `json:"test_until,omitempty"`
	ActiveSource string              `json:"active_source"` // test/override/schedule/global/playlist
	ActiveTpl    string              `json:"active_template,omitempty"`
	AgentVersion string              `json:"agent_version,omitempty"`
	UpdateTarget *store.UpdateTarget `json:"update_target,omitempty"`
	HW           *store.Device       `json:"hw,omitempty"`
}

// Server 持有配置与运行期状态。
type Server struct {
	cfg      *Config
	devices  map[string]DeviceConfig // 静态配置设备
	hashes   *manifest.HashCache
	store    *store.Store
	renderer *render.Renderer
	loc      *time.Location

	mu       sync.Mutex
	lastSeen map[string]time.Time
	lastHB   map[string]Heartbeat

	now func() time.Time // 测试注入
}

// New 创建服务端：加载状态文件、准备数据目录、初始化渲染器。
func New(cfg *Config) (*Server, error) {
	st, err := store.Open(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		return nil, err
	}
	loc := time.Local
	if cfg.Timezone != "" {
		if loc, err = time.LoadLocation(cfg.Timezone); err != nil {
			return nil, fmt.Errorf("timezone: %w", err)
		}
	}
	s := &Server{
		cfg:      cfg,
		devices:  make(map[string]DeviceConfig),
		hashes:   manifest.NewHashCache(),
		store:    st,
		loc:      loc,
		lastSeen: make(map[string]time.Time),
		lastHB:   make(map[string]Heartbeat),
		now:      time.Now,
	}
	for _, dir := range []string{s.uploadsDir(), s.renderedDir(), s.firmwareDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	s.renderer, err = render.New(cfg.FontPath, s.uploadsDir())
	if err != nil {
		return nil, err
	}
	if cfg.FontPath == "" {
		log.Printf("warning: font_path 未配置，模板/测试卡中的中文将无法正常显示（请安装 CJK 字体并配置，如 fonts-noto-cjk）")
	}
	for _, d := range cfg.Devices {
		s.devices[d.ID] = d
	}
	return s, nil
}

func (s *Server) uploadsDir() string  { return filepath.Join(s.cfg.DataDir, "uploads") }
func (s *Server) renderedDir() string { return filepath.Join(s.cfg.DataDir, "rendered") }
func (s *Server) firmwareDir() string { return filepath.Join(s.cfg.DataDir, "firmware") }

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/device/register", s.handleRegister)
	mux.HandleFunc("GET /api/v1/device/manifest", s.handleManifest)
	mux.HandleFunc("POST /api/v1/device/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /media/{device}/{file}", s.handleMedia)
	mux.HandleFunc("GET /render/{device}/{file}", s.handleRender)
	mux.HandleFunc("GET /firmware/{file}", s.handleFirmwareDownload)
	s.registerAdmin(mux)
	return mux
}

// deviceByID 在静态配置与自注册设备中查找。
func (s *Server) deviceByID(id string) (DeviceConfig, bool) {
	if dev, ok := s.devices[id]; ok {
		return dev, true
	}
	if d, ok := s.store.Device(id); ok {
		return DeviceConfig{ID: d.ID, Secret: d.Secret, Name: d.Name}, true
	}
	return DeviceConfig{}, false
}

// allDevices 返回静态 + 自注册设备（静态在前，其余按 ID 排序）。
func (s *Server) allDevices() []DeviceConfig {
	out := append([]DeviceConfig(nil), s.cfg.Devices...)
	var reg []DeviceConfig
	s.store.View(func(st *store.State) {
		for _, d := range st.Devices {
			if _, static := s.devices[d.ID]; static {
				continue
			}
			reg = append(reg, DeviceConfig{ID: d.ID, Secret: d.Secret, Name: d.Name})
		}
	})
	sort.Slice(reg, func(i, j int) bool { return reg[i].ID < reg[j].ID })
	return append(out, reg...)
}

// authenticate 校验设备签名请求头，返回设备配置。
func (s *Server) authenticate(r *http.Request) (DeviceConfig, error) {
	id := r.Header.Get(sign.HeaderDeviceID)
	dev, ok := s.deviceByID(id)
	if !ok {
		return DeviceConfig{}, errors.New("unknown device")
	}
	err := sign.Verify(dev.Secret,
		r.Header.Get(sign.HeaderTimestamp), r.Method, r.URL.Path,
		r.Header.Get(sign.HeaderSign), s.now())
	if err != nil {
		return DeviceConfig{}, err
	}
	return dev, nil
}

func (s *Server) deviceMediaDir(deviceID string) string {
	return filepath.Join(s.cfg.MediaRoot, deviceID)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	m, err := s.buildManifest(dev)
	if err != nil {
		log.Printf("manifest build for %s failed: %v", dev.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	etag := `"` + m.Version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" {
		if inm == etag || inm == m.Version {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(m); err != nil {
		log.Printf("manifest encode for %s failed: %v", dev.ID, err)
	}
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// 设备只能访问自己的媒体目录。
	if r.PathValue("device") != dev.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	name := r.PathValue("file")
	if name == "" || strings.HasPrefix(name, ".") ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.Error(w, "bad file name", http.StatusBadRequest)
		return
	}
	// http.ServeFile 原生支持 Range 断点续传。
	http.ServeFile(w, r, filepath.Join(s.deviceMediaDir(dev.ID), name))
}

// 渲染画布尺寸（与显示屏一致）。
const canvasW, canvasH = 1440, 900

// resolveTemplate 决定设备当前应显示的模板及来源：
// 设备级覆盖 > 时段计划命中 > 全局默认模板；都没有则 ok=false（目录轮播）。
func (s *Server) resolveTemplate(deviceID string, now time.Time) (tpl store.Template, source string, ok bool) {
	if disp := s.store.Display(deviceID); disp.Mode == "template" {
		if t, found := s.store.Template(disp.TemplateID); found {
			return t, "override", true
		}
		log.Printf("device %s: override template %q missing, ignoring", deviceID, disp.TemplateID)
	}
	if sc, hit := store.ActiveSchedule(s.store.Schedules(), now.In(s.loc)); hit {
		if t, found := s.store.Template(sc.TemplateID); found {
			return t, "schedule", true
		}
		log.Printf("schedule %s: template %q missing, ignoring", sc.ID, sc.TemplateID)
	}
	if g := s.store.Global(); g.TemplateID != "" {
		if t, found := s.store.Template(g.TemplateID); found {
			return t, "global", true
		}
		log.Printf("global template %q missing, falling back to playlist", g.TemplateID)
	}
	return store.Template{}, "playlist", false
}

// buildManifest 生成设备清单：测试屏 > 模板（覆盖/时段/全局）> 目录轮播，并附带待执行指令。
func (s *Server) buildManifest(dev DeviceConfig) (*manifest.Manifest, error) {
	now := s.now()
	var items []manifest.Item

	if until := s.store.TestUntil(dev.ID); now.Before(until) {
		img, err := s.renderer.RenderTestCard(canvasW, canvasH, dev.ID, dev.Name, s.store.Attrs(dev.ID), until)
		if err != nil {
			return nil, fmt.Errorf("render test card: %w", err)
		}
		if items, err = s.renderedItems(dev.ID, "test", img); err != nil {
			return nil, err
		}
	} else if tpl, _, ok := s.resolveTemplate(dev.ID, now); ok {
		img, err := s.renderer.Render(tpl, s.store.Attrs(dev.ID), s.store.Display(dev.ID).Bindings)
		if err != nil {
			return nil, fmt.Errorf("render template %s: %w", tpl.ID, err)
		}
		if items, err = s.renderedItems(dev.ID, "tpl", img); err != nil {
			return nil, err
		}
	} else {
		m, err := manifest.BuildFromDir(s.deviceMediaDir(dev.ID), dev.ID, s.cfg.ImageDurationS, s.hashes)
		if err != nil {
			return nil, err
		}
		items = m.Items
	}

	cmds := []manifest.Command{}
	if cmd, ok := s.updateCommand(dev.ID, now); ok {
		cmds = append(cmds, cmd)
	}
	return &manifest.Manifest{Version: manifest.VersionWith(items, cmds), Items: items, Commands: cmds}, nil
}

// renderedItems 把渲染结果落盘为 PNG 并包装成单条目列表。
// 文件名内嵌内容哈希：内容不变则复用既有文件（版本稳定、设备端不重下）。
func (s *Server) renderedItems(deviceID, kind string, img image.Image) ([]manifest.Item, error) {
	var buf bytes.Buffer
	if err := render.EncodePNG(&buf, img); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(buf.Bytes())
	sumHex := hex.EncodeToString(sum[:])
	name := kind + "_" + sumHex[:12] + ".png"

	dir := filepath.Join(s.renderedDir(), deviceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
		// 清理该设备同类前缀的旧渲染文件。
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if n := e.Name(); n != name && strings.HasPrefix(n, kind+"_") {
					os.Remove(filepath.Join(dir, n))
				}
			}
		}
	}

	return []manifest.Item{{
		ID:       sumHex[:12],
		Type:     "image",
		Name:     name,
		URL:      "/render/" + url.PathEscape(deviceID) + "/" + url.PathEscape(name),
		SHA256:   sumHex,
		Size:     int64(buf.Len()),
		Duration: s.cfg.ImageDurationS,
		Order:    1,
	}}, nil
}

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.PathValue("device") != dev.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	name := r.PathValue("file")
	if name == "" || strings.HasPrefix(name, ".") ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.Error(w, "bad file name", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.renderedDir(), dev.ID, name))
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var hb Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&hb); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.lastSeen[dev.ID] = s.now()
	s.lastHB[dev.ID] = hb
	s.mu.Unlock()
	// 自注册设备：持久化程序版本/IP（仅变化时写盘），服务端重启后升级状态仍可判断。
	if d, ok := s.store.Device(dev.ID); ok && (d.AgentVersion != hb.PlayerVer || (hb.IP != "" && d.IP != hb.IP)) {
		if err := s.store.Update(func(st *store.State) error {
			d := st.Devices[dev.ID]
			d.AgentVersion = hb.PlayerVer
			if hb.IP != "" {
				d.IP = hb.IP
			}
			st.Devices[dev.ID] = d
			return nil
		}); err != nil {
			log.Printf("heartbeat: persist device %s failed: %v", dev.ID, err)
		}
	}
	log.Printf("heartbeat device=%s version=%s agent=%s uptime=%ds disk_free=%dMB playing=%q",
		dev.ID, hb.Version, hb.PlayerVer, hb.UptimeS, hb.DiskFreeMB, hb.Playing)
	w.WriteHeader(http.StatusNoContent)
}

// agentVersion 返回设备最近上报的程序版本（内存心跳优先，其次持久化记录）。
func (s *Server) agentVersion(deviceID string) string {
	s.mu.Lock()
	hb, ok := s.lastHB[deviceID]
	s.mu.Unlock()
	if ok && hb.PlayerVer != "" {
		return hb.PlayerVer
	}
	if d, ok := s.store.Device(deviceID); ok {
		return d.AgentVersion
	}
	return ""
}
