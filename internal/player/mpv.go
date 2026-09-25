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
// 图片展示时长以**清单**为准（服务端是控制面，运营方在后台改了要能生效），
// 配置里的 image_duration_s 只作为收到第一份清单之前的兜底。
// mpv 的 m3u 不支持逐条目选项，所以同一份清单里的图片共用一个时长；
// 单张静态图（模板模式恒为此情形）用 inf，避免每 N 秒重新加载一次造成闪烁。
//
// M1 简化：loadlist replace 会立即切换列表（“播完当前项再切”留待后续版本）。
type MPV struct {
	socketPath   string
	playlistPath string
	extraArgs    []string
	reqID        atomic.Int64

	mu         sync.Mutex
	desired    []Item
	imageDur   string // mpv --image-display-duration 的值：秒数或 "inf"
	loaded     bool   // 最近一次巡检是否确认 mpv 已加载期望列表（仅用于控制日志噪音）
	durApplied bool   // 本轮是否已把图片时长同步给 mpv

	wake chan struct{}
}

func NewMPV(socketPath, playlistPath string, imageDurationS int, extraArgs []string) *MPV {
	return &MPV{
		socketPath:   socketPath,
		playlistPath: playlistPath,
		imageDur:     strconv.Itoa(imageDurationS),
		extraArgs:    extraArgs,
		wake:         make(chan struct{}, 1),
	}
}

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

// supervise 拉起 mpv 并在其退出后自动重启（进程级守护的最内层）。
func (p *MPV) supervise(ctx context.Context) {
	for ctx.Err() == nil {
		args := []string{
			"--idle=yes",
			"--force-window=yes",
			"--fullscreen",
			"--no-osc",
			"--no-input-default-bindings",
			"--osd-level=0",
			"--no-terminal",
			"--loop-playlist=inf",
			"--image-display-duration=" + p.snapshotImageDur(),
			"--hwdec=auto-safe",
			"--input-ipc-server=" + p.socketPath,
		}
		if _, err := os.Stat(p.playlistPath); err == nil {
			args = append(args, "--playlist="+p.playlistPath)
		}
		args = append(args, p.extraArgs...)

		cmd := exec.CommandContext(ctx, "mpv", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		log.Printf("player(mpv): starting mpv")
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		log.Printf("player(mpv): mpv exited (%v), restarting in 2s", err)
		// mpv 重启后列表与时长状态都未知，让巡检重新确认一次。
		p.setLoaded(false)
		p.mu.Lock()
		p.durApplied = false
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
	if len(want) == 0 {
		return // 期望黑屏/待机：Load 已发过 stop，无需巡检
	}
	ok, err := p.playlistMatches(want)
	if err != nil {
		p.setLoaded(false) // mpv 未就绪或 IPC 异常，下个周期再试
		return
	}
	// 时长要先于列表生效，否则切换后的第一张图会沿用旧时长。
	p.applyImageDuration()
	if ok {
		if !p.wasLoaded() {
			log.Printf("player(mpv): playlist confirmed loaded (%d item(s))", len(want))
			p.setLoaded(true)
		}
		return
	}
	if _, err := p.command("loadlist", p.playlistPath, "replace"); err != nil {
		p.setLoaded(false)
		return
	}
	log.Printf("player(mpv): pushed playlist to mpv (%d item(s))", len(want))
}

// applyImageDuration 把期望的图片展示时长同步给运行中的 mpv（每轮只做一次）。
func (p *MPV) applyImageDuration() {
	p.mu.Lock()
	dur, done := p.imageDur, p.durApplied
	p.mu.Unlock()
	if done || dur == "" {
		return
	}
	if _, err := p.command("set_property", "image-display-duration", dur); err != nil {
		return // mpv 未就绪，下个周期再试
	}
	p.mu.Lock()
	p.durApplied = true
	p.mu.Unlock()
	log.Printf("player(mpv): image-display-duration = %s", dur)
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

func (p *MPV) Load(items []Item) error {
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
	p.desired = append([]Item(nil), items...)
	p.loaded = false
	if dur := imageDurationFor(items); dur != "" && dur != p.imageDur {
		p.imageDur, p.durApplied = dur, false
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

func (p *MPV) snapshot() []Item {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Item(nil), p.desired...)
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
