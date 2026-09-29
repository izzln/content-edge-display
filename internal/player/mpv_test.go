package player

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMPV 是一个最小的 mpv JSON IPC 桩：支持 get_property playlist / loadlist / stop。
type fakeMPV struct {
	mu        sync.Mutex
	playlist  []string
	loadlists int
	imageDur  string            // 最近一次 set_property image-display-duration 的值
	props     map[string]string // 最近一次 set_property 的其它属性值
	overlays  []string          // 收到的 overlay-add / overlay-remove 命令（原样拼接）

	ln net.Listener
}

func startFakeMPV(t *testing.T, socketPath, playlistPath string) *fakeMPV {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("fake mpv listen: %v", err)
	}
	f := &fakeMPV{ln: ln, props: map[string]string{}}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, playlistPath)
		}
	}()
	return f
}

func (f *fakeMPV) serve(conn net.Conn, playlistPath string) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var req struct {
			Command   []any `json:"command"`
			RequestID int64 `json:"request_id"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.Command) == 0 {
			continue
		}
		name, _ := req.Command[0].(string)
		resp := map[string]any{"request_id": req.RequestID, "error": "success"}

		switch name {
		case "get_property":
			if prop, _ := req.Command[1].(string); prop == "playlist" {
				f.mu.Lock()
				entries := make([]any, 0, len(f.playlist))
				for _, p := range f.playlist {
					entries = append(entries, map[string]any{"filename": p})
				}
				f.mu.Unlock()
				resp["data"] = entries
			}
		case "loadlist":
			// 真 mpv 会读取 m3u 文件；这里照做，以便断言推送的内容正确。
			data, _ := os.ReadFile(playlistPath)
			var files []string
			for _, line := range strings.Split(string(data), "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					files = append(files, line)
				}
			}
			f.mu.Lock()
			f.playlist = files
			f.loadlists++
			f.mu.Unlock()
		case "set_property":
			prop, _ := req.Command[1].(string)
			f.mu.Lock()
			if prop == "image-display-duration" {
				f.imageDur = fmt.Sprint(req.Command[2])
			} else {
				f.props[prop] = fmt.Sprint(req.Command[2])
			}
			f.mu.Unlock()
		case "overlay-add", "overlay-remove":
			parts := []string{name}
			for _, a := range req.Command[1:] {
				parts = append(parts, fmt.Sprint(a))
			}
			f.mu.Lock()
			f.overlays = append(f.overlays, strings.Join(parts, " "))
			f.mu.Unlock()
		case "stop":
			f.mu.Lock()
			f.playlist = nil
			f.mu.Unlock()
		}
		b, _ := json.Marshal(resp)
		conn.Write(append(b, '\n'))
	}
}

func (f *fakeMPV) state() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.playlist...), f.loadlists
}

func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

func newTestMPV(t *testing.T) (*MPV, string, string) {
	t.Helper()
	dir := t.TempDir() // 短路径：unix socket 有长度限制
	sock := filepath.Join(dir, "mpv.sock")
	playlist := filepath.Join(dir, "playlist.m3u")
	return NewMPV(sock, playlist, 10, nil), sock, playlist
}

// 回归测试：代理启动时 mpv 的 IPC 尚未就绪（Load 必然失败），
// 巡检必须在 mpv 就绪后把播放列表补推过去——否则屏幕会一直黑到内容变化为止。
func TestEnsureLoadsPlaylistAfterMpvBecomesReady(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ensureLoop(ctx)

	items := []Item{{Path: "/media/a.png", Type: "image", Duration: 10}, {Path: "/media/b.mp4", Type: "video"}}
	// mpv 尚未启动：Load 不应报错，且应把 m3u 落盘
	if err := p.Load(Scene{Items: items}); err != nil {
		t.Fatalf("Load before mpv is up must not fail: %v", err)
	}
	data, err := os.ReadFile(playlistPath)
	if err != nil || !strings.Contains(string(data), "/media/a.png") {
		t.Fatalf("playlist not written: %v %q", err, data)
	}

	// mpv 起来了（晚于 Load）——巡检应当自动补推
	fake := startFakeMPV(t, sock, playlistPath)
	waitFor(t, 10*time.Second, "ensureLoop 把播放列表推给 mpv", func() bool {
		pl, n := fake.state()
		return n > 0 && len(pl) == 2 && pl[0] == "/media/a.png" && pl[1] == "/media/b.mp4"
	})
}

// 列表已正确加载时不得重复推送，否则每个巡检周期都会打断播放。
func TestEnsureDoesNotReloadWhenAlreadyCorrect(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	fake := startFakeMPV(t, sock, playlistPath)

	items := []Item{{Path: "/media/a.png", Type: "image"}}
	if err := p.Load(Scene{Items: items}); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()
	if pl, n := fake.state(); n != 1 || len(pl) != 1 {
		t.Fatalf("expected exactly one loadlist, got n=%d playlist=%v", n, pl)
	}
	for i := 0; i < 5; i++ {
		p.ensureOnce()
	}
	if _, n := fake.state(); n != 1 {
		t.Fatalf("playlist already correct, must not re-push: loadlists=%d", n)
	}
}

// mpv 重启后列表会丢失，巡检必须重新推送。
func TestEnsureRecoversAfterMpvRestart(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	fake := startFakeMPV(t, sock, playlistPath)

	if err := p.Load(Scene{Items: []Item{{Path: "/media/a.png", Type: "image"}}}); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()
	if _, n := fake.state(); n != 1 {
		t.Fatalf("initial push missing: %d", n)
	}

	// 模拟 mpv 重启：播放列表清空
	fake.mu.Lock()
	fake.playlist = nil
	fake.mu.Unlock()

	p.ensureOnce()
	if pl, n := fake.state(); n != 2 || len(pl) != 1 {
		t.Fatalf("expected re-push after mpv restart, got n=%d playlist=%v", n, pl)
	}
}

func TestLoadEmptyStopsPlayback(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	fake := startFakeMPV(t, sock, playlistPath)

	if err := p.Load(Scene{Items: []Item{{Path: "/media/a.png", Type: "image"}}}); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()
	if err := p.Load(Scene{}); err != nil {
		t.Fatal(err)
	}
	if pl, _ := fake.state(); len(pl) != 0 {
		t.Fatalf("expected playback stopped, got %v", pl)
	}
	// 空列表不应触发巡检推送
	before := func() int { _, n := fake.state(); return n }()
	p.ensureOnce()
	if after := func() int { _, n := fake.state(); return n }(); after != before {
		t.Fatalf("empty desired list must not push: %d -> %d", before, after)
	}
}

func (f *fakeMPV) imageDuration() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageDur
}

// 图片展示时长必须以清单为准：服务端是控制面，运营方在后台改了要能生效，
// 而不是被每台设备本地配置里的值盖掉。
func TestImageDurationComesFromManifest(t *testing.T) {
	// 子测试名保持 ASCII 且简短：t.TempDir() 会带上用例名，unix socket 路径有长度限制。
	cases := []struct {
		name  string
		items []Item
		want  string
		desc  string
	}{
		{"single-image", []Item{{Path: "/m/a.png", Type: "image", Duration: 10}}, "inf",
			"单张静态图用 inf，避免每 N 秒重载一次造成闪烁"},
		{"image-and-video", []Item{{Path: "/m/a.png", Type: "image", Duration: 7}, {Path: "/m/b.mp4", Type: "video"}}, "7",
			"多条目时取清单里的时长，而不是构造时传入的配置值 30"},
		{"video-only", []Item{{Path: "/m/a.mp4", Type: "video"}, {Path: "/m/b.mp4", Type: "video"}}, "30",
			"纯视频列表里没有图片时长，保持兜底值"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, sock, playlistPath := newTestMPV(t)
			p.imageDur = "30" // 模拟 agent.json 里的兜底值
			fake := startFakeMPV(t, sock, playlistPath)
			if err := p.Load(Scene{Items: c.items}); err != nil {
				t.Fatal(err)
			}
			p.ensureOnce()
			if got := fake.imageDuration(); got != c.want {
				t.Fatalf("%s：推送给 mpv 的 image-display-duration = %q，期望 %q", c.desc, got, c.want)
			}
			// mpv 启动参数也应使用同一个值
			if c.want != "" && p.snapshotImageDur() != c.want {
				t.Fatalf("启动参数用的时长 = %q，期望 %q", p.snapshotImageDur(), c.want)
			}
		})
	}
}

// 同一个值不应每轮巡检都重复下发。
func TestImageDurationAppliedOnce(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	fake := startFakeMPV(t, sock, playlistPath)
	if err := p.Load(Scene{Items: []Item{{Path: "/m/a.png", Type: "image", Duration: 5}, {Path: "/m/b.mp4", Type: "video"}}}); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()
	if fake.imageDuration() != "5" {
		t.Fatalf("首次应下发 5，实际 %q", fake.imageDuration())
	}
	fake.mu.Lock()
	fake.imageDur = "（未再下发）"
	fake.mu.Unlock()
	for i := 0; i < 3; i++ {
		p.ensureOnce()
	}
	if fake.imageDuration() != "（未再下发）" {
		t.Fatalf("时长未变时不应重复下发，实际又收到 %q", fake.imageDuration())
	}
}

func (f *fakeMPV) prop(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.props[name]
}

func (f *fakeMPV) overlayCmds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.overlays...)
}

func TestMarginRatios(t *testing.T) {
	ovl := &Overlay{Path: "/c/o.bgra", W: 1440, H: 900}
	cases := []struct {
		name        string
		scene       Scene
		l, r, tp, b float64
	}{
		{"right-half", Scene{Overlay: ovl, Media: Rect{720, 0, 720, 900}, CanvasW: 1440, CanvasH: 900}, 0.5, 0, 0, 0},
		{"left-half", Scene{Overlay: ovl, Media: Rect{0, 0, 720, 900}, CanvasW: 1440, CanvasH: 900}, 0, 0.5, 0, 0},
		{"inset", Scene{Overlay: ovl, Media: Rect{360, 225, 720, 450}, CanvasW: 1440, CanvasH: 900}, 0.25, 0.25, 0.25, 0.25},
		{"fullscreen", Scene{Media: Rect{0, 0, 720, 900}, CanvasW: 1440, CanvasH: 900}, 0, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, r, tp, b := marginRatios(c.scene)
			if l != c.l || r != c.r || tp != c.tp || b != c.b {
				t.Fatalf("留白比例 = %v/%v/%v/%v，期望 %v/%v/%v/%v", l, r, tp, b, c.l, c.r, c.tp, c.b)
			}
		})
	}
}

// 模板叠加：媒体区留白、叠加层与 cover 都要真正下发给 mpv。
func TestOverlayAppliedAndReappliedAfterRestart(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	fake := startFakeMPV(t, sock, playlistPath)

	scene := Scene{
		Items:   []Item{{Path: "/m/a.mp4", Type: "video"}},
		Overlay: &Overlay{Path: "/c/ovl.bgra", W: 1440, H: 900},
		Media:   Rect{720, 0, 720, 900},
		CanvasW: 1440, CanvasH: 900,
	}
	if err := p.Load(scene); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()

	if got := fake.prop("video-margin-ratio-left"); got != "0.500000" {
		t.Fatalf("左留白 = %q，期望 0.500000", got)
	}
	if got := fake.prop("video-margin-ratio-right"); got != "0.000000" {
		t.Fatalf("右留白 = %q，期望 0.000000", got)
	}
	want := "overlay-add 0 0 0 /c/ovl.bgra 0 bgra 1440 900 5760"
	if cmds := fake.overlayCmds(); len(cmds) != 1 || cmds[0] != want {
		t.Fatalf("叠加层命令 = %v，期望 [%s]", cmds, want)
	}

	// 同一画面重复巡检不应反复下发
	for i := 0; i < 3; i++ {
		p.ensureOnce()
	}
	if cmds := fake.overlayCmds(); len(cmds) != 1 {
		t.Fatalf("版面未变时不应重复下发，实际 %d 次", len(cmds))
	}

	// mpv 重启会丢掉叠加层（OSD 不跨进程存活），巡检必须重贴
	p.setLoaded(false)
	p.mu.Lock()
	p.layoutApplied = false
	p.mu.Unlock()
	p.ensureOnce()
	if cmds := fake.overlayCmds(); len(cmds) != 2 || cmds[1] != want {
		t.Fatalf("mpv 重启后应重贴叠加层，实际 %v", cmds)
	}
}

// 从“模板叠加”切回“整屏播放”必须撤掉叠加层并清零留白，否则画面永远停在右半边。
func TestSwitchToFullscreenRemovesOverlay(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	fake := startFakeMPV(t, sock, playlistPath)

	if err := p.Load(Scene{
		Items:   []Item{{Path: "/m/a.mp4", Type: "video"}},
		Overlay: &Overlay{Path: "/c/ovl.bgra", W: 1440, H: 900},
		Media:   Rect{720, 0, 720, 900},
		CanvasW: 1440, CanvasH: 900,
	}); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()

	if err := p.Load(Scene{Items: []Item{{Path: "/m/tpl.png", Type: "image", Duration: 10}}}); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()
	if got := fake.prop("video-margin-ratio-left"); got != "0.000000" {
		t.Fatalf("切回整屏后左留白 = %q，期望 0.000000", got)
	}
	cmds := fake.overlayCmds()
	if len(cmds) != 2 || cmds[1] != "overlay-remove 0" {
		t.Fatalf("切回整屏应撤掉叠加层，实际 %v", cmds)
	}
}

// 只换播放列表、版面不变时，叠加层不该被重贴（重贴会在屏幕上闪一下）。
func TestPlaylistChangeKeepsOverlay(t *testing.T) {
	p, sock, playlistPath := newTestMPV(t)
	fake := startFakeMPV(t, sock, playlistPath)

	ovl := &Overlay{Path: "/c/ovl.bgra", W: 1440, H: 900}
	base := Scene{Overlay: ovl, Media: Rect{720, 0, 720, 900}, CanvasW: 1440, CanvasH: 900}

	first := base
	first.Items = []Item{{Path: "/m/a.mp4", Type: "video"}}
	if err := p.Load(first); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()

	second := base
	second.Items = []Item{{Path: "/m/a.mp4", Type: "video"}, {Path: "/m/b.mp4", Type: "video"}}
	if err := p.Load(second); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()
	p.ensureOnce()
	if cmds := fake.overlayCmds(); len(cmds) != 1 {
		t.Fatalf("播放列表变化不应重贴叠加层，实际 %d 次：%v", len(cmds), cmds)
	}
	if pl, _ := fake.state(); len(pl) != 2 {
		t.Fatalf("新播放列表未生效：%v", pl)
	}
}
