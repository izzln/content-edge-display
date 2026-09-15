package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// ErrRestartForUpdate 表示新版本已就位，进程应退出让 systemd 拉起新二进制。
var ErrRestartForUpdate = errors.New("agent: restart required to apply update")

// 更新失败后的最小重试间隔，避免每次轮询都重下坏包。
const updateRetryInterval = 5 * time.Minute

// installLayout 描述 OTA 安装布局（见 deploy/install-agent.sh）。
type installLayout struct {
	root string
}

func (l installLayout) versionsDir() string   { return filepath.Join(l.root, "versions") }
func (l installLayout) current() string       { return filepath.Join(l.root, "current") }
func (l installLayout) previous() string      { return filepath.Join(l.root, "previous") }
func (l installLayout) pendingVerify() string { return filepath.Join(l.root, "pending-verify") }
func (l installLayout) binary(version string) string {
	return filepath.Join(l.versionsDir(), "display-agent-"+version)
}

// installed 判断是否按 OTA 布局安装（current 必须是符号链接）。
func (l installLayout) installed() bool {
	if l.root == "" {
		return false
	}
	fi, err := os.Lstat(l.current())
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// handleCommands 处理清单附带的指令；返回 ErrRestartForUpdate 时调用方应退出进程。
func (a *Agent) handleCommands(ctx context.Context, cmds []manifest.Command) error {
	for _, cmd := range cmds {
		switch cmd.Type {
		case "update":
			if err := a.applyUpdate(ctx, cmd); err != nil {
				if errors.Is(err, ErrRestartForUpdate) {
					return err
				}
				log.Printf("agent: update to %s failed: %v", cmd.Version, err)
			}
		default:
			log.Printf("agent: ignoring unknown command %q", cmd.Type)
		}
	}
	return nil
}

// applyUpdate 下载校验新版本、切换 current 符号链接、写入 pending-verify，然后要求重启。
func (a *Agent) applyUpdate(ctx context.Context, cmd manifest.Command) error {
	if cmd.Version == "" || cmd.Version == Version {
		return nil
	}
	layout := installLayout{root: a.cfg.InstallDir}
	if !layout.installed() {
		log.Printf("agent: update command for %s ignored: not installed under OTA layout (install_dir=%q)", cmd.Version, a.cfg.InstallDir)
		return nil
	}
	if last, ok := a.updateFailedAt[cmd.Version]; ok && time.Since(last) < updateRetryInterval {
		return nil
	}

	dst := layout.binary(cmd.Version)
	if fi, err := os.Stat(dst); err != nil || fi.Size() != cmd.Size {
		if err := os.MkdirAll(layout.versionsDir(), 0o755); err != nil {
			return err
		}
		if err := a.downloadFile(ctx, cmd.URL, cmd.SHA256, cmd.Size, dst); err != nil {
			a.updateFailedAt[cmd.Version] = time.Now()
			return fmt.Errorf("download: %w", err)
		}
	} else if sum, err := fileSHA256(dst); err != nil || sum != cmd.SHA256 {
		os.Remove(dst)
		a.updateFailedAt[cmd.Version] = time.Now()
		return fmt.Errorf("existing file checksum mismatch, removed; will retry")
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return err
	}

	// previous ← 当前 current；current ← 新版本。均为原子替换。
	curTarget, err := os.Readlink(layout.current())
	if err != nil {
		return err
	}
	if !filepath.IsAbs(curTarget) {
		curTarget = filepath.Join(layout.root, curTarget)
	}
	if err := symlinkAtomic(curTarget, layout.previous()); err != nil {
		return err
	}
	if err := symlinkAtomic(dst, layout.current()); err != nil {
		return err
	}
	if err := os.WriteFile(layout.pendingVerify(),
		[]byte(fmt.Sprintf("version=%s\nattempts=0\n", cmd.Version)), 0o644); err != nil {
		return err
	}
	log.Printf("agent: switched current -> %s (previous -> %s); restarting to apply update", dst, curTarget)
	return ErrRestartForUpdate
}

// commitUpdate 在新版本首个心跳成功后删除 pending-verify，确认本版本可用。
func (a *Agent) commitUpdate() {
	layout := installLayout{root: a.cfg.InstallDir}
	if layout.root == "" {
		return
	}
	if err := os.Remove(layout.pendingVerify()); err == nil {
		log.Printf("agent: version %s verified (pending-verify cleared)", Version)
	}
}

func symlinkAtomic(target, link string) error {
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}
