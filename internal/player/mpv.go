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
	"strings"
	"sync/atomic"
	"time"
)

// MPV 通过 JSON IPC 驱动 mpv 全屏循环播放。
//
// 播放列表落地为 m3u 文件：mpv 运行中经 IPC `loadlist` 热更新；
// mpv 被拉起/重启时经 --playlist 参数自动恢复，两条路径共用同一文件。
// M1 简化：图片展示时长为全局 --image-display-duration（不支持逐条目时长）；
// loadlist replace 会立即切换列表（"播完当前项再切"留待后续版本）。
type MPV struct {
	socketPath   string
	playlistPath string
	imageDur     int
	extraArgs    []string
	reqID        atomic.Int64
}

func NewMPV(socketPath, playlistPath string, imageDurationS int, extraArgs []string) *MPV {
	return &MPV{
		socketPath:   socketPath,
		playlistPath: playlistPath,
		imageDur:     imageDurationS,
		extraArgs:    extraArgs,
	}
}

func (p *MPV) Start(ctx context.Context) error {
	go p.supervise(ctx)
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
			fmt.Sprintf("--image-display-duration=%d", p.imageDur),
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
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
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

	var err error
	if len(items) == 0 {
		_, err = p.command("stop")
	} else {
		_, err = p.command("loadlist", p.playlistPath, "replace")
	}
	if err != nil {
		// mpv 可能尚未起来或正在重启：列表文件已就位，重启时会自动加载。
		log.Printf("player(mpv): IPC load failed (%v); playlist file updated, will apply on mpv (re)start", err)
	}
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
