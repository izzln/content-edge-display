// Package server 实现服务端 HTTP API：设备清单下发、媒体分发、心跳与管理查询。
package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/render"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
)

// placeholderToken 是配置样例里的占位口令。仓库是公开的，样例值人人可见，
// 带着它启动等于没有口令，所以直接拒绝启动。
const placeholderToken = "change-me"

// Config 是服务端配置（JSON 文件）。
// 设备不在这里配置：一律由设备凭 enroll_token 自注册，记录在 data_dir/state.json。
// 图片停留时长也不在这里配置：它是版式的一部分，跟着模板走（管理后台里改）。
type Config struct {
	Listen      string `json:"listen"`
	MediaRoot   string `json:"media_root"`
	DataDir     string `json:"data_dir"`  // state.json / rendered / firmware / incoming
	FontPath    string `json:"font_path"` // 模板渲染字体（生产需 CJK 字体）
	AdminToken  string `json:"admin_token"`
	EnrollToken string `json:"enroll_token"` // 设备自注册口令（烧进母镜像）
	Timezone    string `json:"timezone"`     // 时段计划时区，默认系统时区
	// FFmpegPath 指定 ffmpeg；留空在 PATH 里找（注意是服务进程的 PATH，systemd 下只有
	// /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin）。不可用时不能上传视频（后台会提示原因），
	// 因为未转码的原片码率过高，会让设备过热关机。
	FFmpegPath string `json:"ffmpeg_path,omitempty"`
	// 设备的轮询与心跳间隔：在这里统一规定，设备从响应头学到后照办（见 manifest.HeaderPollInterval）。
	// 轮询决定后台改动多快上屏、多快发现设备离线；心跳只上报温度/硬解等健康数据。
	PollIntervalS      int `json:"poll_interval_s,omitempty"`      // 默认 10
	HeartbeatIntervalS int `json:"heartbeat_interval_s,omitempty"` // 默认 60
}

// checkIntervals 填充轮询/心跳间隔的默认值并检查范围。
func (c *Config) checkIntervals() error {
	if c.PollIntervalS == 0 {
		c.PollIntervalS = manifest.DefaultPollIntervalS
	}
	if c.HeartbeatIntervalS == 0 {
		c.HeartbeatIntervalS = manifest.DefaultHeartbeatIntervalS
	}
	if c.PollIntervalS < manifest.MinPollIntervalS || c.PollIntervalS > manifest.MaxPollIntervalS {
		return fmt.Errorf("config: poll_interval_s must be %d-%d seconds", manifest.MinPollIntervalS, manifest.MaxPollIntervalS)
	}
	if c.HeartbeatIntervalS < manifest.MinHeartbeatIntervalS || c.HeartbeatIntervalS > manifest.MaxHeartbeatIntervalS {
		return fmt.Errorf("config: heartbeat_interval_s must be %d-%d seconds", manifest.MinHeartbeatIntervalS, manifest.MaxHeartbeatIntervalS)
	}
	return nil
}

