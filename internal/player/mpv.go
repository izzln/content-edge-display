package player

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ensureInterval 是“确保 mpv 真的在放期望内容”的巡检间隔。
const ensureInterval = 2 * time.Second

// MPV 通过 JSON IPC 驱动 mpv 全屏循环播放。
//
// 播放列表落地为 m3u 文件，有两条送达路径：mpv 启动时的 --playlist 参数，
// 以及运行中经 IPC `loadlist` 热更新。两条都可能落空——代理启动时 mpv 刚被拉起、
// IPC socket 尚未建立，而 --playlist 只在 mpv 启动那一刻求值——所以由 ensureLoop
// 持续比对 mpv 实际加载的列表与期望列表，不一致就重推，直到一致为止。
// 没有这层巡检，一次落空就会黑屏到下次内容变化为止（服务端在清单未变时只回 304）。
//
// 图片展示时长完全以**清单**为准（服务端是控制面，运营方在后台改了模板要能生效）。
// mpv 的 m3u 不支持逐条目选项，所以同一份清单里的图片共用一个时长；
// 单张静态图（模板模式恒为此情形）用 inf，避免每 N 秒重新加载一次造成闪烁。
//
// 模板叠加（Scene.Overlay 非 nil）时的合成方式：用 --video-margin-ratio-* 把画面压进媒体区、
// --panscan=1 让内容撑满该区（超出部分裁掉），再用 overlay-add 把叠加层贴在上面。
// 三者都是 mpv 运行期可改的属性/命令，所以属性文字变化只需重发一张小 PNG，视频不用重编码。
//
// M1 简化：loadlist replace 会立即切换列表（“播完当前项再切”留待后续版本）。
type MPV struct {
	socketPath   string
	playlistPath string
	extraArgs    []string
	reqID        atomic.Int64

	mu         sync.Mutex
	desired    Scene
	imageDur   string // mpv --image-display-duration 的值：秒数或 "inf"；收到清单前为空
	loaded     bool   // 最近一次巡检是否确认 mpv 已加载期望列表（仅用于控制日志噪音）
	appliedDur string // 已同步给 mpv 的图片时长；与 imageDur 不同即需要（重新）下发
	// 已同步给 mpv 的版面指纹（留白比例 + 叠加图 + 输出分辨率）。
	// 指纹里带上分辨率，显示屏换了模式也能自动重贴一张匹配的叠加图。
	appliedLayout string

	wake chan struct{}
}

func NewMPV(socketPath, playlistPath string, extraArgs []string) *MPV {
	return &MPV{
		socketPath:   socketPath,
		playlistPath: playlistPath,
		extraArgs:    extraArgs,
		wake:         make(chan struct{}, 1),
	}
}

// overlayID 是 mpv OSD 叠加层编号；只用一层。
const overlayID = "0"

// marginRatios 把媒体区矩形换算成 mpv 的四边留白比例。
// 没有叠加层（整屏播放）或画布尺寸非法时全为 0。
func marginRatios(s Scene) (left, right, top, bottom float64) {
	if s.Overlay == nil || s.CanvasW <= 0 || s.CanvasH <= 0 ||
		s.Media.W <= 0 || s.Media.H <= 0 {
		return 0, 0, 0, 0
	}
	w, h := float64(s.CanvasW), float64(s.CanvasH)
	return float64(s.Media.X) / w,
		(w - float64(s.Media.X+s.Media.W)) / w,
		float64(s.Media.Y) / h,
		(h - float64(s.Media.Y+s.Media.H)) / h
}

func ratio(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// imageDurationFor 由清单条目决定图片展示时长。
// 单张图片 → inf（静止画面无需反复重载）；多条目 → 取首张图片的时长。
func imageDurationFor(items []Item) string {
	if len(items) == 1 && items[0].Type == "image" {
		return "inf"
	}
	for _, it := range items {
		if it.Type == "image" && it.Duration > 0 {
			return strconv.Itoa(it.Duration)
		}
	}
	return "" // 没有图片，保持现值
}

func (p *MPV) Start(ctx context.Context) error {
	go p.supervise(ctx)
	go p.ensureLoop(ctx)
	return nil
}

// mpvEnv 返回启动 mpv 的环境变量。
//
// mpv 启动时会探测 Wayland，XDG_RUNTIME_DIR 未设置或指向不存在的目录就报
// "XDG_RUNTIME_DIR is invalid or not set"。systemd 系统服务没有登录会话、不会设置它；
// 新装的 systemd 单元里已经补上，但 OTA 只换二进制、改不了单元文件，所以这里再兜一次：
// 缺失或无效时指向 mpv IPC socket 所在的运行目录（本服务专用，已存在）。
func mpvEnv(runtimeDir string) []string {
	env := os.Environ()
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return env
		}
	}
	return append(env, "XDG_RUNTIME_DIR="+runtimeDir)
}

