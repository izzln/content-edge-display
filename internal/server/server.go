// Package server 实现服务端：设备注册与清单下发、媒体分发、心跳、管理后台与管理 API、一键装机入口。
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net"
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
	zone     string // loc 的 IANA 名称（下发给设备）；拿不到时为空，见 zoneName
	tools    tools
	cache    *contentCache // 文件缓存区，见 cache.go
	jobs     *jobQueue     // 视频转码、PDF 渲染，见 jobs.go
	cert     *tls.Certificate
	certFP   string     // 证书公钥指纹（tls.go）
	auth     *adminAuth // 后台口令验证与猜错锁定（adminauth.go）

	mu         sync.Mutex
	devices    map[string]*deviceRuntime // 设备的在线、心跳与取内容进度（只在内存里），见 device.go
	authLogged map[string]time.Time      // 认证失败日志节流

	uploadMu  sync.Mutex
	uploading map[string]map[string]bool // 设备 → 正在上传的文件名，见 claimMediaName

	thumbMu sync.Mutex // 生成缩略图与视频抽帧一次一个：同一视频并发请求只抽一次（frames.go）

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

// New 创建服务端（cfg 来自 LoadConfig，已校验）：加载状态文件、准备数据目录与证书、检测外部程序、启动后台协程。
func New(cfg *Config) (*Server, error) {
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
		cfg: cfg, store: st, hashes: manifest.NewHashCache(), loc: loc, zone: zoneName(cfg.Timezone, systemZoneFiles), tools: t, jobs: newJobQueue(),
		devices: map[string]*deviceRuntime{}, authLogged: map[string]time.Time{}, uploading: map[string]map[string]bool{},
		now: time.Now,
	}
	// incoming/ 是上传与处理的暂存区（原片、解包、临时文件）：任务只在内存里，重启后已无人认领，清掉。
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
	if err := s.store.Update(func(st *store.State) error {
		if t, seeded := store.SeedDefaultTemplate(st); seeded {
			log.Printf("first start: created default template %s (%s)", t.ID, t.Name)
		}
		// 全局模板指向已被删除的模板时纠正，避免谁都不显示内容
		if store.EnsureGlobalTemplate(st) {
			log.Printf("global default template set to %s", st.Global.TemplateID)
		}
		return nil
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

// data_dir 下的目录。
func (s *Server) renderedDir() string    { return filepath.Join(s.cfg.DataDir, "rendered") }    // 渲染图（叠加图、整屏图、测试卡）
func (s *Server) packagesDir() string    { return filepath.Join(s.cfg.DataDir, "packages") }    // 设备程序包
func (s *Server) depsDir() string        { return filepath.Join(s.cfg.DataDir, "deps") }        // 离线依赖仓库
func (s *Server) backgroundsDir() string { return filepath.Join(s.cfg.DataDir, "backgrounds") } // 模板底图
func (s *Server) thumbsDir() string      { return filepath.Join(s.cfg.DataDir, "thumbs") }      // 缩略图与视频抽帧
func (s *Server) incomingDir() string    { return filepath.Join(s.cfg.DataDir, "incoming") }    // 暂存区
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

// clientIP 是请求方的 IP（设备在局域网里，看到的就是它自己的地址）。
func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// shortSum 是内容 sha256 的前 16 个十六进制字符（文件名、指纹用）。
func shortSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
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
		http.Error(w, "请求格式不对", http.StatusBadRequest)
		return false
	}
	return true
}

// httpError 是要原样回给后台的错误（状态码 + 中文说明）。
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func errBadRequest(format string, a ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}
func errConflict(format string, a ...any) error {
	return &httpError{http.StatusConflict, fmt.Sprintf(format, a...)}
}
func errNotFound(what string) error { return &httpError{http.StatusNotFound, what + "不存在"} }

// update 在一次写锁内检查并修改状态、持久化：fn 返回 httpError 时按它回复，其余错误（写盘失败）回 500；
// 出错都不写盘，返回 false。
func (s *Server) update(w http.ResponseWriter, fn func(*store.State) error) bool {
	if err := s.store.Update(fn); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

// writeError 回复 err：httpError 用它的状态码，其余按 500。
func writeError(w http.ResponseWriter, err error) {
	if he, ok := err.(*httpError); ok {
		http.Error(w, he.msg, he.code)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// editDevice 在写锁内修改一台设备（设备不存在时回 404）。
func (s *Server) editDevice(w http.ResponseWriter, id string, fn func(*store.Device) error) bool {
	return s.update(w, func(st *store.State) error {
		d, ok := st.Devices[id]
		if !ok {
			return errNotFound("设备")
		}
		return fn(d)
	})
}

// sortedByID 返回 map 的全部值，按 id 排序；空 map 返回 []（JSON 输出 [] 而不是 null）。
func sortedByID[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, id := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[id])
	}
	return out
}

// pathPrefix 去掉管理接口路径的公共前缀（日志用）。
func pathPrefix(p string) string { return strings.TrimPrefix(p, "/api/v1/admin") }