// resolvePath 把相对路径按 base 目录展开；绝对路径与空值原样返回。
func resolvePath(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// LoadConfig 读取配置文件并填充默认值。
//
// 配置里的相对路径一律相对**配置文件所在目录**解析，而不是进程的工作目录：
// systemd 启动服务时工作目录是 /，按工作目录解析会让 "./data" 悄悄落到 /data。
// 这样也支持把 display-server、server.json、media/、data/、fonts/ 放在同一个目录里整体搬走。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":9000"
	}
	if cfg.MediaRoot == "" {
		cfg.MediaRoot = "data/media"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	cfg.MediaRoot = resolvePath(base, cfg.MediaRoot)
	cfg.DataDir = resolvePath(base, cfg.DataDir)
	cfg.FontPath = resolvePath(base, cfg.FontPath)
	// ffmpeg_path 只写程序名（如 "ffmpeg"）时按 PATH 查找；带路径的相对写法按配置文件目录解析
	if strings.ContainsRune(cfg.FFmpegPath, os.PathSeparator) {
		cfg.FFmpegPath = resolvePath(base, cfg.FFmpegPath)
	}
	if err := cfg.checkIntervals(); err != nil {
		return nil, err
	}
	if cfg.EnrollToken == "" {
		return nil, errors.New("config: enroll_token is empty, no device could enroll (generate one with make tokens)")
	}
	for name, tok := range map[string]string{"admin_token": cfg.AdminToken, "enroll_token": cfg.EnrollToken} {
		if tok == placeholderToken {
			return nil, fmt.Errorf("config: %s is still the public placeholder %q from the sample config; generate one with make tokens",
				name, placeholderToken)
		}
	}
	return &cfg, nil
}

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
	Sync         string              `json:"sync"`          // offline/waiting/syncing/latest，见 syncState
	ActiveTpl    string              `json:"active_template,omitempty"`
	UpdateTarget *store.UpdateTarget `json:"update_target,omitempty"`
	HW           *store.Device       `json:"hw,omitempty"`
}

// deviceSync 记录设备最近一次取清单时的情况。设备每次轮询都带着自己已应用的版本
// （If-None-Match），所以不用等心跳就能知道它显示的是不是最新内容。
type deviceSync struct {
	expected string // 那次轮询时服务端算出的版本
	applied  string // 设备当时已应用的版本
	served   string // 最近一次下发（200）给它的版本
	key      string // 那次轮询时这台设备显示输入的指纹，见 content.key
}

// 设备内容状态。
const (
	syncOffline = "offline" // 设备离线
	syncWaiting = "waiting" // 配置改过了，设备还没来取（≤ 一个轮询周期）
	syncSyncing = "syncing" // 设备已取到新清单，正在下载/切换
	syncLatest  = "latest"  // 设备显示的就是最新内容
)

// syncState 判断设备的内容状态（调用方持有 s.mu）。key 是这台设备显示输入的当前指纹（content.key），
// 与它上次轮询时的指纹不同，说明它该显示的内容变了而它还没来取。只看这台设备自己的输入：
// 改别的设备、改没被用到的模板都不影响它；时段计划到点切换、测试屏到期也能及时体现。
func (s *Server) syncState(deviceID string, online bool, key string) string {
	if !online {
		return syncOffline
	}
	st, ok := s.sync[deviceID]
	switch {
	case !ok || st.key != key:
		return syncWaiting
	case st.applied != st.expected:
		return syncSyncing
	default:
		return syncLatest
	}
}

// Server 持有配置与运行期状态。
type Server struct {
	cfg      *Config
	hashes   *manifest.HashCache
	store    *store.Store
	renderer *render.Renderer
	loc      *time.Location

	mu         sync.Mutex
	lastSeen   map[string]time.Time
	lastHB     map[string]manifest.Heartbeat
	sync       map[string]deviceSync // 设备取内容的进度（后台"当前显示"列）
	authLogged map[string]time.Time  // 认证失败日志节流

	encMu      sync.Mutex
	encoder    videoEncoder // nil 表示没有可用的 ffmpeg：不收视频
	encoderErr string       // 不可用的原因（日志与后台提示用）
	pdf        pdfRenderer  // nil 表示没有可用的 poppler-utils：不收 PDF
	pdfErr     string
	cache      *contentCache // 文件缓存区，见 cache.go
	jobs       *jobQueue
	uploadMu   sync.Mutex
	uploading  map[string]bool // 正在上传的 设备/文件名，见 claimMediaName
	stop       context.CancelFunc
	stopped    chan struct{} // 转码协程退出后关闭

	now func() time.Time // 测试注入
}

