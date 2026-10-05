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

const (
	updateRetryInterval = 5 * time.Minute  // 更新失败后隔多久再试（不必每次轮询都重下坏包）
	updateScriptTimeout = 15 * time.Minute // 包内 update.sh 的最长运行时间（可能要安装新增的依赖，H3 上解包慢）
)

// installLayout 是 OTA 安装布局的根目录（由 install-agent.sh 建立）：
//
//	<root>/versions/<版本>/            整包解开（程序 + 脚本）
//	<root>/current -> versions/<版本>   systemd 从这里启动
//	<root>/previous -> versions/<版本>  回滚目标
//	<root>/pending-verify               新版本待确认（rollback-check.sh 计数）
//
// 程序和配套脚本在同一个版本目录里，一起更新、一起回滚。
type installLayout string

func (l installLayout) path(elem ...string) string {
	return filepath.Join(append([]string{string(l)}, elem...)...)
}
func (l installLayout) versionDir(v string) string { return l.path("versions", v) }
func (l installLayout) current() string            { return l.path("current") }
func (l installLayout) previous() string           { return l.path("previous") }
func (l installLayout) pendingVerify() string      { return l.path("pending-verify") }

// detectInstallLayout 从正在运行的程序的位置（<root>/versions/<版本>/display-agent）推出安装根目录；
// 不是按 OTA 布局安装的（开发机上直接运行）返回空。
func detectInstallLayout() installLayout {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return ""
	}
	versions := filepath.Dir(filepath.Dir(exe))
	if filepath.Base(versions) != "versions" {
		return ""
	}
	return installLayout(filepath.Dir(versions))
}

// setUpdate 记下清单里待执行的程序更新。换了目标（或撤销了更新）时清掉上一次的失败记录。
func (a *Agent) setUpdate(u *manifest.Update) {
	if u == nil || a.update == nil || u.Version != a.update.Version {
		a.updateErr, a.updateFailedAt = "", time.Time{}
	}
	a.update = u
}

// applyPendingUpdate 执行待执行的程序更新。它不跟着清单变化走：失败后清单不变（设备收到 304），
// 按 updateRetryInterval 重试，直到成功或运营方撤销。成功时返回 ErrRestartForUpdate。
func (a *Agent) applyPendingUpdate(ctx context.Context) error {
	u := a.update
	if u == nil || u.Version == Version || time.Since(a.updateFailedAt) < updateRetryInterval {
		return nil
	}
	err := a.applyUpdate(ctx, *u)
	if err == nil || errors.Is(err, ErrRestartForUpdate) {
		return err
	}
	a.updateFailedAt = time.Now()
	a.updateErr = fmt.Sprintf("update to %s failed: %v", u.Version, err)
	log.Printf("agent: %s (retry in %s)", a.updateErr, updateRetryInterval)
	return nil
}

// applyUpdate 下载校验整包、解到版本目录、执行包内 update.sh，成功后切换 current、写入 pending-verify，
// 然后要求重启。update.sh 失败则不切换，原因随心跳上报（后台设备列表可见）。
func (a *Agent) applyUpdate(ctx context.Context, u manifest.Update) error {
	l := a.install
	if l == "" {
		return errors.New("this agent is not installed under the OTA layout (run install-agent.sh)")
	}
	dir := l.versionDir(u.Version)
	if err := a.stagePackage(ctx, l, u, dir); err != nil {
		return err
	}
	var err error
	keepFeeding(func() { err = runUpdateScript(ctx, dir, string(l)) })
	if err != nil {
		return err
	}

	// previous ← 当前 current；current ← 新版本目录。均为原子替换。
	cur, err := filepath.EvalSymlinks(l.current())
	if err != nil {
		return err
	}
	if err := symlinkAtomic(cur, l.previous()); err != nil {
		return err
	}
	if err := symlinkAtomic(dir, l.current()); err != nil {
		return err
	}
	if err := os.WriteFile(l.pendingVerify(), []byte("0\n"), 0o644); err != nil {
		return err
	}
	log.Printf("agent: switched current -> %s (previous -> %s); restarting to apply update", dir, cur)
	return ErrRestartForUpdate
}

// stagePackage 下载整包（按清单的 sha256 校验）并解到 dir：先解到临时目录、检查齐全后再改名，
// 半截的版本目录不会出现。
func (a *Agent) stagePackage(ctx context.Context, l installLayout, u manifest.Update, dir string) error {
	versions := l.path("versions")
	pkg := filepath.Join(versions, "."+u.Version+".tar.gz")
	defer os.Remove(pkg)
	if err := a.downloadFile(ctx, u.URL, u.SHA256, u.Size, pkg); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	f, err := os.Open(pkg)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := filepath.Join(versions, "."+u.Version+".tmp")
	os.RemoveAll(tmp)
	err = agentpkg.Extract(f, tmp)
	if err == nil {
		var v string
		if v, err = agentpkg.Check(tmp); err == nil && v != u.Version {
			err = fmt.Errorf("package version %q does not match the update (%s)", v, u.Version)
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("unpack: %w", err)
	}
	os.RemoveAll(dir)
	return os.Rename(tmp, dir)
}

// runUpdateScript 以 root 执行版本目录里的 update.sh <安装目录>。输出写进日志；失败时把最后一行带进错误。
func runUpdateScript(ctx context.Context, dir, root string) error {
	ctx, cancel := context.WithTimeout(ctx, updateScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", filepath.Join(dir, agentpkg.UpdateScript), root)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	lines := strings.Split(string(bytes.TrimSpace(out)), "\n")
	for _, line := range lines {
		if line != "" {
			log.Printf("agent: update.sh: %s", line)
		}
	}
	if err != nil {
		return fmt.Errorf("update.sh failed (%v): %s", err, lines[len(lines)-1])
	}
	return nil
}

// commitUpdate 在新版本首个心跳成功后删除 pending-verify，确认本版本可用，并清掉 current、previous
// 以外的旧版本目录。
func (a *Agent) commitUpdate() {
	l := a.install
	if l == "" || os.Remove(l.pendingVerify()) != nil {
		return
	}
	log.Printf("agent: version %s verified (pending-verify cleared)", Version)
	keep := map[string]bool{}
	for _, link := range []string{l.current(), l.previous()} {
		if target, err := filepath.EvalSymlinks(link); err == nil {
			keep[filepath.Base(target)] = true
		}
	}
	entries, _ := os.ReadDir(l.path("versions"))
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(l.versionDir(e.Name()))
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
