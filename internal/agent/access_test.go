package agent

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/server"
)

func sshKey(comment string) string {
	var blob []byte
	for _, part := range [][]byte{[]byte("ssh-ed25519"), make([]byte, 32)} {
		blob = binary.BigEndian.AppendUint32(blob, uint32(len(part)))
		blob = append(blob, part...)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment
}

// 后台设置的 root 密码与 SSH 公钥随清单到达设备并落到系统上；没变化不重复执行；
// 公钥清空时恢复密码 SSH；失败随心跳上报。
func TestAccessSettingsApplied(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	os.MkdirAll(filepath.Dir(e.a.acc.sshdDropIn), 0o755) // 装了 sshd
	var calls []string
	var fail error
	e.a.acc.run = func(stdin, name string, args ...string) error {
		calls = append(calls, strings.TrimSpace(name+" "+strings.Join(args, " ")+" <"+stdin))
		return fail
	}
	step := func() {
		t.Helper()
		if err := e.a.step(ctx); err != nil {
			t.Fatal(err)
		}
	}

	step()
	if len(calls) != 0 {
		t.Fatalf("后台没设置时不应动系统：%v", calls)
	}

	key := sshKey("ops")
	body, _ := json.Marshal(map[string]any{"root_password": "Secret-123", "ssh_keys": []string{key}})
	if w := e.admin(t, "PUT", "/api/v1/admin/access", string(body)); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	step()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "chpasswd -e <root:$6$") || calls[1] != "systemctl try-reload-or-restart ssh <" {
		t.Fatalf("应写入密码哈希并重载 sshd：%q", calls)
	}
	if b, _ := os.ReadFile(e.a.acc.authorizedKeys); string(b) != key+"\n" {
		t.Fatalf("authorized_keys 应是后台的公钥：%q", b)
	}
	if b, _ := os.ReadFile(e.a.acc.sshdDropIn); !strings.Contains(string(b), "PasswordAuthentication no") {
		t.Fatal("有公钥时应只允许密钥登录")
	}

	calls = nil
	step()
	if len(calls) != 0 {
		t.Fatalf("没变化不应重复执行：%v", calls)
	}
	// 重启后（新代理、同一缓存目录）也不重复执行
	a2 := New(e.a.cfg, e.p)
	a2.acc, a2.identity, a2.registered = e.a.acc, e.a.identity, true
	if err := a2.step(ctx); err != nil || len(calls) != 0 {
		t.Fatalf("重启后不应重复执行：%v %v", err, calls)
	}

	// 清空公钥：恢复密码 SSH
	e.admin(t, "PUT", "/api/v1/admin/access", `{"ssh_keys":[]}`)
	step()
	if _, err := os.Stat(e.a.acc.sshdDropIn); !os.IsNotExist(err) {
		t.Fatal("公钥清空后应删除只允许密钥登录的配置")
	}
	if _, err := os.Stat(e.a.acc.authorizedKeys); !os.IsNotExist(err) {
		t.Fatal("公钥清空后 authorized_keys 也应清掉")
	}

	// 失败：随心跳上报，隔一会儿重试
	fail = errors.New("chpasswd: boom")
	e.admin(t, "PUT", "/api/v1/admin/access", `{"root_password":"Another-456","ssh_keys":[]}`)
	step()
	if e.a.accessErr == "" || !strings.Contains(e.a.accessErr, "boom") {
		t.Fatalf("失败原因应记下来随心跳上报：%q", e.a.accessErr)
	}
	if err := e.a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	var st []server.DeviceView
	json.Unmarshal(e.admin(t, "GET", "/api/v1/admin/devices", "").Body.Bytes(), &st)
	if st[0].Heartbeat == nil || !strings.Contains(st[0].Heartbeat.AccessError, "boom") {
		t.Fatal("后台应看到访问设置失败的原因")
	}
	fail, e.a.accessFailedAt = nil, time.Time{}
	step()
	if e.a.accessErr != "" {
		t.Fatalf("重试成功后应清掉错误：%q", e.a.accessErr)
	}
}
