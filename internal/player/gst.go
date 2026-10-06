package player

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// gstScript 是播放进程（Python + GStreamer），随代理二进制分发、OTA 即可更新。
// 画面怎么叠、硬解怎么走、淡入淡出怎么做，见该文件开头的说明。
//
//go:embed gstplayer.py
var gstScript []byte

// GST 驱动播放进程 gstplayer.py：DRM/KMS 直接出画面，视频走 H3 的硬件解码器（cedrus）。
//
// 代理只管"该放什么"：每次 Load 把完整画面交给播放进程，排期、淡入淡出都由它自己做。
// 播放进程退出或卡住（状态查询不回）就重启，重启后重新下发当前画面——内容没变时服务端只回 304，
// 不重发的话，一次重启就会黑屏到下次内容变化。
type GST struct {
	python        string   // 解释器
	env           []string // 额外环境变量（测试用）
	script        string   // 运行时写出的脚本路径
	width, height int      // 显示输出分辨率：显示模式、叠加图光栅化都按它

	callTimeout   time.Duration // 一次请求等多久回复
	watchInterval time.Duration // 看门狗（兼状态查询）间隔
	restartDelay  time.Duration // 播放进程退出后隔多久重启；接连起不来（如 HDMI 没接）时翻倍，最长 maxRestartDelay

	mu         sync.Mutex
	desired    *Scene
	brightness int // 期望的画面亮度（百分比）；播放进程重启后补发
	proc       *gstProc
	hwdec      string // 看门狗最近一次问到的解码方式
	paused     bool
	runCancel  context.CancelFunc // 结束当前这一轮播放进程（暂停时用）
	wake       chan struct{}      // 恢复播放
}

// NewGST 创建播放器；脚本写在 dir 下，显示输出分辨率为 width×height。
func NewGST(dir string, width, height int) *GST {
	return &GST{
		python: "python3", script: filepath.Join(dir, "gstplayer.py"), width: width, height: height,
		callTimeout: 5 * time.Second, watchInterval: 10 * time.Second, restartDelay: 2 * time.Second,
		wake: make(chan struct{}, 1), brightness: 100,
	}
}

func (p *GST) Start(ctx context.Context) error {
	// 每次启动都重写，保证播放进程与当前程序版本一致。
	if err := os.WriteFile(p.script, gstScript, 0o755); err != nil {
		return fmt.Errorf("write player script: %w", err)
	}
	go p.supervise(ctx)
	return nil
}

// Load 记下期望画面并交给播放进程；播放进程不在时，等它（重新）起来后补发。
func (p *GST) Load(scene Scene) error {
	s := scene
	s.Items = append([]Item(nil), scene.Items...)
	p.mu.Lock()
	p.desired = &s
	proc := p.proc
	p.mu.Unlock()
	if proc == nil {
		return nil
	}
	return p.send(proc, s)
}

// SetBrightness 记下期望亮度并交给播放进程；播放进程不在时，等它（重新）起来后补发。
func (p *GST) SetBrightness(percent int) {
	p.mu.Lock()
	p.brightness = percent
	proc := p.proc
	p.mu.Unlock()
	if proc != nil {
		p.sendBrightness(proc, percent)
	}
}

func (p *GST) sendBrightness(proc *gstProc, percent int) {
	if err := proc.call(map[string]any{"cmd": "brightness", "percent": percent}, nil); err != nil {
		log.Printf("player(gst): set brightness: %v", err)
	}
}

// SetPaused 暂停时结束播放进程（DRM 随之释放，内核恢复控制台显示），恢复时重新拉起并补发当前画面。
func (p *GST) SetPaused(paused bool) {
	p.mu.Lock()
	changed := p.paused != paused
	p.paused = paused
	cancel := p.runCancel
	p.mu.Unlock()
	if !changed {
		return
	}
	if paused {
		log.Printf("player(gst): pausing playback (player process stopped, display released)")
		if cancel != nil {
			cancel()
		}
		return
	}
	log.Printf("player(gst): resuming playback")
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// waitUnpaused 等到不处于暂停；ctx 取消返回 false。
func (p *GST) waitUnpaused(ctx context.Context) bool {
	for {
		p.mu.Lock()
		paused := p.paused
		p.mu.Unlock()
		if !paused {
			return true
		}
		select {
		case <-p.wake:
		case <-ctx.Done():
			return false
		}
	}
}

// Stats 返回看门狗最近一次问到的解码方式与输出分辨率（不阻塞，心跳随时可调）。
func (p *GST) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{HWDec: p.hwdec, OutputW: p.width, OutputH: p.height}
}

// send 把画面转成播放进程的 load 请求。叠加图在这里按输出分辨率光栅化成 BGRA，
// 播放进程直接读原始像素，不用自己解 PNG、缩放。
func (p *GST) send(proc *gstProc, s Scene) error {
	items := make([]map[string]any, 0, len(s.Items))
	for _, it := range s.Items {
		items = append(items, map[string]any{"path": it.Path, "type": it.Type, "duration": it.Duration})
	}
	req := map[string]any{"cmd": "load", "items": items, "overlay": nil}
	if s.OverlayPNG != "" {
		raw, hole, err := rasterize(s.OverlayPNG, p.width, p.height, s.Media)
		if err != nil {
			// 叠加图用不了时宁可整屏播放，也不要黑屏
			log.Printf("player(gst): overlay unusable (%v), playing fullscreen", err)
		} else {
			req["overlay"], req["media"] = raw, []int{hole.X, hole.Y, hole.W, hole.H} // 都是输出坐标
		}
	}
	if err := proc.call(req, nil); err != nil {
		return fmt.Errorf("player(gst): load: %w", err)
	}
	log.Printf("player(gst): loaded %d item(s)", len(s.Items))
	return nil
}