// New 创建服务端：加载状态文件、准备数据目录、初始化渲染器。
func New(cfg *Config) (*Server, error) {
	if err := cfg.checkIntervals(); err != nil {
		return nil, err
	}
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
		cfg:        cfg,
		hashes:     manifest.NewHashCache(),
		store:      st,
		loc:        loc,
		lastSeen:   make(map[string]time.Time),
		lastHB:     make(map[string]manifest.Heartbeat),
		sync:       make(map[string]deviceSync),
		authLogged: make(map[string]time.Time),
		now:        time.Now,
	}
	// 首次启动时把需要的目录都建出来（media_root 也在内：设备媒体文件放这里）。
	// incoming/ 是待转码原片的暂存区：转码任务只在内存里，重启后它们已无人认领，清掉。
	os.RemoveAll(s.incomingDir())
	for _, dir := range []string{cfg.MediaRoot, s.renderedDir(), s.firmwareDir(), s.incomingDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	s.renderer, err = render.New(cfg.FontPath)
	if err != nil {
		return nil, err
	}
	if cfg.FontPath == "" {
		log.Printf("warning: font_path is not set; CJK text in templates and test cards will render as boxes (install a CJK font such as fonts-noto-cjk and set font_path)")
	}
	if err := s.seedDefaults(); err != nil {
		return nil, err
	}

	if s.cache, err = openCache(filepath.Join(cfg.MediaRoot, ".store"), filepath.Join(cfg.DataDir, "cache.json")); err != nil {
		return nil, err
	}
	s.jobs = newJobQueue()
	// ffmpeg 只在启动时检测一次；装好或改了 ffmpeg_path 后重启服务端生效。
	if enc, err := transcode.Find(cfg.FFmpegPath); err != nil {
		s.encoderErr = err.Error()
		log.Printf("warning: ffmpeg unavailable, video uploads are disabled: %v. "+
			"Run apt install ffmpeg, or set ffmpeg_path in server.json to an absolute path "+
			"executable by the service user, then restart the server", err)
	} else {
		s.encoder = enc
		log.Printf("video transcoding enabled: %s", enc.Version())
	}
	if r, err := transcode.FindPDF(); err != nil {
		s.pdfErr = err.Error()
		log.Printf("warning: poppler-utils unavailable, PDF uploads are disabled: %v. "+
			"Run apt install poppler-utils, then restart the server", err)
	} else {
		s.pdf = r
		log.Printf("PDF rendering enabled: %s", r.Version())
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

// videoEncoder 返回可用的转码器，没有则为 nil。
func (s *Server) videoEncoder() videoEncoder {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	return s.encoder
}

// setEncoder 直接指定转码器（测试用）；nil 表示模拟"没有 ffmpeg"。
func (s *Server) setEncoder(enc videoEncoder) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	s.encoder, s.encoderErr = enc, ""
	if enc == nil {
		s.encoderErr = "test: no encoder configured"
	}
}

// pdfRenderer 返回可用的 PDF 渲染器，没有则为 nil。
func (s *Server) pdfRenderer() pdfRenderer {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	return s.pdf
}

// setPDFRenderer 直接指定 PDF 渲染器（测试用）；nil 表示模拟"没有 poppler-utils"。
func (s *Server) setPDFRenderer(r pdfRenderer) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	s.pdf, s.pdfErr = r, ""
	if r == nil {
		s.pdfErr = "test: no PDF renderer configured"
	}
}

// Close 停止后台转码与缓存整理协程并等它们退出（正在跑的 ffmpeg 会被杀掉），之后不会再有文件写入。
func (s *Server) Close() {
	if s.stop != nil {
		s.stop()
		<-s.stopped
	}
}

// seedDefaults 首次启动时播种一个可用的左右分屏模板并设为全局默认；
// 全局模板指向已被删除的模板时也在这里纠正，避免服务端启动后谁都不显示内容。
func (s *Server) seedDefaults() error {
	return s.store.Update(func(st *store.State) error {
		if t, seeded := store.SeedDefaultTemplate(st); seeded {
			log.Printf("first start: created default template %s (%s)", t.ID, t.Name)
		}
		if store.EnsureGlobalTemplate(st) {
			log.Printf("global default template set to %s", st.Global.TemplateID)
		}
		return nil
	})
}

