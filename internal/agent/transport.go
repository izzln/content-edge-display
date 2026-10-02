package agent

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 设备与服务端都在局域网里，系统必须在**没有外网**时照常工作。传输层为此做了三件事：
//
//  1. 不走代理：环境里若带着指向外网的 http_proxy，局域网请求会被送去一个连不上的代理。
//  2. DNS 兜底：server_url 推荐写域名（换服务器只改解析），但局域网 DNS 常常是路由器转发到
//     运营商——外网一断，域名就解析不了。成功解析过的地址记在 cache_dir/server-addr，
//     解析失败时用它。
//  3. 控制请求（清单/心跳/注册）用短超时：服务端卡住时，主循环不能被一个请求拖过 systemd
//     看门狗的 90 秒，否则代理连同 mpv 一起被杀、屏幕黑一下。大文件下载另有"停滞检测"，见 download.go。
const (
	apiTimeout     = 30 * time.Second
	dnsTimeout     = 5 * time.Second
	connectTimeout = 10 * time.Second
)

// newTransport 构造设备端共用的 HTTP 传输层。
func newTransport(cacheDir string, clock *serverClock, sched *schedule) http.RoundTripper {
	r := &fallbackResolver{path: filepath.Join(cacheDir, "server-addr")}
	t := &http.Transport{
		Proxy:                 nil, // 永远直连
		DialContext:           r.dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: apiTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	return clockTransport{base: t, clock: clock, sched: sched}
}

// fallbackResolver 解析服务端地址，失败时退回上次成功解析的地址。
type fallbackResolver struct {
	path string

	mu     sync.Mutex
	last   map[string]string // host → 上次成功解析的 IP（内存副本）
	warned map[string]bool   // 只在开始用兜底地址时提示一次
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
	if lerr == nil && len(ips) > 0 {
		r.remember(host, ips[0])
		var conn net.Conn
		for _, ip := range ips {
			if conn, err = d.DialContext(ctx, network, net.JoinHostPort(ip, port)); err == nil {
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
	if !r.warned[host] {
		if r.warned == nil {
			r.warned = map[string]bool{}
		}
		r.warned[host] = true
		log.Printf("agent: 解析 %s 失败（%v），改用上次的地址 %s（外网中断时局域网 DNS 常会失效）", host, lerr, ip)
	}
	r.mu.Unlock()
	return d.DialContext(ctx, network, net.JoinHostPort(ip, port))
}

// remember 记下成功解析的地址；变化时落盘，供重启后断网时使用。
func (r *fallbackResolver) remember(host, ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]string{}
	}
	delete(r.warned, host) // DNS 恢复了，下次失效时再提示
	if r.last[host] == ip {
		return
	}
	r.last[host] = ip
	_ = writeFileSync(r.path, []byte(host+" "+ip+"\n"), 0o644)
}

// recall 返回上次成功解析的地址（内存优先，其次磁盘）。
func (r *fallbackResolver) recall(host string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ip := r.last[host]; ip != "" {
		return ip
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return ""
	}
	f := strings.Fields(string(data))
	if len(f) == 2 && f[0] == host && net.ParseIP(f[1]) != nil {
		return f[1]
	}
	return ""
}
