package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/sign"
)

// 设备与服务端都在局域网里，系统必须在**没有外网**时照常工作。传输层为此做了三件事：
//
//  1. 不走代理：环境里若带着指向外网的 http_proxy，局域网请求会被送去一个连不上的代理。
//  2. DNS 兜底：server_url 推荐写域名（换服务器只改解析），但局域网 DNS 常常是路由器转发到
//     运营商——外网一断，域名就解析不了。成功解析过的地址记在 cache_dir/server-addr，
//     解析失败时用它。
//  3. 控制请求（清单/心跳/注册）用短超时：服务端卡住时，主循环不能被一个请求拖过 systemd
//     看门狗的 90 秒，否则代理连同播放进程一起被杀、屏幕黑一下。大文件下载另有"停滞检测"，见 download.go。
const (
	apiTimeout     = 30 * time.Second
	dnsTimeout     = 5 * time.Second
	connectTimeout = 10 * time.Second
)

// newTransport 构造设备端共用的 HTTP 传输层：只接受公钥指纹等于 pin 的服务端证书。
func newTransport(cacheDir, pin string, clock *serverClock, sched *schedule) clockTransport {
	r := &fallbackResolver{path: filepath.Join(cacheDir, "server-addr")}
	t := &http.Transport{
		TLSClientConfig:       pinnedTLS(pin),
		Proxy:                 nil, // 永远直连
		DialContext:           r.dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: apiTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	return clockTransport{base: t, clock: clock, sched: sched}
}

// clockTransport 在每个 HTTP 响应上更新 serverClock 与服务端规定的轮询/心跳间隔（schedule.go）。
type clockTransport struct {
	base  *http.Transport
	clock *serverClock
	sched *schedule
}

func (t clockTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil {
		t.clock.observe(resp.Header)
		t.sched.observe(resp.Header)
	}
	return resp, err
}

// CloseIdleConnections 让 http.Client.CloseIdleConnections 生效（Client 只认实现了它的 Transport）。
func (t clockTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// pinnedTLS 固定服务端证书：不走 CA 链，也不看有效期与主机名（设备时钟可能不准、服务器 IP 会变），
// 只要求对端证书的公钥指纹等于装机时记下的那个。服务端证书是自签的，见 server/tls.go。
func pinnedTLS(pin string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // 由下面的 VerifyConnection 按指纹校验
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			if got := sign.CertFingerprint(cs.PeerCertificates[0]); got != pin {
				return fmt.Errorf("server certificate fingerprint %s does not match tls_fingerprint %s "+
					"(someone may be impersonating the server, or the server was reinstalled without its data_dir/tls)", got, pin)
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}
}

// fallbackResolver 解析服务端地址，失败时退回上次成功连上的地址（内存里一份，cache_dir 里落盘一份）。
type fallbackResolver struct {
	path string

	mu     sync.Mutex
	host   string // 上次成功连上的主机名与 IP
	ip     string
	warned bool // 只在开始用兜底地址时提示一次
}

func (r *fallbackResolver) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return d.DialContext(ctx, network, addr) // 本来就是 IP
	}
	lctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	ips, lerr := net.DefaultResolver.LookupHost(lctx, host)
	cancel()
	if lerr == nil {
		for _, ip := range ips {
			var conn net.Conn
			if conn, err = d.DialContext(ctx, network, net.JoinHostPort(ip, port)); err == nil {
				r.remember(host, ip)
				return conn, nil
			}
		}
		return nil, err
	}
	ip := r.recall(host)
	if ip == "" {
		return nil, lerr
	}
	r.mu.Lock()
	if !r.warned {
		r.warned = true
		log.Printf("agent: resolving %s failed (%v), using last known address %s (LAN DNS often fails when the internet is down)", host, lerr, ip)
	}
	r.mu.Unlock()
	return d.DialContext(ctx, network, net.JoinHostPort(ip, port))
}

// remember 记下成功连上的地址；变化时落盘，供重启后断网时使用。
func (r *fallbackResolver) remember(host, ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warned = false // DNS 恢复了，下次失效时再提示
	if r.host == host && r.ip == ip {
		return
	}
	r.host, r.ip = host, ip
	_ = fsutil.WriteFile(r.path, []byte(host+" "+ip+"\n"), 0o644)
}

// recall 返回上次成功连上 host 的地址（内存优先，其次磁盘）。
func (r *fallbackResolver) recall(host string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.host == host {
		return r.ip
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return ""
	}
	if f := strings.Fields(string(data)); len(f) == 2 && f[0] == host && net.ParseIP(f[1]) != nil {
		return f[1]
	}
	return ""
}
