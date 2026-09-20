package player

import (
	"bufio"
	"context"
	"encoding/json"
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

	ln net.Listener
}

func startFakeMPV(t *testing.T, socketPath, playlistPath string) *fakeMPV {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("fake mpv listen: %v", err)
	}
	f := &fakeMPV{ln: ln}
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
	if err := p.Load(items); err != nil {
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
	if err := p.Load(items); err != nil {
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

	if err := p.Load([]Item{{Path: "/media/a.png", Type: "image"}}); err != nil {
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

	if err := p.Load([]Item{{Path: "/media/a.png", Type: "image"}}); err != nil {
		t.Fatal(err)
	}
	p.ensureOnce()
	if err := p.Load(nil); err != nil {
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
