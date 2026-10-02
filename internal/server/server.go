// Package server 实现服务端 HTTP API：设备清单下发、媒体分发、心跳与管理查询。
package server

import (
	"bytes"
	"context"
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

// OnlineWindow 内有心跳视为设备在线。
const OnlineWindow = 5 * time.Minute

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
	if cfg.EnrollToken == "" {
		return nil, errors.New("config: enroll_token 不能为空，否则没有任何设备能接入（用 make tokens 生成）")
	}
	for name, tok := range map[string]string{"admin_token": cfg.AdminToken, "enroll_token": cfg.EnrollToken} {
		if tok == placeholderToken {
			return nil, fmt.Errorf("config: %s 还是配置样例里的占位值 %q，这个值是公开的，请用 make tokens 生成后替换",
				name, placeholderToken)
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
	// HWDec 是设备端 mpv 实际使用的硬解方式；"no" = 软解（H3 上带不动 1440×900，
	// 会卡顿发热甚至过热关机），空串 = 问不到（如 null 播放器）。
	HWDec string `json:"hwdec,omitempty"`
	// OutputW/H 是显示屏实际输出分辨率，与模板画布不一致时要查内核的 video= 参数。
	OutputW int `json:"output_w,omitempty"`
	OutputH int `json:"output_h,omitempty"`
}

// DeviceStatus 是管理接口返回的设备状态。
type DeviceStatus struct {
	ID           string              `json:"id"`
	Online       bool                `json:"online"`
	LastSeen     *time.Time          `json:"last_seen,omitempty"`
	Heartbeat    *Heartbeat          `json:"heartbeat,omitempty"`
	Attrs        map[string]string   `json:"attrs"`
	Display      store.DisplayConfig `json:"display"`
	TestUntil    *time.Time          `json:"test_until,omitempty"`
	ActiveSource string              `json:"active_source"` // test/override/schedule/global
	ActiveTpl    string              `json:"active_template,omitempty"`
	AgentVersion string              `json:"agent_version,omitempty"`
	UpdateTarget *store.UpdateTarget `json:"update_target,omitempty"`
	HW           *store.Device       `json:"hw,omitempty"`
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
	lastHB     map[string]Heartbeat
	authLogged map[string]time.Time // 认证失败日志节流

	encMu      sync.Mutex
	encoder    videoEncoder                 // nil 表示当前没有可用的 ffmpeg：不收视频
	encoderErr string                       // 不可用的原因（日志与后台提示用）
	encTried   time.Time                    // 上次探测时间
	findEnc    func() (videoEncoder, error) // 探测 ffmpeg（测试可替换）
	jobs       *jobQueue
	stop       context.CancelFunc
	stopped    chan struct{} // 转码协程退出后关闭

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
		cfg:        cfg,
		hashes:     manifest.NewHashCache(),
		store:      st,
		loc:        loc,
		lastSeen:   make(map[string]time.Time),
		lastHB:     make(map[string]Heartbeat),
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
		log.Printf("warning: font_path 未配置，模板/测试卡中的中文将无法正常显示（请安装 CJK 字体并配置，如 fonts-noto-cjk）")
	}
	if err := s.seedDefaults(); err != nil {
		return nil, err
	}

	s.jobs = newJobQueue()
	s.findEnc = func() (videoEncoder, error) {
		enc, err := transcode.Find(cfg.FFmpegPath)
		if err != nil {
			return nil, err // 注意别把 nil 的 *Encoder 包进接口
		}
		return enc, nil
	}
	s.videoEncoder()
	ctx, cancel := context.WithCancel(context.Background())
	s.stop, s.stopped = cancel, make(chan struct{})
	go func() {
		defer close(s.stopped)
		s.runTranscoder(ctx)
	}()
	return s, nil
}

// encoderRetry 是 ffmpeg 不可用时重新探测的最短间隔：运营方装好 ffmpeg 后不用重启服务端。
const encoderRetry = 30 * time.Second

// videoEncoder 返回可用的转码器，没有则为 nil。不可用时按 encoderRetry 节流重新探测。
func (s *Server) videoEncoder() videoEncoder {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if s.encoder != nil || s.findEnc == nil || time.Since(s.encTried) < encoderRetry {
		return s.encoder
	}
	s.encTried = time.Now()
	enc, err := s.findEnc()
	if err != nil {
		if s.encoderErr != err.Error() { // 同一原因只记一次，别刷屏
			log.Printf("warning: ffmpeg 不可用，暂不能上传视频：%v。"+
				"请 apt install ffmpeg，或在 server.json 的 ffmpeg_path 里写 ffmpeg 的绝对路径"+
				"（该文件须能被服务的运行用户执行）", err)
		}
		s.encoderErr = err.Error()
		return nil
	}
	s.encoder, s.encoderErr = enc, ""
	log.Printf("视频转码已启用：%s", enc.Version())
	return enc
}

// setEncoder 直接指定转码器（测试用）；nil 表示模拟"没有 ffmpeg"且不再探测。
func (s *Server) setEncoder(enc videoEncoder) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	s.encoder, s.findEnc = enc, nil
	if enc == nil {
		s.encoderErr = "测试：未配置转码器"
	}
}

