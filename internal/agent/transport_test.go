package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 外网一断，局域网 DNS（常是路由器转发到运营商）就解析不了 server_url 里的域名。
// 成功解析过一次后，解析失败时要退回上次的地址，设备照常连上服务端。
func TestResolverFallsBackToLastKnownAddress(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer ts.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	dir := t.TempDir()

	// 第一次：localhost 能解析，记下地址
	r := &fallbackResolver{path: filepath.Join(dir, "server-addr")}
	conn, err := r.dial(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "server-addr"))
	if !strings.HasPrefix(string(data), "localhost ") {
		t.Fatalf("应把解析结果落盘：%q", data)
	}

	// 模拟断网后重启：内存里没有，磁盘里有；用一个解析不了的域名替代 localhost
	os.WriteFile(filepath.Join(dir, "server-addr"), []byte("display.invalid 127.0.0.1\n"), 0o644)
	r2 := &fallbackResolver{path: filepath.Join(dir, "server-addr")}
	conn, err = r2.dial(context.Background(), "tcp", net.JoinHostPort("display.invalid", port))
	if err != nil {
		t.Fatalf("DNS 失败时应退回上次的地址：%v", err)
	}
	conn.Close()

	// 从没解析成功过的域名：照实报错
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
	c := &http.Client{Transport: newTransport(t.TempDir(), newServerClock(), nil)}
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
	buf := make([]byte, 3)
	if _, err := io.ReadFull(r, buf); err != nil {
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

// listenNotify 起一个假的 systemd NOTIFY_SOCKET，统计收到的 WATCHDOG=1。
func listenNotify(t *testing.T) func() int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "n.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	var mu sync.Mutex
	n := 0
	go func() {
		buf := make([]byte, 64)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			if string(buf[:k]) == "WATCHDOG=1" {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}
	}()
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// 回归：服务端连不上（断网、服务器关机）时注册重试会退避到几分钟，等待期间必须持续喂狗，
// 否则超过 90 秒 systemd 把代理连同 mpv 一起杀掉，屏幕每隔一分半黑一下。
func TestRegisterRetryKeepsFeedingWatchdog(t *testing.T) {
	count := listenNotify(t)
	old := watchdogInterval
	watchdogInterval = 20 * time.Millisecond
	defer func() { watchdogInterval = old }()

	// 一个肯定连不上的服务端地址
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cfg := &Config{ServerURL: "http://" + addr, EnrollToken: "t", CacheDir: t.TempDir(), Player: "null"}
	cfg.fillDefaults()
	a := New(cfg, nil)
	if err := a.ResolveIdentity(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	a.registerLoop(ctx) // 第一次失败后要等 5 秒才重试；这期间看门狗不能断
	if n := count(); n < 10 {
		t.Fatalf("重试等待期间应持续喂狗，0.5 秒内只收到 %d 次", n)
	}
}

// 轮询/心跳间隔跟着服务端走；离谱的值收进合理范围，没带头（旧版服务端）就保持原样。
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
	c := &http.Client{Transport: newTransport(t.TempDir(), newServerClock(), sched)}
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