func (s *Server) renderedDir() string { return filepath.Join(s.cfg.DataDir, "rendered") }
func (s *Server) firmwareDir() string { return filepath.Join(s.cfg.DataDir, "firmware") }

// Handler 返回完整路由。
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
		return filepath.Join(s.deviceMediaDir(dev), manifest.PagesDir, doc)
	}))
	mux.HandleFunc("GET /render/{device}/{file}", s.serveDeviceFile(func(dev string, _ *http.Request) string { return filepath.Join(s.renderedDir(), dev) }))
	mux.HandleFunc("GET /firmware/{file}", s.serveDeviceFile(func(string, *http.Request) string { return s.firmwareDir() }))
	s.registerAdmin(mux)
	return mux
}

// allDevices 返回全部已注册设备，按 ID 排序。
func (s *Server) allDevices() []store.Device {
	var out []store.Device
	s.store.View(func(st *store.State) {
		out = sortedValues(st.Devices, func(a, b store.Device) int { return strings.Compare(a.ID, b.ID) })
	})
	return out
}

// errUnknownDevice 表示请求里的设备编号没有登记（从未注册，或在后台被删除了）。
var errUnknownDevice = errors.New("unknown device")

// authenticate 校验设备签名请求头，返回设备。
func (s *Server) authenticate(r *http.Request) (store.Device, error) {
	dev, ok := s.store.Device(r.Header.Get(sign.HeaderDeviceID))
	if !ok {
		return store.Device{}, errUnknownDevice
	}
	err := sign.Verify(dev.Secret,
		r.Header.Get(sign.HeaderTimestamp), r.Method, r.URL.Path,
		r.Header.Get(sign.HeaderSign), s.now())
	if err != nil {
		return store.Device{}, err
	}
	return dev, nil
}

// announceSchedule 在响应头里告诉设备该按什么间隔轮询、心跳（设备照办，见 manifest.HeaderPollInterval）。
func (s *Server) announceSchedule(w http.ResponseWriter) {
	w.Header().Set(manifest.HeaderPollInterval, strconv.Itoa(s.cfg.PollIntervalS))
	w.Header().Set(manifest.HeaderHeartbeatInterval, strconv.Itoa(s.cfg.HeartbeatIntervalS))
}

// offlineAfter 返回设备多久没有任何请求就算离线：约 3 个轮询周期（轮询间隔由服务端规定，
// 设备照办），至少 30 秒。要连续几次没来才算，偶尔一次请求失败不会让状态来回跳。
func (s *Server) offlineAfter() time.Duration {
	return max(3*time.Duration(s.cfg.PollIntervalS)*time.Second, 30*time.Second)
}

// noteContact 记下设备最近一次联系，用于判断在线。任何签名通过的请求都算：轮询、心跳、下载文件——
// 设备下载大文件时轮询会暂停，只看轮询的话"正在刷新"的设备反而会被判离线。
func (s *Server) noteContact(dev store.Device, r *http.Request) {
	now := s.now()
	s.mu.Lock()
	seen, known := s.lastSeen[dev.ID]
	back := !known || now.Sub(seen) > s.offlineAfter()
	s.lastSeen[dev.ID] = now
	s.mu.Unlock()
	if back {
		ip := dev.IP
		if ip == "" {
			ip, _, _ = net.SplitHostPort(r.RemoteAddr)
		}
		log.Printf("device %s online (%s, agent %s)", dev.ID, ip, cmp.Or(dev.AgentVersion, "?"))
	}
}

