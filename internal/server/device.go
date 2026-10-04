package server

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
)

// ---- 设备认证 ----

// errUnknownDevice 表示请求里的设备编号没有登记（从未注册，或在后台被删除了）。
var errUnknownDevice = errors.New("unknown device")

// clientIP 是请求方的 IP（设备在局域网里，看到的就是它自己的地址）。
func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// announceSchedule 在响应头里告诉设备该按什么间隔轮询、心跳（设备照办，见 manifest.HeaderPollInterval）。
func (s *Server) announceSchedule(w http.ResponseWriter) {
	w.Header().Set(manifest.HeaderPollInterval, strconv.Itoa(s.cfg.PollIntervalS))
	w.Header().Set(manifest.HeaderHeartbeatInterval, strconv.Itoa(s.cfg.HeartbeatIntervalS))
}

// offlineAfter 返回设备多久没有任何请求就算离线：约 3 个轮询周期，至少 30 秒。
// 要连续几次没来才算，偶尔一次请求失败不会让状态来回跳。
func (s *Server) offlineAfter() time.Duration {
	return max(3*time.Duration(s.cfg.PollIntervalS)*time.Second, 30*time.Second)
}

// deviceAuth 校验设备签名请求；失败时回 401 并说明原因、记日志（同一设备同一原因每分钟最多一条）。
//
// 只回一句 "unauthorized" 的话，现场根本无从判断是时钟不准、密钥不对还是设备被删了——
// 而这三种的处理办法完全不同。原因写进响应体，设备端据此记日志或自动重新注册。
func (s *Server) deviceAuth(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	id := r.Header.Get(sign.HeaderDeviceID)
	dev, ok := s.store.Device(id)
	err := errUnknownDevice
	if ok {
		err = sign.Verify(dev.Secret, r.Header.Get(sign.HeaderTimestamp), r.Method, r.URL.Path, r.Header.Get(sign.HeaderSign), s.now())
	}
	if err == nil {
		s.noteContact(dev, r)
		s.announceSchedule(w)
		return dev, true
	}
	reason := err.Error()
	switch {
	case errors.Is(err, sign.ErrExpired):
		ts, _ := strconv.ParseInt(r.Header.Get(sign.HeaderTimestamp), 10, 64)
		reason = fmt.Sprintf("clock skew: device time %s, server time %s (allowed ±%s)",
			time.Unix(ts, 0).In(s.loc).Format(time.DateTime), s.now().In(s.loc).Format(time.DateTime), sign.MaxClockSkew)
	case errors.Is(err, sign.ErrMismatch):
		reason = "bad signature: device key does not match the registered key"
	}
	kind, _, _ := strings.Cut(reason, ":")
	s.mu.Lock()
	if key := id + "|" + kind; s.now().Sub(s.authLogged[key]) >= time.Minute {
		s.authLogged[key] = s.now()
		log.Printf("auth rejected: device %q %s %s: %s", id, r.Method, r.URL.Path, reason)
	}
	s.mu.Unlock()
	http.Error(w, "unauthorized: "+reason, http.StatusUnauthorized)
	return store.Device{}, false
}

// noteContact 记下设备最近一次联系，用于判断在线。任何签名通过的请求都算：轮询、心跳、下载文件——
// 设备下载大文件时轮询会暂停，只看轮询的话"正在刷新"的设备反而会被判离线。
func (s *Server) noteContact(dev store.Device, r *http.Request) {
	now := s.now()
	s.mu.Lock()
	seen, known := s.lastSeen[dev.ID]
	s.lastSeen[dev.ID] = now
	s.mu.Unlock()
	if !known || now.Sub(seen) > s.offlineAfter() {
		log.Printf("device %s online (%s, agent %s)", dev.ID, clientIP(r), cmp.Or(dev.AgentVersion, "?"))
	}
}

// serveDeviceFile 生成设备下载文件的处理器（媒体、渲染图、程序包共用）：
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

// ---- 清单 ----

// deviceSync 记录设备最近一次取清单时的情况。设备每次轮询都带着自己已应用的版本
// （If-None-Match），所以不用等心跳就能知道它显示的是不是最新内容。
type deviceSync struct {
	key      string             // 那次轮询时这台设备显示输入的指纹，见 content.key
	m        *manifest.Manifest // 按 key 生成的清单：key 不变就直接复用，不再渲染、编码、算哈希
	applied  string             // 设备当时已应用的版本
	announce string             // 最近一次记过"新内容下发"日志的版本
}

