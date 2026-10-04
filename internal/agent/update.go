package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/agentpkg"
	"github.com/izzln/content-edge-display/internal/manifest"
)

// ErrRestartForUpdate 表示新版本已就位，进程应退出让 systemd 拉起新版本。
var ErrRestartForUpdate = errors.New("agent: restart required to apply update")

// 更新失败后的最小重试间隔，避免每次轮询都重下坏包。
const updateRetryInterval = 5 * time.Minute

// updateScriptTimeout 是包内 update.sh 的最长运行时间。
const updateScriptTimeout = 2 * time.Minute

// installLayout 描述 OTA 安装布局（见 deploy/agent/install-agent.sh）：
//
//	<root>/versions/<版本>/      整包解开（程序 + 脚本）
//	<root>/current -> versions/<版本>   systemd 从这里启动
//	<root>/previous -> versions/<版本>  回滚目标
//	<root>/pending-verify         新版本待确认（rollback-check.sh 计数）
//
// 程序和配套脚本在同一个版本目录里，一起更新、一起回滚。
type installLayout struct {
	root string
}

func (l installLayout) versionsDir() string        { return filepath.Join(l.root, "versions") }
func (l installLayout) versionDir(v string) string { return filepath.Join(l.versionsDir(), v) }
func (l installLayout) current() string            { return filepath.Join(l.root, "current") }
func (l installLayout) previous() string           { return filepath.Join(l.root, "previous") }
func (l installLayout) pendingVerify() string      { return filepath.Join(l.root, "pending-verify") }

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
				a.updateErr = fmt.Sprintf("update to %s failed: %v", cmd.Version, err)
				log.Printf("agent: %s", a.updateErr)
			}
		default:
			log.Printf("agent: ignoring unknown command %q", cmd.Type)
		}
	}
	return nil
}

// applyUpdate 下载校验整包、解到版本目录、执行包内 update.sh，成功后切换 current、写入 pending-verify，
// 然后要求重启。update.sh 失败则不切换，原因随心跳上报（后台设备列表可见）。
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
	fail := func(err error) error {
		a.updateFailedAt[cmd.Version] = time.Now()
		return err
	}

	dir := layout.versionDir(cmd.Version)
	if err := a.stagePackage(ctx, layout, cmd, dir); err != nil {
		return fail(err)
	}
	if err := runUpdateScript(ctx, dir, layout.root); err != nil {
		return fail(err)
	}

	// previous ← 当前 current；current ← 新版本目录。均为原子替换。
	curTarget, err := os.Readlink(layout.current())
	if err != nil {
		return fail(err)
	}
	if !filepath.IsAbs(curTarget) {
		curTarget = filepath.Join(layout.root, curTarget)
	}
	if err := symlinkAtomic(curTarget, layout.previous()); err != nil {
		return fail(err)
	}
	if err := symlinkAtomic(dir, layout.current()); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(layout.pendingVerify(),
		[]byte(fmt.Sprintf("version=%s\nattempts=0\n", cmd.Version)), 0o644); err != nil {
		return fail(err)
	}
	a.updateErr = ""
	log.Printf("agent: switched current -> %s (previous -> %s); restarting to apply update", dir, curTarget)
	return ErrRestartForUpdate
}

// stagePackage 下载整包（按清单的 sha256 校验）并解到 dir：先解到临时目录、检查齐全后再改名，
// 半截的版本目录不会出现。
func (a *Agent) stagePackage(ctx context.Context, layout installLayout, cmd manifest.Command, dir string) error {
	if err := os.MkdirAll(layout.versionsDir(), 0o755); err != nil {
		return err
	}
	pkg := filepath.Join(layout.versionsDir(), "."+cmd.Version+".tar.gz")
	defer os.Remove(pkg)
	if err := a.downloadFile(ctx, cmd.URL, cmd.SHA256, cmd.Size, pkg); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	f, err := os.Open(pkg)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := filepath.Join(layout.versionsDir(), "."+cmd.Version+".tmp")
	os.RemoveAll(tmp)
	if err := agentpkg.Extract(f, tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("unpack: %w", err)
	}
	if v, err := agentpkg.Version(tmp); err != nil || v != cmd.Version {
		os.RemoveAll(tmp)
		return fmt.Errorf("package version %q does not match the update command (%s): %v", v, cmd.Version, err)
	}
	for _, name := range []string{agentpkg.Binary, agentpkg.UpdateScript} {
		if _, err := os.Stat(filepath.Join(tmp, name)); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("package has no %s", name)
		}
	}
	os.RemoveAll(dir)
	return os.Rename(tmp, dir)
}

// runUpdateScript 以 root 执行版本目录里的 update.sh <安装目录>（安装 systemd 单元、刷新固定路径的脚本、
// 必要的系统调整）。输出写进日志；失败时把最后一行带进错误。
func runUpdateScript(ctx context.Context, dir, root string) error {
	ctx, cancel := context.WithTimeout(ctx, updateScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", filepath.Join(dir, agentpkg.UpdateScript), root)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			log.Printf("agent: update.sh: %s", line)
		}
	}
	if err != nil {
		lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
		return fmt.Errorf("update.sh failed (%v): %s", err, lines[len(lines)-1])
	}
	return nil
}

// commitUpdate 在新版本首个心跳成功后删除 pending-verify，确认本版本可用，并清掉 current、previous
// 以外的旧版本目录。
func (a *Agent) commitUpdate() {
	layout := installLayout{root: a.cfg.InstallDir}
	if layout.root == "" {
		return
	}
	if err := os.Remove(layout.pendingVerify()); err != nil {
		return
	}
	log.Printf("agent: version %s verified (pending-verify cleared)", Version)
	keep := map[string]bool{}
	for _, link := range []string{layout.current(), layout.previous()} {
		if target, err := filepath.EvalSymlinks(link); err == nil {
			keep[filepath.Base(target)] = true
		}
	}
	entries, _ := os.ReadDir(layout.versionsDir())
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(layout.versionsDir(), e.Name()))
		}
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