// deviceAuth 校验设备请求；失败时回 401 并说明原因、记日志（同一设备同一原因每分钟最多一条）。
//
// 只回一句 "unauthorized" 的话，现场根本无从判断是时钟不准、密钥不对还是设备被删了——
// 而这三种的处理办法完全不同。原因写进响应体，设备端据此记日志或自动重新注册。
func (s *Server) deviceAuth(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	dev, err := s.authenticate(r)
	if err == nil {
		s.noteContact(dev, r)
		s.announceSchedule(w)
		return dev, true
	}
	id := r.Header.Get(sign.HeaderDeviceID)
	reason := err.Error()
	switch {
	case errors.Is(err, sign.ErrExpired):
		ts, _ := strconv.ParseInt(r.Header.Get(sign.HeaderTimestamp), 10, 64)
		reason = fmt.Sprintf("clock skew: device time %s, server time %s (allowed ±%s)",
			time.Unix(ts, 0).In(s.loc).Format(time.DateTime), s.now().In(s.loc).Format(time.DateTime), sign.MaxClockSkew)
	case errors.Is(err, sign.ErrMismatch):
		reason = "bad signature: device key does not match the registered key"
	}
	s.mu.Lock()
	key := id + "|" + strings.SplitN(reason, ":", 2)[0]
	if s.now().Sub(s.authLogged[key]) >= time.Minute {
		s.authLogged[key] = s.now()
		log.Printf("auth rejected: device %q %s %s: %s", id, r.Method, r.URL.Path, reason)
	}
	s.mu.Unlock()
	http.Error(w, "unauthorized: "+reason, http.StatusUnauthorized)
	return store.Device{}, false
}

// serveDeviceFile 生成设备下载文件的处理器（媒体、渲染图、固件共用）：
// 校验签名；路径里带 {device} 的只允许访问自己的目录；文件名不得含路径成分。
// http.ServeFile 原生支持 Range，设备端据此断点续传。
func (s *Server) serveDeviceFile(dirOf func(deviceID string, r *http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, ok := s.deviceAuth(w, r)
		if !ok {
			return
		}
		if d := r.PathValue("device"); d != "" && d != dev.ID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		name, dir := r.PathValue("file"), dirOf(dev.ID, r)
		if !manifest.SafeFileName(name) || dir == "" {
			http.Error(w, "bad file name", http.StatusBadRequest)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, name))
	}
}