// supervise 拉起 mpv 并在其退出后自动重启（进程级守护的最内层）。
func (p *MPV) supervise(ctx context.Context) {
	for ctx.Err() == nil {
		l, r, t, b := marginRatios(p.snapshot())
		args := []string{
			"--idle=yes",
			"--force-window=yes",
			"--fullscreen",
			"--no-osc",
			"--no-input-default-bindings",
			"--osd-level=0",
			"--no-terminal",
			"--loop-playlist=inf",
			"--hwdec=auto-safe",
			// 内容一律撑满播放区（超出部分裁掉），且一律静音。
			"--panscan=1",
			"--no-audio",
			// 启动即带上媒体区留白，避免新起的 mpv 先整屏闪一下再被巡检压回去。
			"--video-margin-ratio-left=" + ratio(l),
			"--video-margin-ratio-right=" + ratio(r),
			"--video-margin-ratio-top=" + ratio(t),
			"--video-margin-ratio-bottom=" + ratio(b),
			"--input-ipc-server=" + p.socketPath,
		}
		if dur := p.snapshotImageDur(); dur != "" {
			args = append(args, "--image-display-duration="+dur)
		}
		if _, err := os.Stat(p.playlistPath); err == nil {
			args = append(args, "--playlist="+p.playlistPath)
		}
		args = append(args, p.extraArgs...)

		cmd := exec.CommandContext(ctx, "mpv", args...)
		cmd.Env = mpvEnv(filepath.Dir(p.socketPath))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		log.Printf("player(mpv): starting mpv")
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		log.Printf("player(mpv): mpv exited (%v), restarting in 2s", err)
		// mpv 重启后列表、时长与叠加层状态都未知（overlay-add 不会跨进程存活），
		// 让巡检重新确认一次。
		p.setLoaded(false)
		p.mu.Lock()
		p.appliedDur, p.appliedLayout = "", ""
		p.mu.Unlock()
		p.signal()
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// ensureLoop 周期性确认 mpv 实际加载的播放列表与期望一致，不一致则重推。
// mpv 未就绪（IPC 连不上）时静默跳过，下个周期再试。
func (p *MPV) ensureLoop(ctx context.Context) {
	ticker := time.NewTicker(ensureInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.wake:
		}
		p.ensureOnce()
	}
}

func (p *MPV) ensureOnce() {
	want := p.snapshot()
	if len(want.Items) == 0 {
		return // 期望黑屏/待机：Load 已发过 stop，无需巡检
	}
	ok, err := p.playlistMatches(want.Items)
	if err != nil {
		p.setLoaded(false) // mpv 未就绪或 IPC 异常，下个周期再试
		return
	}
	// 时长与版面要先于列表生效，否则切换后的第一帧会沿用旧设置（整屏闪一下）。
	p.applyImageDuration()
	p.applyLayout(want)
	if ok {
		if !p.wasLoaded() {
			log.Printf("player(mpv): playlist confirmed loaded (%d item(s))", len(want.Items))
			p.setLoaded(true)
		}
		return
	}
	if _, err := p.command("loadlist", p.playlistPath, "replace"); err != nil {
		p.setLoaded(false)
		return
	}
	log.Printf("player(mpv): pushed playlist to mpv (%d item(s))", len(want.Items))
}

// applyLayout 把媒体区留白与模板叠加层同步给运行中的 mpv。
// 任一步失败就保持“未同步”，由下个巡检周期重试。
//
// 每轮都会问一次 mpv 的实际输出分辨率：叠加图按模板画布（1440×900）渲染，而显示屏真实输出
// 可能是别的尺寸（面板 EDID、内核没吃下 video= 参数、换了块屏），overlay-add 的坐标用的是
// 实际输出分辨率，尺寸对不上整个画面就错位。
func (p *MPV) applyLayout(want Scene) {
	l, r, t, b := marginRatios(want)
	fp := strings.Join([]string{ratio(l), ratio(r), ratio(t), ratio(b)}, "|")

	var raw string
	var w, h int
	if o := want.Overlay; o != nil {
		var err error
		if w, h, err = p.osdSize(); err != nil || w <= 0 || h <= 0 {
			return // mpv 未就绪，下个周期再试
		}
		fp += fmt.Sprintf("|%s|%dx%d", o.PNG, w, h)
	}
	p.mu.Lock()
	done := p.appliedLayout == fp
	p.mu.Unlock()
	if done {
		return
	}
	if want.Overlay != nil {
		var err error
		if raw, err = rasterize(want.Overlay.PNG, w, h); err != nil {
			// 叠加图用不了时宁可整屏播放也不要黑屏/错位的画面。
			log.Printf("player(mpv): overlay unusable (%v), falling back to fullscreen", err)
			raw = ""
		}
	}

	for _, m := range []struct {
		prop string
		v    float64
	}{
		{"video-margin-ratio-left", l},
		{"video-margin-ratio-right", r},
		{"video-margin-ratio-top", t},
		{"video-margin-ratio-bottom", b},
	} {
		if _, err := p.command("set_property", m.prop, ratio(m.v)); err != nil {
			return
		}
	}
	if raw != "" {
		// overlay-add <id> <x> <y> <file> <offset> <fmt> <w> <h> <stride>
		if _, err := p.command("overlay-add", overlayID, 0, 0, raw, 0, "bgra", w, h, w*4); err != nil {
			return
		}
	} else if _, err := p.command("overlay-remove", overlayID); err != nil {
		return
	}
	p.mu.Lock()
	p.appliedLayout = fp
	p.mu.Unlock()
	if raw != "" {
		log.Printf("player(mpv): overlay applied at %dx%d, media area %dx%d at (%d,%d) of canvas %dx%d",
			w, h, want.Media.W, want.Media.H, want.Media.X, want.Media.Y, want.CanvasW, want.CanvasH)
	} else {
		log.Printf("player(mpv): fullscreen playback (no overlay)")
	}
}

// applyImageDuration 把期望的图片展示时长同步给运行中的 mpv（值没变就不重复下发）。
func (p *MPV) applyImageDuration() {
	p.mu.Lock()
	dur, done := p.imageDur, p.imageDur == p.appliedDur
	p.mu.Unlock()
	if done || dur == "" {
		return
	}
	if _, err := p.command("set_property", "image-display-duration", dur); err != nil {
		return // mpv 未就绪，下个周期再试
	}
	p.mu.Lock()
	p.appliedDur = dur
	p.mu.Unlock()
	log.Printf("player(mpv): image-display-duration = %s", dur)
}

// osdSize 返回 mpv 当前的输出分辨率（overlay-add 的坐标系）。
func (p *MPV) osdSize() (int, int, error) {
	data, err := p.command("get_property", "osd-dimensions")
	if err != nil {
		return 0, 0, err
	}
	m, ok := data.(map[string]any)
	if !ok {
		return 0, 0, fmt.Errorf("mpv ipc: unexpected osd-dimensions %T", data)
	}
	w, _ := m["w"].(float64)
	h, _ := m["h"].(float64)
	return int(w), int(h), nil
}

// Stats 读取 mpv 的实际解码方式与输出分辨率，随心跳上报。
// 运营方在后台就能看出哪台设备退化成了软解，不用登录设备翻日志。
func (p *MPV) Stats() Stats {
	var st Stats
	if data, err := p.command("get_property", "hwdec-current"); err == nil {
		st.HWDec, _ = data.(string)
	}
	if w, h, err := p.osdSize(); err == nil {
		st.OutputW, st.OutputH = w, h
	}
	return st
}

// playlistMatches 比对 mpv 当前播放列表与期望条目（按顺序比文件路径）。
func (p *MPV) playlistMatches(want []Item) (bool, error) {
	data, err := p.command("get_property", "playlist")
	if err != nil {
		return false, err
	}
	entries, ok := data.([]any)
	if !ok {
		return false, fmt.Errorf("mpv ipc: unexpected playlist property %T", data)
	}
	if len(entries) != len(want) {
		return false, nil
	}
	for i, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			return false, nil
		}
		if name, _ := m["filename"].(string); name != want[i].Path {
			return false, nil
		}
	}
	return true, nil
}

