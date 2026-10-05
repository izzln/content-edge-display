// Package server 实现服务端：设备注册与清单下发、媒体分发、心跳、管理后台与管理 API、一键装机入口。
package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/render"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
)

// Server 持有配置与运行期状态。
type Server struct {
	cfg      *Config
	store    *store.Store
	hashes   *manifest.HashCache
	renderer *render.Renderer
	loc      *time.Location
	tools    tools
	cache    *contentCache // 文件缓存区，见 cache.go
	jobs     *jobQueue     // 视频转码、PDF 渲染，见 jobs.go
	cert     *tls.Certificate
	certFP   string     // 证书公钥指纹（tls.go）
	auth     *adminAuth // 后台口令验证与猜错锁定（adminauth.go）

	mu         sync.Mutex
	lastSeen   map[string]time.Time
	lastHB     map[string]manifest.Heartbeat
	sync       map[string]deviceSync // 设备取内容的进度与清单缓存，见 device.go
	authLogged map[string]time.Time  // 认证失败日志节流

	uploadMu  sync.Mutex
	uploading map[string]bool // 正在上传的 设备/文件名，见 claimMediaName

	stop    context.CancelFunc
	stopped chan struct{} // 后台协程退出后关闭
	now     func() time.Time
}

// tools 是服务端用到的外部程序；不可用时记下原因（启动日志与后台提示）。
type tools struct {
	enc    videoEncoder // nil：没有可用的 ffmpeg，不收视频
	encErr string
	pdf    pdfRenderer // nil：没有可用的 poppler-utils，不收 PDF
	pdfErr string
}

// findTools 在启动时检测一次 ffmpeg 与 poppler-utils；装好或改了 ffmpeg_path 后重启服务端生效。
func findTools(ffmpegPath string) tools {
	var t tools
	if enc, err := transcode.Find(ffmpegPath); err != nil {
		t.encErr = err.Error()
		log.Printf("warning: ffmpeg unavailable, video uploads are disabled: %v. "+
			"Run apt install ffmpeg, or set ffmpeg_path in server.json to an absolute path "+
			"executable by the service user, then restart the server", err)
	} else {
		t.enc = enc
		log.Printf("video transcoding enabled: %s", enc.Version())
	}
	if r, err := transcode.FindPDF(); err != nil {
		t.pdfErr = err.Error()
		log.Printf("warning: poppler-utils unavailable, PDF uploads are disabled: %v. "+
			"Run apt install poppler-utils, then restart the server", err)
	} else {
		t.pdf = r
		log.Printf("PDF rendering enabled: %s", r.Version())
	}
	return t
}

// New 创建服务端：加载状态文件、准备数据目录与证书、检测外部程序、启动后台协程。
func New(cfg *Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return newServer(cfg, findTools(cfg.FFmpegPath))
}

func newServer(cfg *Config, t tools) (*Server, error) {
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
		cfg: cfg, store: st, hashes: manifest.NewHashCache(), loc: loc, tools: t, jobs: newJobQueue(),
		lastSeen: map[string]time.Time{}, lastHB: map[string]manifest.Heartbeat{},
		sync: map[string]deviceSync{}, authLogged: map[string]time.Time{}, uploading: map[string]bool{},
		now: time.Now,
	}
	// incoming/ 是待处理原片的暂存区：任务只在内存里，重启后它们已无人认领，清掉。
	os.RemoveAll(s.incomingDir())
	for _, dir := range []string{cfg.MediaRoot, s.renderedDir(), s.packagesDir(), s.depsDir(), s.backgroundsDir(), s.thumbsDir(), s.incomingDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := s.loadOrCreateCert(); err != nil {
		return nil, err
	}
	if err := s.syncAdminToken(); err != nil {
		return nil, err
	}
	if s.renderer, err = render.New(cfg.FontPath, s.backgroundsDir()); err != nil {
		return nil, err
	}
	if cfg.FontPath == "" {
		log.Printf("warning: font_path is not set; CJK text in templates and test cards will render as boxes (install a CJK font such as fonts-noto-cjk and set font_path)")
	}
	if err := s.store.Update(func(st *store.State) {
		if t, seeded := store.SeedDefaultTemplate(st); seeded {
			log.Printf("first start: created default template %s (%s)", t.ID, t.Name)
		}
		// 全局模板指向已被删除的模板时纠正，避免谁都不显示内容
		if store.EnsureGlobalTemplate(st) {
			log.Printf("global default template set to %s", st.Global.TemplateID)
		}
	}); err != nil {
		return nil, err
	}
	if s.cache, err = openCache(filepath.Join(cfg.MediaRoot, ".store"), filepath.Join(cfg.DataDir, "cache.json")); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop, s.stopped = cancel, make(chan struct{})
	go func() {
		defer close(s.stopped)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.runJobs(ctx) }()
		go func() { defer wg.Done(); s.runCache(ctx.Done()) }()
		wg.Wait()
	}()
	return s, nil
}

// Close 停止后台转码与缓存整理协程并等它们退出（正在跑的 ffmpeg 会被杀掉），之后不会再有文件写入。
func (s *Server) Close() {
	s.stop()
	<-s.stopped
}

func (s *Server) renderedDir() string { return filepath.Join(s.cfg.DataDir, "rendered") }
func (s *Server) packagesDir() string { return filepath.Join(s.cfg.DataDir, "packages") }
func (s *Server) incomingDir() string { return filepath.Join(s.cfg.DataDir, "incoming") } // 待处理的原片
func (s *Server) deviceMediaDir(deviceID string) string {
	return filepath.Join(s.cfg.MediaRoot, deviceID)
}

// Handler 返回 HTTPS 端口的完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/device/register", s.handleRegister)
	mux.HandleFunc("GET /api/v1/device/manifest", s.handleManifest)
	mux.HandleFunc("POST /api/v1/device/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /media/{device}/{file}", s.serveDeviceFile(func(dev string, _ *http.Request) string { return s.deviceMediaDir(dev) }))
	// PDF 的逐页图片：/media/<设备>/<文档名>/p001.jpg
	mux.HandleFunc("GET /media/{device}/{doc}/{file}", s.serveDeviceFile(func(dev string, r *http.Request) string {
		doc := r.PathValue("doc")
		if !manifest.SafeFileName(doc) || manifest.TypeOf(doc) != "document" {
			return ""
		}
		return manifest.PagesPath(s.deviceMediaDir(dev), doc)
	}))
	mux.HandleFunc("GET /render/{device}/{file}", s.serveDeviceFile(func(dev string, _ *http.Request) string { return filepath.Join(s.renderedDir(), dev) }))
	mux.HandleFunc("GET /packages/{file}", s.serveDeviceFile(func(string, *http.Request) string { return s.packagesDir() }))
	s.registerAdmin(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/apt/") {
			s.handleApt(w, r) // 离线依赖仓库（deps.go），不经 ServeMux
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// tokenOK 常量时间比较口令。
func tokenOK(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
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
	if err := s.store.Update(fn); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	return true
}

// sortedByID 返回 map 的全部值，按 id 排序；空 map 返回 []（JSON 输出 [] 而不是 null）。
func sortedByID[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, id := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[id])
	}
	return out
}

// humanBytes 把字节数格式化成 KB/MB（日志与提示用）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// pathPrefix 去掉管理接口路径的公共前缀（日志用）。
func pathPrefix(p string) string { return strings.TrimPrefix(p, "/api/v1/admin") }