func (s *Server) deviceMediaDir(deviceID string) string {
	return filepath.Join(s.cfg.MediaRoot, deviceID)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceAuth(w, r)
	if !ok {
		return
	}
	c, err := s.content(dev.ID, s.now())
	var m *manifest.Manifest
	if err == nil {
		m, err = s.buildManifest(dev.ID, c)
	}
	if err != nil {
		log.Printf("manifest build failed: device %s: %v", dev.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	applied := strings.Trim(strings.TrimSpace(r.Header.Get("If-None-Match")), `"`)
	s.mu.Lock()
	prev := s.sync[dev.ID]
	st := deviceSync{expected: m.Version, applied: applied, served: prev.served, key: c.key()}
	if applied != m.Version {
		st.served = m.Version
	}
	s.sync[dev.ID] = st
	s.mu.Unlock()

	w.Header().Set("ETag", `"`+m.Version+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if applied == m.Version {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if prev.served != m.Version { // 下载失败重试时会重复取同一版本，只记第一次
		what := fmt.Sprintf("%d file(s)", len(m.Items))
		if m.Layout != nil {
			what += " + template overlay"
		}
		if len(m.Commands) > 0 {
			what += fmt.Sprintf(", %d command(s)", len(m.Commands))
		}
		log.Printf("new content pushed: device %s, version %s, %s", dev.ID, m.Version, what)
	}
	writeJSON(w, m)
}

// 渲染画布尺寸（与显示屏一致）。
const canvasW, canvasH = 1440, 900

// resolveTemplate 决定设备当前应显示的模板及来源：
// 设备专属模板 > 时段计划命中 > 全局默认模板。全局默认模板由首启播种保证存在，
// 且最后一个模板不可删，所以正常情况下总能找到；找不到说明 state.json 被手工改坏了。
func (s *Server) resolveTemplate(deviceID string, now time.Time) (tpl store.Template, source string, ok bool) {
	if disp := s.store.Display(deviceID); disp.Mode == store.ModeTemplate {
		if t, found := s.store.Template(disp.TemplateID); found {
			return t, "override", true
		}
		log.Printf("device %s: template %q no longer exists, ignoring", deviceID, disp.TemplateID)
	}
	if sc, hit := store.ActiveSchedule(s.store.Schedules(), now.In(s.loc)); hit {
		if t, found := s.store.Template(sc.TemplateID); found {
			return t, "schedule", true
		}
		log.Printf("schedule %s: template %q no longer exists, ignoring", sc.ID, sc.TemplateID)
	}
	if t, found := s.store.Template(s.store.Global().TemplateID); found {
		return t, "global", true
	}
	return store.Template{}, "", false
}

// content 是决定一台设备此刻该显示什么的全部输入。清单只由它生成（buildManifest），
// 后台的"等待刷新"也只看它的指纹（key）——两边用的是同一份数据，指纹不可能漏掉清单的输入。
type content struct {
	TestUntil time.Time         `json:"test_until,omitzero"` // 非零：显示测试卡
	Source    string            `json:"source"`              // test/override/schedule/global
	Template  store.Template    `json:"template"`
	Attrs     map[string]string `json:"attrs"`
	Mirror    bool              `json:"mirror"`
	Media     []mediaFile       `json:"media"` // 媒体区的播放列表；模板没有媒体区时为空
	Update    *manifest.Command `json:"update,omitempty"`
}

// mediaFile 是播放列表里的一个文件。带上大小与修改时间：同名文件被替换也算内容变了。
type mediaFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// content 汇总设备此刻的显示输入。只读状态与文件元数据，不渲染、不算哈希，后台每次刷新都能算。
func (s *Server) content(deviceID string, now time.Time) (content, error) {
	c := content{Attrs: s.store.Attrs(deviceID)}
	if cmd, ok := s.updateCommand(deviceID, now); ok {
		c.Update = &cmd
	}
	if until := s.store.TestUntil(deviceID); now.Before(until) {
		c.TestUntil, c.Source = until, "test"
		return c, nil
	}
	tpl, source, ok := s.resolveTemplate(deviceID, now)
	if !ok {
		return c, errors.New("no usable template (global default template missing)")
	}
	c.Template, c.Source, c.Mirror = tpl, source, s.store.Display(deviceID).Mirror
	if _, ok := tpl.MediaRegion(); ok {
		names, err := s.playlist(deviceID)
		if err != nil {
			return c, err
		}
		dir := s.deviceMediaDir(deviceID)
		for _, n := range names {
			if fi, err := os.Stat(filepath.Join(dir, n)); err == nil {
				c.Media = append(c.Media, mediaFile{Name: n, Size: fi.Size(), ModTime: fi.ModTime()})
			}
		}
	}
	return c, nil
}

// key 是 content 的指纹。
func (c content) key() string {
	b, _ := json.Marshal(c) // map 按键排序输出，结果确定
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// buildManifest 由显示输入生成设备清单：测试卡，或模板（+ 媒体区播放列表），并附带待执行指令。
//
// 模板有媒体区且媒体区有内容时下发 layout：清单条目就是媒体文件本身（视频不转码），
// 模板的静态部分作为“媒体区挖空”的叠加图随 layout 下发，由设备端贴在画面上。
// 模板没有媒体区、或媒体区还没放内容时，把整块模板渲染成一张整屏图，
// 这样“刚建好还没传内容”的设备显示的是版式而不是黑屏。
func (s *Server) buildManifest(deviceID string, c content) (*manifest.Manifest, error) {
	var (
		img    image.Image
		kind   string // 渲染图的种类：test 测试卡 / tpl 整屏模板 / ovl 叠加图
		media  []manifest.Item
		layout *manifest.Layout
	)
	if !c.TestUntil.IsZero() {
		card, err := s.renderer.RenderTestCard(canvasW, canvasH, deviceID, c.Attrs, c.TestUntil.In(s.loc))
		if err != nil {
			return nil, fmt.Errorf("render test card: %w", err)
		}
		img, kind = card, "test"
	} else {
		names := make([]string, len(c.Media))
		for i, f := range c.Media {
			names[i] = f.Name
		}
		var err error
		media, err = manifest.BuildItems(s.deviceMediaDir(deviceID), deviceID, names, c.Template.ImageDurationS, s.hashes)
		if err != nil {
			return nil, err
		}
		rendered, err := s.renderer.Render(c.Template, c.Attrs, c.Mirror, len(media) > 0)
		if err != nil {
			return nil, fmt.Errorf("render template %s: %w", c.Template.ID, err)
		}
		img, kind = rendered.Image, "tpl"
		if len(media) > 0 {
			r := rendered.MediaRegion
			kind, layout = "ovl", &manifest.Layout{
				CanvasW: c.Template.W, CanvasH: c.Template.H,
				Media: manifest.Rect{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()},
			}
		}
	}
	png, err := s.renderedItem(deviceID, kind, img)
	if err != nil {
		return nil, err
	}
	s.pruneRendered(deviceID, png.Name)

	items := []manifest.Item{png}
	if layout != nil {
		items, layout.Overlay = media, png
	}
	cmds := []manifest.Command{}
	if c.Update != nil {
		cmds = append(cmds, *c.Update)
	}
	return &manifest.Manifest{
		Version:  manifest.Version(items, cmds, layout),
		Items:    items,
		Layout:   layout,
		Commands: cmds,
	}, nil
}

// renderedItem 把渲染结果落盘为 PNG 并包装成一个清单条目。
// 文件名内嵌内容哈希：内容不变则复用既有文件（版本稳定、设备端不重下）。
//
// Duration 恒为 0：渲染产物要么是整屏静态图（设备端对单图用 inf，不需要时长），
// 要么是叠加图（不进播放列表）。媒体区里图片的停留时长来自模板，写在媒体条目上。
func (s *Server) renderedItem(deviceID, kind string, img image.Image) (manifest.Item, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return manifest.Item{}, err
	}
	sum := sha256.Sum256(buf.Bytes())
	sumHex := hex.EncodeToString(sum[:])
	name := kind + "_" + sumHex[:12] + ".png"

	dir := filepath.Join(s.renderedDir(), deviceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return manifest.Item{}, err
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
			return manifest.Item{}, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return manifest.Item{}, err
		}
	}
	return manifest.Item{
		Type:   "image",
		Name:   name,
		URL:    "/render/" + url.PathEscape(deviceID) + "/" + url.PathEscape(name),
		SHA256: sumHex,
		Size:   int64(buf.Len()),
	}, nil
}

// pruneRendered 删除设备渲染目录里除 keep 之外的 PNG
// （模板改了、属性改了、或在测试卡/整屏图/叠加图之间切换后留下的旧文件）。
func (s *Server) pruneRendered(deviceID, keep string) {
	dir := filepath.Join(s.renderedDir(), deviceID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		// .tmp 是别的请求正在写入的文件，不碰。
		if n := e.Name(); n != keep && !strings.HasSuffix(n, ".tmp") {
			os.Remove(filepath.Join(dir, n))
		}
	}
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceAuth(w, r)
	if !ok {
		return
	}
	var hb manifest.Heartbeat
	if !decodeJSON(w, r, 64<<10, &hb) {
		return
	}
	s.mu.Lock()
	s.lastHB[dev.ID] = hb
	s.mu.Unlock()
	// 持久化程序版本/IP（仅变化时写盘）：服务端重启后升级状态仍可判断。
	if dev.AgentVersion != hb.AgentVersion || (hb.IP != "" && dev.IP != hb.IP) {
		if err := s.store.Update(func(st *store.State) error {
			d := st.Devices[dev.ID]
			d.AgentVersion = hb.AgentVersion
			if hb.IP != "" {
				d.IP = hb.IP
			}
			st.Devices[dev.ID] = d
			return nil
		}); err != nil {
			log.Printf("saving version/IP of device %s failed: %v", dev.ID, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