// 设备内容状态（后台"当前显示"列）。
const (
	syncOffline = "offline" // 设备离线
	syncWaiting = "waiting" // 配置改过了，设备还没来取（≤ 一个轮询周期）
	syncSyncing = "syncing" // 设备已取到新清单，正在下载/切换
	syncLatest  = "latest"  // 设备显示的就是最新内容
)

// syncState 判断设备的内容状态（调用方持有 s.mu）。key 是这台设备显示输入的当前指纹，
// 与它上次轮询时的不同，说明它该显示的内容变了而它还没来取。只看这台设备自己的输入：
// 改别的设备、改没被用到的模板都不影响它；时段计划到点切换、测试屏到期也能及时体现。
func (s *Server) syncState(deviceID string, online bool, key string) string {
	st, ok := s.sync[deviceID]
	switch {
	case !online:
		return syncOffline
	case !ok || st.m == nil || st.key != key:
		return syncWaiting
	case st.applied != st.m.Version:
		return syncSyncing
	}
	return syncLatest
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceAuth(w, r)
	if !ok {
		return
	}
	m, err := s.manifestFor(dev.ID)
	if err != nil {
		log.Printf("manifest build failed: device %s: %v", dev.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	applied := strings.Trim(strings.TrimSpace(r.Header.Get("If-None-Match")), `"`)
	s.mu.Lock()
	st := s.sync[dev.ID]
	st.applied = applied
	announce := applied != m.Version && st.announce != m.Version // 下载失败重试时会重复取同一版本，只记第一次
	if announce {
		st.announce = m.Version
	}
	s.sync[dev.ID] = st
	s.mu.Unlock()

	w.Header().Set("ETag", `"`+m.Version+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if applied == m.Version {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if announce {
		what := fmt.Sprintf("%d file(s)", len(m.Items))
		if m.Layout != nil {
			what += " + template overlay"
		}
		if m.Update != nil {
			what += ", agent update " + m.Update.Version
		}
		log.Printf("new content pushed: device %s, version %s, %s", dev.ID, m.Version, what)
	}
	writeJSON(w, m)
}

// manifestFor 返回设备此刻的清单：显示输入没变（指纹相同）就复用上次生成的，否则重新生成。
// 设备每 10 秒轮询一次，绝大多数时候什么都没变，不必每次都渲染整屏图、编码 PNG、算哈希。
func (s *Server) manifestFor(deviceID string) (*manifest.Manifest, error) {
	c, err := s.content(deviceID, s.now())
	if err != nil {
		return nil, err
	}
	key := c.key()
	s.mu.Lock()
	st := s.sync[deviceID]
	s.mu.Unlock()
	if st.key == key && st.m != nil {
		return st.m, nil
	}
	m, err := s.buildManifest(deviceID, c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	st = s.sync[deviceID]
	st.key, st.m = key, m
	s.sync[deviceID] = st
	s.mu.Unlock()
	return m, nil
}

// content 是决定一台设备此刻该显示什么的全部输入。清单只由它生成（buildManifest），
// 清单缓存与后台的"等待刷新"也只看它的指纹（key）——指纹不可能漏掉清单的输入。
type content struct {
	TestUntil time.Time         `json:"test_until,omitzero"` // 非零：显示测试卡
	Source    string            `json:"source"`              // test/override/schedule/global
	Template  store.Template    `json:"template"`
	Attrs     map[string]string `json:"attrs"`
	Mirror    bool              `json:"mirror"`
	Media     []mediaFile       `json:"media"` // 媒体区的播放列表；模板没有媒体区时为空
	Update    *manifest.Update  `json:"update,omitempty"`
}

// mediaFile 是播放列表里的一个文件。带上大小与修改时间：同名文件被替换也算内容变了。
type mediaFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// content 汇总设备此刻的显示输入：一次读锁取齐状态，再读媒体文件的元数据。不渲染、不算哈希。
func (s *Server) content(deviceID string, now time.Time) (content, error) {
	var (
		c        content
		playlist []string
		found    bool
	)
	s.store.View(func(st *store.State) {
		c.Attrs = maps.Clone(st.DeviceAttrs[deviceID])
		c.Update = pendingUpdate(st, deviceID, now)
		if until := st.TestUntil[deviceID]; now.Before(until) {
			c.TestUntil, c.Source, found = until, "test", true
			return
		}
		disp := st.Displays[deviceID]
		c.Mirror, playlist = disp.Mirror, disp.Playlist
		c.Template, c.Source, found = resolveTemplate(st, disp, now.In(s.loc))
	})
	switch {
	case !found:
		return c, errors.New("no usable template (global default template missing)")
	case c.Source == "test":
		return c, nil
	}
	if _, ok := c.Template.MediaRegion(); ok {
		dir := s.deviceMediaDir(deviceID)
		names, err := orderPlaylist(dir, playlist)
		if err != nil {
			return c, err
		}
		for _, n := range names {
			if fi, err := os.Stat(filepath.Join(dir, n)); err == nil {
				c.Media = append(c.Media, mediaFile{Name: n, Size: fi.Size(), ModTime: fi.ModTime()})
			}
		}
	}
	return c, nil
}

// resolveTemplate 决定设备当前应显示的模板及来源：设备专属模板 > 时段计划命中 > 全局默认模板。
// 被引用的模板不能删（handleDeleteTemplate），最后一个模板也不能删，所以正常情况下总能找到。
func resolveTemplate(st *store.State, disp store.DisplayConfig, now time.Time) (store.Template, string, bool) {
	if t, ok := st.Templates[disp.TemplateID]; ok {
		return t, "override", true
	}
	if sc, hit := store.ActiveSchedule(st.Schedules, now); hit {
		if t, ok := st.Templates[sc.TemplateID]; ok {
			return t, "schedule", true
		}
	}
	t, ok := st.Templates[st.Global.TemplateID]
	return t, "global", ok
}

// key 是 content 的指纹。
func (c content) key() string {
	b, _ := json.Marshal(c) // map 按键排序输出，结果确定
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// buildManifest 由显示输入生成设备清单：测试卡，或模板（+ 媒体区播放列表），并附带待执行的程序更新。
//
// 模板有媒体区且媒体区有内容时下发 layout：清单条目就是媒体文件本身，模板的静态部分作为
// "媒体区挖空"的叠加图随 layout 下发，由设备端贴在画面上。模板没有媒体区、或媒体区还没放内容时，
// 把整块模板渲染成一张整屏图，这样"刚建好还没传内容"的设备显示的是版式而不是黑屏。
func (s *Server) buildManifest(deviceID string, c content) (*manifest.Manifest, error) {
	var (
		img    image.Image
		kind   string // 渲染图的种类：test 测试卡 / tpl 整屏模板 / ovl 叠加图
		media  []manifest.Item
		layout *manifest.Layout
	)
	if !c.TestUntil.IsZero() {
		card, err := s.renderer.RenderTestCard(manifest.CanvasW, manifest.CanvasH, deviceID, c.Attrs, c.TestUntil.In(s.loc))
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
	items := []manifest.Item{png}
	if layout != nil {
		items, layout.Overlay = media, png
	}
	return &manifest.Manifest{
		Version: manifest.Version(items, c.Update, layout),
		Items:   items,
		Layout:  layout,
		Update:  c.Update,
	}, nil
}

// renderedItem 把渲染结果存成 PNG（文件名内嵌内容哈希）并包装成清单条目，同时清掉这台设备的旧渲染图
// （模板改了、属性改了、或在测试卡/整屏图/叠加图之间切换后留下的）。
// Duration 为 0：渲染图要么是整屏静态图，要么是叠加图（不进播放列表）。
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
	if err := fsutil.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		return manifest.Item{}, err
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := e.Name(); n != name && !strings.HasPrefix(n, ".") { // 隐藏文件是别的请求正在写的
			os.Remove(filepath.Join(dir, n))
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

// ---- 心跳 ----

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
	// 持久化程序版本与 IP（仅变化时写盘）：服务端重启后升级状态仍可判断。
	if ip := clientIP(r); dev.AgentVersion != hb.AgentVersion || dev.IP != ip {
		if err := s.store.Update(func(st *store.State) {
			d := st.Devices[dev.ID]
			d.AgentVersion, d.IP = hb.AgentVersion, ip
			st.Devices[dev.ID] = d
		}); err != nil {
			log.Printf("saving version/IP of device %s failed: %v", dev.ID, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
