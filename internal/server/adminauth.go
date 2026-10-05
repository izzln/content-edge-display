package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/store"
)

// 后台认证：所有人共用一个管理口令（请求头 X-Admin-Token）。
//   - 服务端只存口令的 PBKDF2 哈希（state.json 的 admin），后台可以改口令；
//   - server.json 的 admin_token 是初始/找回口令：它被改成新值并重启后生效（syncAdminToken）；
//   - 猜口令按来源 IP 阶梯锁定，像手机锁屏：前几次不锁，之后越错锁越久（adminAuth.check）。

// pbkdf2Iter 是新哈希的迭代次数（记在哈希串里，验证按串里的走）。
var pbkdf2Iter = 600_000

func init() {
	// 测试里每建一个服务端都要算一次哈希（race 模式下约 2 秒）：调低次数，验证逻辑不变
	if testing.Testing() {
		pbkdf2Iter = 1000
	}
}

const (
	lockFreeTries = 5 // 前几个错误口令不锁
	maxSeenTokens = 64
)

// lockSteps：第 6 个错误口令起依次锁 1 分钟、5 分钟、15 分钟，之后每次 1 小时。
var lockSteps = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

func hashAdminToken(tok string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, tok, salt, pbkdf2Iter, 32)
	enc := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc(salt), enc(key))
}

func checkAdminToken(tok, stored string) bool {
	f := strings.Split(stored, "$")
	if len(f) != 4 || f[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(f[1])
	salt, err1 := base64.RawStdEncoding.DecodeString(f[2])
	want, err2 := base64.RawStdEncoding.DecodeString(f[3])
	if err != nil || err1 != nil || err2 != nil || iter <= 0 || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, tok, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// adminAuth 验证管理口令并按来源 IP 记录猜错的次数。
type adminAuth struct {
	mu    sync.Mutex
	hash  string   // 当前口令的哈希（与 state.json 同步）
	okSum [32]byte // 最近验证通过的口令的 sha256：每个请求都带口令，命中它就不必再算一遍 PBKDF2
	okSet bool
	ips   map[string]*ipFailures
}

type ipFailures struct {
	seen  map[[32]byte]bool // 猜过的错误口令：同一个错口令重复出现（如后台页面拿着旧口令自动刷新）不算新的一次
	count int
	until time.Time // 锁到什么时候
}

func newAdminAuth(hash string) *adminAuth {
	return &adminAuth{hash: hash, ips: map[string]*ipFailures{}}
}

// check 验证 ip 发来的口令。锁定期间一律不通过（口令对也不行），并返回还要等多久。
func (a *adminAuth) check(tok, ip string, now time.Time) (ok bool, wait time.Duration) {
	sum := sha256.Sum256([]byte(tok))
	a.mu.Lock()
	f := a.ips[ip]
	switch {
	case f != nil && now.Before(f.until):
		a.mu.Unlock()
		return false, f.until.Sub(now)
	case a.okSet && subtle.ConstantTimeCompare(sum[:], a.okSum[:]) == 1:
		delete(a.ips, ip)
		a.mu.Unlock()
		return true, 0
	case tok == "" || f != nil && f.seen[sum]:
		a.mu.Unlock() // 没带口令、或重复的错口令：不计数，也不再算哈希
		return false, 0
	}
	hash := a.hash
	a.mu.Unlock()

	good := checkAdminToken(tok, hash) // 慢，不持锁
	a.mu.Lock()
	defer a.mu.Unlock()
	if good {
		if a.hash == hash {
			a.okSum, a.okSet = sum, true
		}
		delete(a.ips, ip)
		return true, 0
	}
	if f = a.ips[ip]; f == nil {
		f = &ipFailures{seen: map[[32]byte]bool{}}
		a.ips[ip] = f
	}
	if f.seen[sum] {
		return false, 0 // 并发的几个请求带着同一个错口令：只算一次、只记一次
	}
	if len(f.seen) < maxSeenTokens {
		f.seen[sum] = true
	}
	f.count++
	if f.count <= lockFreeTries {
		log.Printf("admin auth failed from %s (%d wrong token(s))", ip, f.count)
		return false, 0
	}
	d := lockSteps[min(f.count-lockFreeTries, len(lockSteps))-1]
	f.until = now.Add(d)
	log.Printf("admin auth failed from %s (%d wrong tokens): locked out for %s", ip, f.count, d)
	return false, d
}

// set 换成新口令（哈希 hash）。旧口令立即失效。
func (a *adminAuth) set(tok, hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hash, a.okSum, a.okSet = hash, sha256.Sum256([]byte(tok)), true
}

// syncAdminToken 在启动时决定用哪个口令：server.json 的 admin_token 与上次记下的不同（第一次启动，
// 或有人在服务器上把它改成了新值来找回口令）就采用它；否则沿用后台改过的口令。
func (s *Server) syncAdminToken() error {
	var cur store.Admin
	s.store.View(func(st *store.State) { cur = st.Admin })
	if cur.TokenHash == "" || !checkAdminToken(s.cfg.AdminToken, cur.ConfigHash) {
		h := hashAdminToken(s.cfg.AdminToken)
		if err := s.store.Update(func(st *store.State) { st.Admin = store.Admin{TokenHash: h, ConfigHash: h} }); err != nil {
			return err
		}
		if cur.TokenHash != "" {
			log.Printf("admin token reset: using the new admin_token from server.json (the one set in the admin UI no longer works)")
		}
		cur.TokenHash = h
	}
	s.auth = newAdminAuth(cur.TokenHash)
	return nil
}

// handleGetAdminToken 返回当前口令的来源（server.json 还是后台改过）与修改时间。
func (s *Server) handleGetAdminToken(w http.ResponseWriter, r *http.Request) {
	var a store.Admin
	s.store.View(func(st *store.State) { a = st.Admin })
	out := map[string]any{"from_config": a.TokenHash == a.ConfigHash}
	if !a.ChangedAt.IsZero() && a.TokenHash != a.ConfigHash {
		out["changed_at"] = a.ChangedAt
	}
	writeJSON(w, out)
}

// handlePutAdminToken 改管理口令（请求本身已凭当前口令通过验证）。服务端只存哈希。
func (s *Server) handlePutAdminToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NewToken string `json:"new_token"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	if n := len(req.NewToken); n < 8 || n > 64 || strings.ContainsFunc(req.NewToken, func(c rune) bool { return c <= ' ' || c > '~' }) {
		http.Error(w, "新口令须为 8~64 个字符（字母、数字或符号，不含空格与中文）", http.StatusBadRequest)
		return
	}
	h := hashAdminToken(req.NewToken)
	now := s.now()
	if s.update(w, func(st *store.State) { st.Admin.TokenHash, st.Admin.ChangedAt = h, now }) {
		s.auth.set(req.NewToken, h)
		log.Printf("admin token changed in the admin UI (from %s)", clientIP(r))
		w.WriteHeader(http.StatusNoContent)
	}
}

// waitText 把等待时间写成"N 分钟"之类给人看的说法。
func waitText(d time.Duration) string {
	if d < 30*time.Second {
		return fmt.Sprintf("%d 秒", int(d.Seconds())+1)
	}
	return fmt.Sprintf("%d 分钟", int((d+time.Minute-1)/time.Minute))
}