// maxRestartDelay 是播放进程接连起不来时重启间隔的上限；跑稳了（超过 stableRun）就恢复到 restartDelay。
const maxRestartDelay, stableRun = time.Minute, time.Minute

// supervise 拉起播放进程并在其退出后自动重启（进程级守护的最内层）。
func (p *GST) supervise(ctx context.Context) {
	delay := p.restartDelay
	for ctx.Err() == nil {
		if !p.waitUnpaused(ctx) {
			return
		}
		runCtx, cancel := context.WithCancel(ctx)
		p.mu.Lock()
		if p.paused { // 刚好在这期间被暂停
			p.mu.Unlock()
			cancel()
			continue
		}
		p.runCancel = cancel
		p.mu.Unlock()
		started := time.Now()
		p.runOnce(runCtx)
		p.mu.Lock()
		p.runCancel = nil
		paused := p.paused
		p.mu.Unlock()
		cancel()
		if ctx.Err() != nil {
			return
		}
		if paused {
			continue
		}
		if time.Since(started) >= stableRun {
			delay = p.restartDelay
		}
		log.Printf("player(gst): player process exited, restarting in %s", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(2*delay, maxRestartDelay)
	}
}

func (p *GST) runOnce(ctx context.Context) {
	cmd := exec.CommandContext(ctx, p.python, "-u", p.script)
	cmd.Env = append(os.Environ(), p.env...)
	cmd.Stderr = &lineLogger{} // 播放进程的日志经代理的 logger 输出，格式与时间戳一致
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("player(gst): %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("player(gst): %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("player(gst): cannot start %s: %v", p.python, err)
		return
	}
	proc := &gstProc{stdin: stdin, timeout: p.callTimeout, pending: map[int64]chan json.RawMessage{}, dead: make(chan struct{})}
	go func() {
		proc.read(stdout)
		close(proc.dead)
	}()
	defer func() {
		p.mu.Lock()
		if p.proc == proc {
			p.proc = nil
		}
		p.mu.Unlock()
	}()

	if err := proc.call(map[string]any{"cmd": "config", "width": p.width, "height": p.height}, nil); err != nil {
		log.Printf("player(gst): configure failed: %v", err)
		cmd.Process.Kill()
		cmd.Wait()
		return
	}
	p.mu.Lock()
	p.proc = proc
	scene, brightness := p.desired, p.brightness
	p.mu.Unlock()
	if brightness != 100 {
		p.sendBrightness(proc, brightness)
	}
	if scene != nil {
		if err := p.send(proc, *scene); err != nil {
			log.Print(err)
		}
	}

	// 看门狗：播放进程卡死（GStreamer 死锁、驱动挂住）时不会自己退出，状态查询不回就杀掉重来。
	// 问到的解码方式顺便缓存下来，心跳直接读。
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(p.watchInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				var st struct {
					HWDec string `json:"hwdec"`
				}
				if err := proc.call(map[string]any{"cmd": "stats"}, &st); err != nil {
					log.Printf("player(gst): player not responding (%v), killing it", err)
					cmd.Process.Kill()
					return
				}
				p.mu.Lock()
				p.hwdec = st.HWDec
				p.mu.Unlock()
			}
		}
	}()
	err = cmd.Wait()
	close(stop)
	if ctx.Err() == nil {
		log.Printf("player(gst): player process ended: %v", err)
	}
}

// lineLogger 把写进来的字节按行交给 log。
type lineLogger struct{ buf []byte }

func (l *lineLogger) Write(b []byte) (int, error) {
	l.buf = append(l.buf, b...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		log.Print(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
}

// gstProc 是一个运行中的播放进程：按行收发 JSON，回复按 id 对上请求。
type gstProc struct {
	stdin   io.Writer
	timeout time.Duration
	wmu     sync.Mutex // 串行写 stdin；与 mu 分开，写阻塞时不耽误分发回复
	mu      sync.Mutex
	lastID  int64
	pending map[int64]chan json.RawMessage
	dead    chan struct{}
}

// call 发出请求并等回复；out 非 nil 时把回复解码进去。
func (g *gstProc) call(req map[string]any, out any) error {
	ch := make(chan json.RawMessage, 1)
	g.mu.Lock()
	g.lastID++
	id := g.lastID
	g.pending[id] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
	}()
	req["id"] = id
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	g.wmu.Lock()
	_, err = g.stdin.Write(append(line, '\n'))
	g.wmu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r := <-ch:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(r, &e); e.Error != "" {
			return errors.New(e.Error)
		}
		if out != nil {
			return json.Unmarshal(r, out)
		}
		return nil
	case <-g.dead:
		return errors.New("player process exited")
	case <-time.After(g.timeout):
		return errors.New("timeout")
	}
}

// read 分发播放进程的输出：带 id 的是回复；事件（playing/error/crop）只用于诊断，播放进程自己已记日志。
func (g *gstProc) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var msg struct {
			ID *int64 `json:"id"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		g.mu.Lock()
		ch := g.pending[*msg.ID]
		g.mu.Unlock()
		if ch != nil {
			ch <- append(json.RawMessage(nil), sc.Bytes()...)
		}
	}
}
