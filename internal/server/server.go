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
	DataDir        string         `json:"data_dir"`   // state.json / uploads / rendered
	FontPath       string         `json:"font_path"`  // 模板渲染字体（生产需 CJK 字体）
	AdminToken     string         `json:"admin_token"`
	ImageDurationS int            `json:"image_duration_s"`
	Devices        []DeviceConfig `json:"devices"`
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
	if len(cfg.Devices) == 0 {
		return nil, errors.New("config: no devices configured")
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
}

// DeviceStatus 是管理接口返回的设备状态。
type DeviceStatus struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Online    bool                `json:"online"`
	LastSeen  *time.Time          `json:"last_seen,omitempty"`
	Heartbeat *Heartbeat          `json:"heartbeat,omitempty"`
	Attrs     map[string]string   `json:"attrs"`
	Display   store.DisplayConfig `json:"display"`
	TestUntil *time.Time          `json:"test_until,omitempty"`
}

// Server 持有配置与运行期状态。
type Server struct {
	cfg      *Config
	devices  map[string]DeviceConfig
	hashes   *manifest.HashCache
	store    *store.Store
	renderer *render.Renderer

	mu       sync.Mutex
	lastSeen map[string]time.Time
	lastHB   map[string]Heartbeat

	now func() time.Time // 测试注入
}

// New 创建服务端：加载状态文件、准备 uploads/rendered 目录、初始化渲染器。
func New(cfg *Config) (*Server, error) {
	st, err := store.Open(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		devices:  make(map[string]DeviceConfig),
		hashes:   manifest.NewHashCache(),
		store:    st,
		lastSeen: make(map[string]time.Time),
		lastHB:   make(map[string]Heartbeat),
		now:      time.Now,
	}
	for _, dir := range []string{s.uploadsDir(), s.renderedDir()} {
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

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/device/manifest", s.handleManifest)
	mux.HandleFunc("POST /api/v1/device/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /media/{device}/{file}", s.handleMedia)
	mux.HandleFunc("GET /render/{device}/{file}", s.handleRender)
	s.registerAdmin(mux)
	return mux
}

// authenticate 校验设备签名请求头，返回设备配置。
func (s *Server) authenticate(r *http.Request) (DeviceConfig, error) {
	id := r.Header.Get(sign.HeaderDeviceID)
	dev, ok := s.devices[id]
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

// buildManifest 按优先级生成设备清单：测试屏 > 模板模式 > 目录轮播。
func (s *Server) buildManifest(dev DeviceConfig) (*manifest.Manifest, error) {
	// 1. 测试屏：截止时间未到则全屏测试卡；到期自动回落，无需清理任务。
	if until := s.store.TestUntil(dev.ID); s.now().Before(until) {
		img, err := s.renderer.RenderTestCard(canvasW, canvasH, dev.ID, dev.Name, s.store.Attrs(dev.ID), until)
		if err != nil {
			return nil, fmt.Errorf("render test card: %w", err)
		}
		return s.renderedManifest(dev.ID, "test", img)
	}

	// 2. 模板模式：渲染模板成图（模板被删除等异常时回落目录轮播并告警）。
	if disp := s.store.Display(dev.ID); disp.Mode == "template" {
		tpl, ok := s.store.Template(disp.TemplateID)
		if !ok {
			log.Printf("device %s: template %q missing, falling back to playlist", dev.ID, disp.TemplateID)
		} else {
			img, err := s.renderer.Render(tpl, s.store.Attrs(dev.ID), disp.Bindings)
			if err != nil {
				return nil, fmt.Errorf("render template %s: %w", tpl.ID, err)
			}
			return s.renderedManifest(dev.ID, "tpl", img)
		}
	}

	// 3. 目录轮播（原有行为）。
	return manifest.BuildFromDir(s.deviceMediaDir(dev.ID), dev.ID, s.cfg.ImageDurationS, s.hashes)
}

// renderedManifest 把渲染结果落盘为 PNG 并包装成单条目清单。
// 文件名内嵌内容哈希：内容不变则复用既有文件（版本稳定、设备端不重下）。
func (s *Server) renderedManifest(deviceID, kind string, img image.Image) (*manifest.Manifest, error) {
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

	items := []manifest.Item{{
		ID:       sumHex[:12],
		Type:     "image",
		Name:     name,
		URL:      "/render/" + url.PathEscape(deviceID) + "/" + url.PathEscape(name),
		SHA256:   sumHex,
		Size:     int64(buf.Len()),
		Duration: s.cfg.ImageDurationS,
		Order:    1,
	}}
	return &manifest.Manifest{Version: manifest.VersionOf(items), Items: items, Commands: []string{}}, nil
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
	log.Printf("heartbeat device=%s version=%s uptime=%ds disk_free=%dMB playing=%q",
		dev.ID, hb.Version, hb.UptimeS, hb.DiskFreeMB, hb.Playing)
	w.WriteHeader(http.StatusNoContent)
}
