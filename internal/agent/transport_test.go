package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/sign"
)

// 外网一断，局域网 DNS（常是路由器转发到运营商）就解析不了 server_url 里的域名。
// 成功连上过一次后，解析失败时要退回上次的地址，设备照常连上服务端。
func TestResolverFallsBackToLastKnownAddress(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer ts.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	path := filepath.Join(t.TempDir(), "server-addr")

	// localhost 可能先解析出 ::1（服务只听 127.0.0.1，连不上）：记下的必须是真正连上的那个地址
	r := &fallbackResolver{path: path}
	conn, err := r.dial(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if data, _ := os.ReadFile(path); string(data) != "localhost 127.0.0.1\n" {
		t.Fatalf("应记下实际连上的地址：%q", data)
	}

	// 模拟断网后重启：内存里没有，磁盘里有；用一个解析不了的域名替代 localhost
	os.WriteFile(path, []byte("display.invalid 127.0.0.1\n"), 0o644)
	r2 := &fallbackResolver{path: path}
	conn, err = r2.dial(context.Background(), "tcp", net.JoinHostPort("display.invalid", port))
	if err != nil {
		t.Fatalf("DNS 失败时应退回上次的地址：%v", err)
	}
	conn.Close()

	// 从没连上过的域名：照实报错
	if _, err := r2.dial(context.Background(), "tcp", net.JoinHostPort("other.invalid", port)); err == nil {
		t.Fatal("没有兜底地址时应报错")
	}
}

// 局域网请求永远直连：环境里带着指向外网的代理时，不能把请求送过去。
func TestTransportIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer ts.Close()
	c := &http.Client{Transport: newTransport(t.TempDir(), "", newServerClock(), newSchedule())}
	resp, err := c.Get(strings.Replace(ts.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatalf("应忽略代理直连：%v", err)
	}
	resp.Body.Close()
}

// 下载停滞（服务端或网络卡死）要在看门狗 90 秒之前放弃，而不是把主循环拖到被 systemd 杀掉。
func TestProgressReaderCancelsOnStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	r := &progressReader{r: pr, stall: 50 * time.Millisecond, timer: time.AfterFunc(50*time.Millisecond, cancel)}
	go func() {
		pw.Write([]byte("abc")) // 先来一点数据，然后卡住
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	if _, err := io.ReadFull(r, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("停滞后应取消请求")
	}
	if stallTimeout >= 90*time.Second {
		t.Fatalf("停滞超时 %s 必须小于 systemd 看门狗的 90 秒", stallTimeout)
	}
}

// 服务端连不上（断网、服务器关机）时重试会退避到几分钟，主循环等待期间必须持续喂狗，
// 否则超过 90 秒 systemd 把代理连同播放进程一起杀掉，屏幕每隔一分半黑一下。
func TestRunKeepsFeedingWatchdogWhileServerIsDown(t *testing.T) {
	count := listenNotify(t)
	fastWatchdog(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0") // 一个肯定连不上的服务端地址
	addr := l.Addr().String()
	l.Close()
	cfg := &Config{ServerURL: "https://" + addr, TLSFingerprint: strings.Repeat("0", 64), EnrollToken: "t", CacheDir: t.TempDir(), Player: "null"}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := New(cfg, player.NewNull()).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(); n < 10 {
		t.Fatalf("重试等待期间应持续喂狗，0.5 秒内只收到 %d 次", n)
	}
}

// 连不上服务端、从没注册上时，现场自检（check-display.sh）照样要读到本地播放状态。
func TestStatusWrittenWhileServerIsDown(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cfg := &Config{ServerURL: "https://" + addr, TLSFingerprint: strings.Repeat("0", 64), EnrollToken: "t", CacheDir: t.TempDir(), Player: "null"}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatal(err)
	}
	a := New(cfg, player.NewNull())
	a.sched.heartbeat.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, "status.json")); err != nil {
		t.Fatalf("没连上服务端也应写播放状态：%v", err)
	}
}

// 失败后指数退避，但不超过上限；密钥冲突时最多一分钟重试一次（等运营方在后台点接受）。
func TestRetryDelay(t *testing.T) {
	a := &Agent{sched: newSchedule()}
	if d := a.retryDelay(nil); d != 10*time.Second {
		t.Fatalf("正常按轮询间隔：%s", d)
	}
	a.failures = 3
	if d := a.retryDelay(errUnknownDevice); d != 80*time.Second {
		t.Fatalf("失败 3 次应退避到 80s：%s", d)
	}
	a.failures = 10
	if d := a.retryDelay(errUnknownDevice); d != maxBackoff {
		t.Fatalf("不应超过上限：%s", d)
	}
	if d := a.retryDelay(errKeyConflict); d != time.Minute {
		t.Fatalf("密钥冲突最多一分钟：%s", d)
	}
}

// 轮询/心跳间隔跟着服务端走；离谱的值收进合理范围，响应里没带头就保持原样。
func TestScheduleFollowsServer(t *testing.T) {
	var poll, hb string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if poll != "" {
			w.Header().Set("X-Poll-Interval", poll)
			w.Header().Set("X-Heartbeat-Interval", hb)
		}
	}))
	defer srv.Close()
	sched := newSchedule()
	c := &http.Client{Transport: newTransport(t.TempDir(), "", newServerClock(), sched)}
	get := func() {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if sched.Poll() != 10*time.Second || sched.Heartbeat() != 60*time.Second {
		t.Fatalf("联系上服务端之前用默认 10s/60s：%s %s", sched.Poll(), sched.Heartbeat())
	}
	poll, hb = "5", "120"
	get()
	if sched.Poll() != 5*time.Second || sched.Heartbeat() != 120*time.Second {
		t.Fatalf("应照服务端设定：%s %s", sched.Poll(), sched.Heartbeat())
	}
	poll, hb = "0", "999999"
	get()
	if sched.Poll() != time.Second || sched.Heartbeat() != time.Hour {
		t.Fatalf("越界值应收进范围：%s %s", sched.Poll(), sched.Heartbeat())
	}
	poll = ""
	get()
	if sched.Poll() != time.Second {
		t.Fatal("没带头时应保持上次的值")
	}
}

// 证书固定：指纹对就连，不对就拒；证书过期（设备时钟不准时常见）只要指纹对也照常连。
func TestPinnedTLS(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().AddDate(-2, 0, 0), NotAfter: time.Now().AddDate(-1, 0, 0)} // 已过期
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}}
	srv.StartTLS()
	defer srv.Close()

	get := func(pin string) error {
		c := &http.Client{Transport: newTransport(t.TempDir(), pin, newServerClock(), newSchedule())}
		resp, err := c.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(sign.CertFingerprint(leaf)); err != nil {
		t.Fatalf("指纹正确（证书已过期）应能连接：%v", err)
	}
	if err := get(strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "does not match tls_fingerprint") {
		t.Fatalf("指纹不对应拒绝并说明原因：%v", err)
	}
}

// http.Client.CloseIdleConnections 只认实现了它的 Transport：包了一层也必须把它转下去，
// 请求失败后丢掉连接池里的死连接才会生效。
func TestTransportForwardsCloseIdleConnections(t *testing.T) {
	var tr http.RoundTripper = newTransport(t.TempDir(), "", newServerClock(), newSchedule())
	if _, ok := tr.(interface{ CloseIdleConnections() }); !ok {
		t.Fatal("传输层应实现 CloseIdleConnections")
	}
}
