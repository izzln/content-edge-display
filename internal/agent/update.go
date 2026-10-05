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
	"github.com/izzln/content-edge-display/internal/fsutil"
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
//	<root>/failed-version               rollback-check.sh 回滚掉的版本：不再自动重试
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
func (l installLayout) failedVersion() string      { return l.path("failed-version") }

// rolledBack 返回 rollback-check.sh 回滚掉的版本（没有则为空）。
func (l installLayout) rolledBack() string {
	if l == "" {
		return ""
	}
	data, _ := os.ReadFile(l.failedVersion())
	return strings.TrimSpace(string(data))
}

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

// setUpdate 记下清单里待执行的程序更新。换了目标（或撤销了更新）时清掉上一次的失败记录；
// 撤销更新也清掉回滚记录（运营方已知晓）。
func (a *Agent) setUpdate(u *manifest.Update) {
	if u == nil || a.update == nil || u.Version != a.update.Version {
		a.updRetry.clear()
	}
	if u == nil && a.install != "" {
		os.Remove(a.install.failedVersion())
	}
	a.update = u
}

// applyPendingUpdate 执行待执行的程序更新。它不跟着清单变化走：失败后清单不变（设备收到 304），
// 按 updateRetryInterval 重试，直到成功或运营方撤销。成功时返回 ErrRestartForUpdate。
// 被 rollback-check.sh 回滚掉的版本不再自动重试（否则每次都是下载、切换、启动失败、回滚，屏幕反复黑），
// 运营方修好后换个版本号重新下发。
func (a *Agent) applyPendingUpdate(ctx context.Context) error {
	u := a.update
	switch {
	case u == nil || u.Version == Version:
		return nil
	case u.Version == a.install.rolledBack():
		a.updRetry.err = fmt.Sprintf("版本 %s 连续 3 次启动失败，已回滚；修好后请换个版本号重新下发", u.Version)
		return nil
	case !a.updRetry.due():
		return nil
	}
	err := a.applyUpdate(ctx, *u)
	if err == nil || errors.Is(err, ErrRestartForUpdate) {
		return err
	}
	a.updRetry.fail(fmt.Sprintf("update to %s failed: %v", u.Version, err))
	log.Printf("agent: %s (retry in %s)", a.updRetry.err, updateRetryInterval)
	return nil
}

// applyUpdate 下载校验整包、解到版本目录、执行包内 update.sh，成功后写入 pending-verify、切换 current，
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
	keepFeeding(func() { err = runUpdateScript(ctx, dir, a.cfg.ServerURL) })
	if err != nil {
		return err
	}

	// 先落盘 pending-verify 再切换：切换后任何时候断电，新版本都有回滚保护（切换前断电则旧版本照常启动、
	// 首个心跳就把它删掉）。previous ← 当前 current；current ← 新版本目录。均为原子替换。
	cur, err := filepath.EvalSymlinks(l.current())
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(l.pendingVerify(), []byte("0\n"), 0o644); err != nil {
		return err
	}
	os.Remove(l.failedVersion())
	if err := symlinkAtomic(cur, l.previous()); err != nil {
		return err
	}
	if err := symlinkAtomic(dir, l.current()); err != nil {
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
	// 包的内容由清单里的 sha256 担保（服务端上传时已核对过程序的平台与版本），这里只看齐不齐
	if err = agentpkg.Extract(f, tmp); err == nil {
		err = agentpkg.Check(tmp)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("unpack: %w", err)
	}
	os.RemoveAll(dir)
	return os.Rename(tmp, dir)
}

// runUpdateScript 以 root 执行版本目录 dir 里的 update.sh（安装目录由它从自己的位置推出；服务端地址经
// 环境变量 SERVER_URL 传入，离线依赖从那里装）。输出写进日志；失败时把最后一行带进错误。
func runUpdateScript(ctx context.Context, dir, serverURL string) error {
	ctx, cancel := context.WithTimeout(ctx, updateScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", filepath.Join(dir, agentpkg.UpdateScript))
	cmd.Dir, cmd.Env = dir, append(os.Environ(), "SERVER_URL="+serverURL)
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
	if err := os.Rename(tmp, link); err != nil {
		return err
	}
	fsutil.SyncDir(filepath.Dir(link))
	return nil
}
