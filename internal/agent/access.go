package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
)

// 访问凭据（root 密码、SSH 公钥）由后台统一设置、随清单下发（服务端 access.go），这里把它落到系统上。
// 与内容切换互不影响：失败不耽误播放，原因随心跳上报，隔一会儿重试。

const accessRetryInterval = time.Minute

// accessTarget 是访问凭据落地的位置与执行命令的方式（测试里换成临时目录与桩）。
type accessTarget struct {
	authorizedKeys string // root 的 authorized_keys：整份由后台决定
	sshdDropIn     string // 有公钥时只允许密钥登录
	run            func(stdin, name string, args ...string) error
}

var systemAccess = accessTarget{
	authorizedKeys: "/root/.ssh/authorized_keys",
	// sshd 取第一次出现的值：文件名排最前，压过 Armbian 主配置里的 PermitRootLogin yes
	sshdDropIn: "/etc/ssh/sshd_config.d/00-display-agent.conf",
	run:        runCommand,
}

const sshdKeysOnly = `# display-agent: SSH keys are configured in the admin UI, so only key logins are allowed
# (this file is removed when the keys are cleared). The local console still uses the root password.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
`

func runCommand(stdin, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v %s", name, err, bytes.TrimSpace(out))
	}
	return nil
}

// accessFPPath 记着上次成功应用的访问凭据的指纹：重启后不必再应用一遍（会重载 sshd）。
func (a *Agent) accessFPPath() string { return filepath.Join(a.cfg.CacheDir, "access-applied") }

func (a *Agent) loadAccessFP() string {
	b, _ := os.ReadFile(a.accessFPPath())
	return string(b)
}

// applyAccess 把清单里的访问凭据落到系统上；与上次成功应用的相同（按指纹比）就什么都不做。
// 清单里没有（后台从没设置过）时保持系统现状。
func (a *Agent) applyAccess() {
	acc := a.access
	if acc == nil || !a.accRetry.due() {
		return
	}
	b, _ := json.Marshal(acc)
	sum := sha256.Sum256(b)
	fp := hex.EncodeToString(sum[:8])
	if fp == a.accessFP {
		return
	}
	if err := a.accessSys.apply(*acc); err != nil {
		a.accRetry.fail("access settings: " + err.Error())
		log.Printf("agent: applying access settings failed: %v (retry in %s)", err, accessRetryInterval)
		return
	}
	a.accRetry.clear()
	a.accessFP = fp
	if err := fsutil.WriteFile(a.accessFPPath(), []byte(fp), 0o600); err != nil {
		log.Printf("agent: %v", err)
	}
	root := "unchanged"
	if acc.RootHash != "" {
		root = "set"
	}
	log.Printf("agent: access settings applied (root password %s, %d SSH key(s))", root, len(acc.SSHKeys))
}

func (t accessTarget) apply(acc manifest.Access) error {
	if acc.RootHash != "" {
		if err := t.run("root:"+acc.RootHash+"\n", "chpasswd", "-e"); err != nil {
			return err
		}
	}
	// 先写好公钥再关密码登录，顺序反了会有一段时间谁都进不去
	if len(acc.SSHKeys) > 0 {
		if err := os.MkdirAll(filepath.Dir(t.authorizedKeys), 0o700); err != nil {
			return err
		}
		if err := fsutil.WriteFile(t.authorizedKeys, []byte(strings.Join(acc.SSHKeys, "\n")+"\n"), 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(t.authorizedKeys); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(filepath.Dir(t.sshdDropIn)); err != nil {
		return nil // 没装 sshd
	}
	want := ""
	if len(acc.SSHKeys) > 0 {
		want = sshdKeysOnly
	}
	if old, _ := os.ReadFile(t.sshdDropIn); string(old) == want {
		return nil
	}
	if want == "" {
		if err := os.Remove(t.sshdDropIn); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := fsutil.WriteFile(t.sshdDropIn, []byte(want), 0o644); err != nil {
		return err
	}
	return t.run("", "systemctl", "try-reload-or-restart", "ssh")
}
