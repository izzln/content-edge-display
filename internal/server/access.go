package server

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

// 设备访问凭据：root 密码与 SSH 公钥在后台统一设置，随清单下发（固定证书的 HTTPS + 设备签名，只有服务端能下发），
// 设备代理应用（agent/access.go）。人员变动删公钥、密码泄露重设，所有设备一个轮询周期内跟上，离线设备上线即跟上。
// 不放进程序包：程序包是公开构建的，不能带密码。

const minRootPasswordLen = 8

// deviceAccess 返回要下发给设备的访问凭据；后台从没设置过时为 nil（设备保持现状）。
func deviceAccess(a store.Access) *manifest.Access {
	if a.RootPasswordHash == "" && len(a.SSHKeys) == 0 {
		return nil
	}
	return &manifest.Access{RootHash: a.RootPasswordHash, SSHKeys: slices.Clone(a.SSHKeys)}
}

type accessView struct {
	RootPasswordSetAt *time.Time `json:"root_password_set_at,omitempty"`
	SSHKeys           []string   `json:"ssh_keys"`
}

// handleGetAccess 返回访问设置（从不返回密码哈希）。
func (s *Server) handleGetAccess(w http.ResponseWriter, r *http.Request) {
	var a store.Access
	s.store.View(func(st *store.State) { a = st.Access })
	v := accessView{SSHKeys: append([]string{}, a.SSHKeys...)}
	if !a.RootPasswordSetAt.IsZero() {
		v.RootPasswordSetAt = &a.RootPasswordSetAt
	}
	writeJSON(w, v)
}

// handlePutAccess 设置 SSH 公钥（整份替换），root_password 非空时同时重设 root 密码（服务端只存哈希）。
func (s *Server) handlePutAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RootPassword string   `json:"root_password"`
		SSHKeys      []string `json:"ssh_keys"`
	}
	if !decodeJSON(w, r, 64<<10, &req) {
		return
	}
	keys := []string{}
	for i, k := range req.SSHKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if err := checkSSHKey(k); err != nil {
			http.Error(w, fmt.Sprintf("第 %d 个公钥%v", i+1, err), http.StatusBadRequest)
			return
		}
		keys = append(keys, k)
	}
	var hash string
	if req.RootPassword != "" {
		if n := len([]rune(req.RootPassword)); n < minRootPasswordLen || n > 128 {
			http.Error(w, fmt.Sprintf("root 密码须为 %d~128 位", minRootPasswordLen), http.StatusBadRequest)
			return
		}
		hash = sha512Crypt(req.RootPassword, "")
	}
	now := s.now()
	if s.update(w, func(st *store.State) {
		st.Access.SSHKeys = keys
		if hash != "" {
			st.Access.RootPasswordHash, st.Access.RootPasswordSetAt = hash, now
		}
	}) {
		log.Printf("device access updated: %d SSH key(s)%s", len(keys), map[bool]string{true: ", root password changed"}[hash != ""])
		w.WriteHeader(http.StatusNoContent)
	}
}

// checkSSHKey 检查一行 authorized_keys 格式的公钥：类型 + base64 内容（内容里也写着同样的类型）+ 可选备注。
func checkSSHKey(k string) error {
	if strings.ContainsAny(k, "\r\n") {
		return fmt.Errorf("不能换行")
	}
	f := strings.Fields(k)
	if len(f) < 2 {
		return fmt.Errorf("格式不对：应形如 ssh-ed25519 AAAA… 备注")
	}
	switch {
	case f[0] == "ssh-ed25519", f[0] == "ssh-rsa", strings.HasPrefix(f[0], "ecdsa-sha2-"), strings.HasPrefix(f[0], "sk-"):
	default:
		return fmt.Errorf("类型 %q 不支持（用 ssh-ed25519 或 ssh-rsa）", f[0])
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) < 4+len(f[0]) || string(blob[4:4+len(f[0])]) != f[0] {
		return fmt.Errorf("内容不完整或已损坏（请整行复制 .pub 文件）")
	}
	return nil
}
