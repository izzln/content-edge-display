package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 像手机锁屏：前 5 个错误口令不锁，之后 1 分钟、5 分钟、15 分钟、1 小时……；同一个错口令重复不算；
// 锁定期间口令对也不放行；成功清零；各 IP 互不影响。
func TestAdminLockout(t *testing.T) {
	a := newAdminAuth(hashAdminToken("Right123"))
	now := time.Unix(1_000_000, 0)
	const ip = "10.0.0.9"
	try := func(tok string) (bool, time.Duration) { return a.check(tok, ip, now) }

	for range 10 {
		if ok, wait := try("stale-token"); ok || wait != 0 {
			t.Fatal("重复的同一个错口令不应计数，也不应锁定")
		}
	}
	try("")
	for i := 2; i <= 5; i++ {
		if _, wait := try("guess" + string(rune('a'+i))); wait != 0 {
			t.Fatalf("第 %d 个错误口令不应锁定", i)
		}
	}
	if _, wait := try("guess-6"); wait != time.Minute {
		t.Fatalf("第 6 个错误口令应锁 1 分钟：%s", wait)
	}
	if ok, wait := try("Right123"); ok || wait <= 0 {
		t.Fatal("锁定期间口令对也不放行")
	}
	if ok, _ := a.check("Right123", "10.0.0.10", now); !ok {
		t.Fatal("别的 IP 不受影响")
	}
	for i, want := range []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour} {
		now = now.Add(2 * time.Hour) // 等锁定过去
		if _, wait := try("more-" + string(rune('a'+i))); wait != want {
			t.Fatalf("第 %d 个错误口令应锁 %s，得到 %s", 7+i, want, wait)
		}
	}
	now = now.Add(2 * time.Hour)
	if ok, _ := try("Right123"); !ok {
		t.Fatal("锁定过后口令对应通过")
	}
	if _, wait := try("fresh-wrong"); wait != 0 {
		t.Fatal("成功后计数应清零")
	}
}

// 错误计数不是永久的：锁定过去且 24 小时没再猜错就清零，维护时删掉这样的记录。
func TestAdminFailuresExpire(t *testing.T) {
	a := newAdminAuth(hashAdminToken("Right123"))
	now := time.Unix(1_000_000, 0)
	for i := range 6 {
		a.check("wrong-"+string(rune('a'+i)), "10.0.0.9", now)
	}
	now = now.Add(forgetAfter)
	if _, wait := a.check("wrong-again", "10.0.0.9", now); wait != 0 {
		t.Fatalf("安静 24 小时后应从头计数，不应锁定：%s", wait)
	}
	a.check("other", "10.0.0.8", now)
	a.prune(now.Add(forgetAfter))
	if len(a.ips) != 0 {
		t.Fatalf("维护时应删掉过期记录：%d", len(a.ips))
	}
}

// 刚重启时后台页面并发好几个请求：只算一次 PBKDF2，其余等它算完直接命中。
func TestAdminAuthHashesOnce(t *testing.T) {
	a := newAdminAuth(hashAdminToken("Right123"))
	var calls atomic.Int32
	a.verify = func(tok, hash string) bool { calls.Add(1); return checkAdminToken(tok, hash) }
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := a.check("Right123", "10.0.0.9", time.Now()); !ok {
				t.Error("口令对应通过")
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("并发的同一个口令应只算一次哈希，算了 %d 次", n)
	}
}

// 管理接口：没口令 401；猜错多了 429 + Retry-After；后台页面带安全响应头。
func TestAdminAuthHTTP(t *testing.T) {
	_, h := newAdminTestServer(t)
	get := func(tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/v1/admin/devices", nil)
		if tok != "" {
			r.Header.Set("X-Admin-Token", tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("没口令应 401：%d", w.Code)
	}
	if w := get(adminToken); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("口令对应 200 且不缓存：%d", w.Code)
	}
	for i := range 6 {
		w := get("wrong-" + string(rune('a'+i)))
		if i < 5 && (w.Code != http.StatusUnauthorized || w.Header().Get("X-Auth-Failures") != strconv.Itoa(i+1)) {
			t.Fatalf("第 %d 个错口令应 401 并带上失败次数：%d %q", i+1, w.Code, w.Header().Get("X-Auth-Failures"))
		}
	}
	if w := get(adminToken); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "分钟后再试") {
		t.Fatalf("猜错 6 次后应锁定：%d %s", w.Code, w.Body.String())
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/admin", nil))
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("后台页面应带 CSP：%q", csp)
	}
}

// 后台改口令：只存哈希；重启后仍是改过的口令；把 server.json 的 admin_token 改成新值再重启即可找回。
func TestAdminTokenChangeAndRecovery(t *testing.T) {
	cfg := &Config{Listen: ":9001", MediaRoot: t.TempDir(), DataDir: t.TempDir(), AdminToken: adminToken, EnrollToken: enrollToken}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	start := func() http.Handler {
		s, err := newServer(cfg, tools{encErr: "n/a", pdfErr: "n/a"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s.Handler()
	}
	code := func(h http.Handler, tok string) int {
		r := httptest.NewRequest("GET", "/api/v1/admin/token", nil)
		r.Header.Set("X-Admin-Token", tok)
		r.RemoteAddr = "10.1.1.1:1" + tok[:1] // 不同口令用不同来源，免得测试自己触发锁定
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	h := start()
	if w := do2(t, h, adminReq("PUT", "/api/v1/admin/token", map[string]string{"new_token": "短"})); w.Code != http.StatusBadRequest {
		t.Fatalf("太短的新口令应被拒：%d", w.Code)
	}
	do(t, h, adminReq("PUT", "/api/v1/admin/token", map[string]string{"new_token": "N3w-token!"}), http.StatusNoContent)
	if code(h, adminToken) != http.StatusUnauthorized || code(h, "N3w-token!") != http.StatusOK {
		t.Fatal("改口令后旧口令应失效、新口令生效")
	}
	state, _ := os.ReadFile(filepath.Join(cfg.DataDir, "state.json"))
	if strings.Contains(string(state), "N3w-token!") || strings.Contains(string(state), adminToken) || !strings.Contains(string(state), "pbkdf2-sha256$") {
		t.Fatal("state.json 里只能有口令哈希，不能有明文")
	}

	h = start() // 重启：server.json 没改，沿用后台改过的口令
	if code(h, "N3w-token!") != http.StatusOK || code(h, adminToken) != http.StatusUnauthorized {
		t.Fatal("重启后应沿用后台改过的口令")
	}

	cfg.AdminToken = "Recover99" // 忘了口令：在服务器上把 admin_token 改成新值并重启
	h = start()
	if code(h, "Recover99") != http.StatusOK || code(h, "N3w-token!") != http.StatusUnauthorized {
		t.Fatal("server.json 改成新值后应生效，后台改过的口令作废")
	}
}
