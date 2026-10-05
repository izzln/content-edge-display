package server

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// testSSHKey 造一个格式正确的 ed25519 公钥行。
func testSSHKey(comment string, seed byte) string {
	var blob []byte
	for _, part := range [][]byte{[]byte("ssh-ed25519"), make([]byte, 32)} {
		blob = binary.BigEndian.AppendUint32(blob, uint32(len(part)))
		blob = append(blob, part...)
	}
	blob[len(blob)-1] = seed
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment
}

// 访问凭据：后台设置、服务端只存哈希且从不回传；随清单下发，变化即改变清单版本。
func TestDeviceAccess(t *testing.T) {
	s, h := newAdminTestServer(t)
	before := deviceManifest(t, h)
	if before.Access != nil {
		t.Fatal("后台没设置过时不下发访问凭据（设备保持现状）")
	}

	for _, c := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"root_password": "short", "ssh_keys": []string{}}, "8~128"},
		{map[string]any{"ssh_keys": []string{"ssh-ed25519 notbase64!"}}, "第 1 个公钥"},
		{map[string]any{"ssh_keys": []string{"ssh-dss AAAA x"}}, "不支持"},
	} {
		if w := do2(t, h, adminReq("PUT", "/api/v1/admin/access", c.body)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%v：应被拒并提示 %q，得到 %d %s", c.body, c.want, w.Code, w.Body.String())
		}
	}

	key := testSSHKey("ops@laptop", 1)
	do(t, h, adminReq("PUT", "/api/v1/admin/access", map[string]any{"root_password": "Secret-123", "ssh_keys": []string{" " + key + " ", ""}}), http.StatusNoContent)
	w := do(t, h, adminReq("GET", "/api/v1/admin/access", nil), http.StatusOK)
	if strings.Contains(w.Body.String(), "$6$") || !strings.Contains(w.Body.String(), "root_password_set_at") {
		t.Fatalf("不应回传密码哈希，应显示设置时间：%s", w.Body.String())
	}
	var view struct {
		SSHKeys []string `json:"ssh_keys"`
	}
	json.Unmarshal(w.Body.Bytes(), &view)
	if len(view.SSHKeys) != 1 || view.SSHKeys[0] != key {
		t.Fatalf("公钥应去掉空白与空行后保存：%q", view.SSHKeys)
	}

	m := deviceManifest(t, h)
	if m.Access == nil || !strings.HasPrefix(m.Access.RootHash, "$6$") || len(m.Access.SSHKeys) != 1 || m.Version == before.Version {
		t.Fatalf("清单应带上访问凭据并换版本：%+v", m.Access)
	}
	hash := m.Access.RootHash

	// 只改公钥、不填密码：密码保持不变
	do(t, h, adminReq("PUT", "/api/v1/admin/access", map[string]any{"ssh_keys": []string{key, testSSHKey("new", 2)}}), http.StatusNoContent)
	if m2 := deviceManifest(t, h); m2.Access.RootHash != hash || len(m2.Access.SSHKeys) != 2 || m2.Version == m.Version {
		t.Fatalf("只改公钥时密码不变、版本变化：%+v", m2.Access)
	}
	if state(s).Access.RootPasswordHash != hash {
		t.Fatal("状态里应保留原密码哈希")
	}
}
