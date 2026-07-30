// Package server 实现服务端 HTTP API：设备清单下发、媒体分发、心跳与管理查询。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
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
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Online    bool       `json:"online"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Heartbeat *Heartbeat `json:"heartbeat,omitempty"`
}

// Server 持有配置与运行期状态。
type Server struct {
	cfg     *Config
	devices map[string]DeviceConfig
	hashes  *manifest.HashCache

	mu       sync.Mutex
	lastSeen map[string]time.Time
	lastHB   map[string]Heartbeat

	now func() time.Time // 测试注入
}

func New(cfg *Config) *Server {
	s := &Server{
		cfg:      cfg,
		devices:  make(map[string]DeviceConfig),
		hashes:   manifest.NewHashCache(),
		lastSeen: make(map[string]time.Time),
		lastHB:   make(map[string]Heartbeat),
		now:      time.Now,
	}
	for _, d := range cfg.Devices {
		s.devices[d.ID] = d
	}
	return s
}

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/device/manifest", s.handleManifest)
	mux.HandleFunc("POST /api/v1/device/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /media/{device}/{file}", s.handleMedia)
	mux.HandleFunc("GET /api/v1/admin/devices", s.handleAdminDevices)
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
	m, err := manifest.BuildFromDir(s.deviceMediaDir(dev.ID), dev.ID, s.cfg.ImageDurationS, s.hashes)
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

func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminToken != "" && r.Header.Get("X-Admin-Token") != s.cfg.AdminToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	now := s.now()
	s.mu.Lock()
	statuses := make([]DeviceStatus, 0, len(s.cfg.Devices))
	for _, d := range s.cfg.Devices {
		st := DeviceStatus{ID: d.ID, Name: d.Name}
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
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(statuses); err != nil {
		log.Printf("admin devices encode failed: %v", err)
	}
}