// Close 停止后台转码协程并等它退出（正在跑的 ffmpeg 会被杀掉），之后不会再有文件写入。
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
			log.Printf("首次启动：已创建默认模板 %s（%s）", t.ID, t.Name)
		}
		if store.EnsureGlobalTemplate(st) {
			log.Printf("全局默认模板设为 %s", st.Global.TemplateID)
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
	mux.HandleFunc("GET /media/{device}/{file}", s.serveDeviceFile(func(dev string) string { return s.deviceMediaDir(dev) }))
	mux.HandleFunc("GET /render/{device}/{file}", s.serveDeviceFile(func(dev string) string { return filepath.Join(s.renderedDir(), dev) }))
	mux.HandleFunc("GET /firmware/{file}", s.serveDeviceFile(func(string) string { return s.firmwareDir() }))
	s.registerAdmin(mux)
	return mux
}

// allDevices 返回全部已注册设备，按 ID 排序。
func (s *Server) allDevices() []store.Device {
	out := []store.Device{}
	s.store.View(func(st *store.State) {
		for _, d := range st.Devices {
			out = append(out, d)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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

// deviceAuth 校验设备请求；失败时回 401 并说明原因、记日志（同一设备同一原因每分钟最多一条）。
//
// 只回一句 "unauthorized" 的话，现场根本无从判断是时钟不准、密钥不对还是设备被删了——
// 而这三种的处理办法完全不同。原因写进响应体，设备端据此记日志或自动重新注册。
func (s *Server) deviceAuth(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	dev, err := s.authenticate(r)
	if err == nil {
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
		log.Printf("auth: rejected device=%q %s %s: %s", id, r.Method, r.URL.Path, reason)
	}
	s.mu.Unlock()
	http.Error(w, "unauthorized: "+reason, http.StatusUnauthorized)
	return store.Device{}, false
}

// serveDeviceFile 生成设备下载文件的处理器（媒体、渲染图、固件共用）：
// 校验签名；路径里带 {device} 的只允许访问自己的目录；文件名不得含路径成分。
// http.ServeFile 原生支持 Range，设备端据此断点续传。
func (s *Server) serveDeviceFile(dirOf func(deviceID string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, ok := s.deviceAuth(w, r)
		if !ok {
			return
		}
		if d := r.PathValue("device"); d != "" && d != dev.ID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		name := r.PathValue("file")
		if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || strings.Contains(name, "\\") {
			http.Error(w, "bad file name", http.StatusBadRequest)
			return
		}
		http.ServeFile(w, r, filepath.Join(dirOf(dev.ID), name))
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
		log.Printf("device %s: override template %q missing, ignoring", deviceID, disp.TemplateID)
	}
	if sc, hit := store.ActiveSchedule(s.store.Schedules(), now.In(s.loc)); hit {
		if t, found := s.store.Template(sc.TemplateID); found {
			return t, "schedule", true
		}
		log.Printf("schedule %s: template %q missing, ignoring", sc.ID, sc.TemplateID)
	}
	if t, found := s.store.Template(s.store.Global().TemplateID); found {
		return t, "global", true
	}
	return store.Template{}, "", false
}

// mediaItems 构建设备媒体区的播放条目（顺序见 playlist）；模板没有媒体区时为空。
func (s *Server) mediaItems(deviceID string, tpl store.Template) ([]manifest.Item, error) {
	if _, ok := tpl.MediaRegion(); !ok {
		return nil, nil
	}
	names, err := s.playlist(deviceID)
	if err != nil {
		return nil, err
	}
	return manifest.BuildItems(s.deviceMediaDir(deviceID), deviceID, names, tpl.ImageDurationS, s.hashes)
}

// buildManifest 生成设备清单：测试屏 > 模板（专属/时段/全局），并附带待执行指令。
//
// 模板有媒体区且媒体区有内容时下发 layout：清单条目就是媒体文件本身（视频不转码），
// 模板的静态部分作为“媒体区挖空”的叠加图随 layout 下发，由设备端贴在画面上。
// 模板没有媒体区、或媒体区还没放内容时，退回到把整块模板渲染成一张整屏图的成熟路径，
// 这样“刚建好还没传内容”的设备显示的是版式而不是黑屏。
func (s *Server) buildManifest(dev store.Device) (*manifest.Manifest, error) {
	now := s.now()
	var (
		items  []manifest.Item
		layout *manifest.Layout
		keep   []string // 本份清单用到的渲染文件名
	)

	if until := s.store.TestUntil(dev.ID); now.Before(until) {
		img, err := s.renderer.RenderTestCard(canvasW, canvasH, dev.ID, s.store.Attrs(dev.ID), until.In(s.loc))
		if err != nil {
			return nil, fmt.Errorf("render test card: %w", err)
		}
		it, err := s.renderedItem(dev.ID, "test", img)
		if err != nil {
			return nil, err
		}
		items, keep = []manifest.Item{it}, []string{it.Name}
	} else if tpl, _, ok := s.resolveTemplate(dev.ID, now); !ok {
		return nil, errors.New("没有可用的模板（全局默认模板缺失）")
	} else {
		media, err := s.mediaItems(dev.ID, tpl)
		if err != nil {
			return nil, err
		}
		rendered, err := s.renderer.Render(tpl, s.store.Attrs(dev.ID), s.store.Display(dev.ID).Mirror, len(media) > 0)
		if err != nil {
			return nil, fmt.Errorf("render template %s: %w", tpl.ID, err)
		}
		kind := "tpl" // 整屏静态图
		if len(media) > 0 {
			kind = "ovl" // 叠加图
		}
		png, err := s.renderedItem(dev.ID, kind, rendered.Image)
		if err != nil {
			return nil, err
		}
		keep = []string{png.Name}
		if len(media) > 0 {
			r := rendered.MediaRegion
			items, layout = media, &manifest.Layout{
				CanvasW: tpl.W, CanvasH: tpl.H,
				Media:   manifest.Rect{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()},
				Overlay: png,
			}
		} else {
			items = []manifest.Item{png}
		}
	}
	s.pruneRendered(dev.ID, keep)

	cmds := []manifest.Command{}
	if cmd, ok := s.updateCommand(dev.ID, now); ok {
		cmds = append(cmds, cmd)
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
	if err := render.EncodePNG(&buf, img); err != nil {
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

// pruneRendered 删除设备渲染目录里本份清单不再引用的 PNG
// （模板改了、属性改了、或在“整屏图”与“叠加图”之间切换后留下的旧文件）。
func (s *Server) pruneRendered(deviceID string, keep []string) {
	dir := filepath.Join(s.renderedDir(), deviceID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	alive := make(map[string]bool, len(keep))
	for _, n := range keep {
		alive[n] = true
	}
	for _, e := range entries {
		n := e.Name()
		// .tmp 是别的请求正在写入的文件，不碰。
		if alive[n] || strings.HasSuffix(n, ".tmp") {
			continue
		}
		os.Remove(filepath.Join(dir, n))
	}
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceAuth(w, r)
	if !ok {
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
	// 持久化程序版本/IP（仅变化时写盘）：服务端重启后升级状态仍可判断。
	if dev.AgentVersion != hb.PlayerVer || (hb.IP != "" && dev.IP != hb.IP) {
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
	log.Printf("heartbeat device=%s version=%s agent=%s uptime=%ds disk_free=%dMB temp=%dC hwdec=%s out=%dx%d playing=%q",
		dev.ID, hb.Version, hb.PlayerVer, hb.UptimeS, hb.DiskFreeMB, hb.TempC,
		hb.HWDec, hb.OutputW, hb.OutputH, hb.Playing)
	w.WriteHeader(http.StatusNoContent)
}