func (p *MPV) Load(scene Scene) error {
	items := scene.Items
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, it := range items {
		b.WriteString(it.Path)
		b.WriteByte('\n')
	}
	tmp := p.playlistPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.playlistPath); err != nil {
		return err
	}

	p.mu.Lock()
	p.desired = scene
	p.desired.Items = append([]Item(nil), items...)
	p.loaded = false
	if dur := imageDurationFor(items); dur != "" {
		p.imageDur = dur
	}
	p.mu.Unlock()

	if len(items) == 0 {
		if _, err := p.command("stop"); err != nil {
			log.Printf("player(mpv): stop failed (%v); playlist emptied on disk", err)
		}
		return nil
	}
	// 立即推一次（mpv 已在运行时可秒级生效）；失败也无妨，ensureLoop 会持续重试。
	p.signal()
	return nil
}

func (p *MPV) NowPlaying() string {
	data, err := p.command("get_property", "path")
	if err != nil {
		return ""
	}
	s, _ := data.(string)
	return s
}

func (p *MPV) snapshotImageDur() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.imageDur
}

func (p *MPV) snapshot() Scene {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.desired
	s.Items = append([]Item(nil), p.desired.Items...)
	return s
}

func (p *MPV) wasLoaded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loaded
}

func (p *MPV) setLoaded(v bool) {
	p.mu.Lock()
	p.loaded = v
	p.mu.Unlock()
}

// signal 唤醒 ensureLoop 立即巡检一次（不阻塞）。
func (p *MPV) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// command 对每次调用独立建连，避免维护常驻连接与事件流解析。
func (p *MPV) command(cmd ...any) (any, error) {
	conn, err := net.DialTimeout("unix", p.socketPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	id := p.reqID.Add(1)
	req, err := json.Marshal(map[string]any{"command": cmd, "request_id": id})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		var resp struct {
			RequestID int64  `json:"request_id"`
			Error     string `json:"error"`
			Data      any    `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &resp) != nil {
			continue // 事件或无法解析的行，跳过
		}
		if resp.RequestID != id {
			continue
		}
		if resp.Error != "" && resp.Error != "success" {
			return nil, fmt.Errorf("mpv ipc: %s", resp.Error)
		}
		return resp.Data, nil
	}
	return nil, fmt.Errorf("mpv ipc: no response (%v)", scanner.Err())
}
