package agent

import (
	"log"
	"net/http"
	"sync"
	"time"
)

// serverClock 让设备按**服务端时间**签名。
//
// 签名里带时间戳，服务端只认与自己相差 ±5 分钟以内的请求（防重放）。可 Orange Pi One
// 没有电池供电的 RTC：断电重启后系统时间要等 NTP 校准，而本地化部署常常连不上外网的
// NTP——设备时钟一偏，注册（不签名）照样成功，此后每个轮询、心跳都是 401。
//
// 服务端每个响应都带 Date 头（包括不需要签名的注册响应），据此记下"服务端时间 − 本机时间"，
// 签名时加上这个偏差。认证从此不依赖设备时钟准不准；服务端照样按自己的时钟校验时间窗。
type serverClock struct {
	mu     sync.Mutex
	offset time.Duration
	skewed bool             // 当前是否处于"明显偏差"状态（只在状态变化时记日志）
	local  func() time.Time // 本机时钟（测试注入）
}

// skewWarn 超过这个偏差就记日志提醒（签名已自动修正，但设备日志时间等仍不准）。
const skewWarn = time.Minute

func newServerClock() *serverClock { return &serverClock{local: time.Now} }

// Now 返回按服务端校准后的当前时间。
func (c *serverClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.local().Add(c.offset)
}

// observe 用响应的 Date 头更新偏差。Date 只精确到秒，±1 秒的误差远小于 ±5 分钟的时间窗。
func (c *serverClock) observe(h http.Header) {
	server, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = server.Sub(c.local()).Round(time.Second)
	skewed := c.offset > skewWarn || c.offset < -skewWarn
	if skewed != c.skewed {
		c.skewed = skewed
		if skewed {
			log.Printf("agent: local clock differs from the server by %s (local %s); signing with server time instead. "+
				"This happens without an RTC and without NTP; playback is unaffected but device log timestamps are off. "+
				"Point the device's NTP at a LAN server (see docs/deployment.md)",
				-c.offset, c.local().Format(time.DateTime))
		} else {
			log.Printf("agent: local clock is in sync with the server again")
		}
	}
}

// clockTransport 在每个 HTTP 响应上更新 serverClock 与服务端规定的轮询/心跳间隔（schedule.go）。
type clockTransport struct {
	base  http.RoundTripper
	clock *serverClock
	sched *schedule // 可为 nil
}

func (t clockTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil {
		t.clock.observe(resp.Header)
		if t.sched != nil {
			t.sched.observe(resp.Header)
		}
	}
	return resp, err
}
