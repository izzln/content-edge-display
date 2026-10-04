package agent

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/sign"
)

// 设备没有 RTC、连不上 NTP，系统时间偏了几个小时：注册（不签名）能成功，
// 但之前每个签名请求都是 401。按服务端时间签名后，轮询、心跳、下载都必须正常。
func TestSkewedDeviceClockStillAuthenticates(t *testing.T) {
	for _, skew := range []time.Duration{2 * time.Hour, -26 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			e := newEnv(t, true)
			a, p := e.a, e.p
			a.clock.local = func() time.Time { return time.Now().Add(skew) }

			// 问题复现：直接用本机时钟签名，服务端拒绝并说明是时钟偏差
			req, _ := http.NewRequest("GET", a.cfg.ServerURL+"/api/v1/device/manifest", nil)
			ts := strconv.FormatInt(time.Now().Add(skew).Unix(), 10)
			req.Header.Set(sign.HeaderDeviceID, a.identity.DeviceID)
			req.Header.Set(sign.HeaderTimestamp, ts)
			req.Header.Set(sign.HeaderSign, sign.Sign(a.identity.Secret, ts, "GET", "/api/v1/device/manifest"))
			raw := &http.Client{Transport: &http.Transport{TLSClientConfig: pinnedTLS(a.cfg.TLSFingerprint)}}
			resp, err := raw.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "clock skew") {
				t.Fatalf("用偏差时钟签名应被拒并说明原因：%d %s", resp.StatusCode, body)
			}

			// 修复：代理从服务端响应的 Date 头学到偏差（注册响应就足够），按服务端时间签名
			if err := a.register(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := a.poll(context.Background()); err != nil {
				t.Fatalf("时钟偏差 %s 时轮询应成功：%v", skew, err)
			}
			if err := a.heartbeat(context.Background()); err != nil {
				t.Fatalf("时钟偏差 %s 时心跳应成功：%v", skew, err)
			}
			if len(p.Scene().Items) == 0 {
				t.Fatal("应已下载并加载内容")
			}
		})
	}
}

// 已注册的设备断电重启、时间回退了一天：第一次轮询因时钟偏差被拒，但同一个响应已让代理学到偏差，
// 立即重试就成功，不用等下一个轮询周期。
func TestClockSkewRetriedImmediately(t *testing.T) {
	e := newEnv(t, true)
	a, p := e.a, e.p
	a.clock.local = func() time.Time { return time.Now().Add(-24 * time.Hour) }
	calls := 0
	err := retrySkew(func() error { calls++; return a.poll(context.Background()) })
	if err != nil || calls != 2 {
		t.Fatalf("应在第二次（立即重试）成功：调用 %d 次，错误 %v", calls, err)
	}
	if len(p.Scene().Items) == 0 {
		t.Fatal("应已加载内容")
	}
}

func TestServerClockOffset(t *testing.T) {
	c := newServerClock()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c.local = func() time.Time { return base }
	c.observe(http.Header{"Date": []string{base.Add(3 * time.Hour).Format(http.TimeFormat)}})
	if got := c.Now(); !got.Equal(base.Add(3 * time.Hour)) {
		t.Fatalf("应按服务端时间：%v", got)
	}
	c.observe(http.Header{"Date": []string{"garbage"}})
	if got := c.Now(); !got.Equal(base.Add(3 * time.Hour)) {
		t.Fatalf("解析不了的 Date 头应忽略：%v", got)
	}
}

// 没有 NTP 同步、偏差明显时，把系统时钟校到服务端时间；已由 NTP 同步、或偏差很小时不动。
func TestSystemClockSetFromServer(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	server := base.Add(72 * time.Hour) // 设备断电重启后时间停在三天前
	cases := []struct {
		name   string
		synced bool
		server time.Time
		set    bool
	}{
		{"unsynced, large skew", false, server, true},
		{"NTP-synced", true, server, false},
		{"small skew", false, base.Add(20 * time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := base
			var setTo time.Time
			c := newServerClock()
			c.local = func() time.Time { return now }
			c.synced = func() bool { return tc.synced }
			c.setSystem = func(t time.Time) error { setTo, now = t, t; return nil }
			c.observe(http.Header{"Date": []string{tc.server.Format(http.TimeFormat)}})
			if got := !setTo.IsZero(); got != tc.set {
				t.Fatalf("set system clock = %v, want %v", got, tc.set)
			}
			if !c.Now().Equal(tc.server) {
				t.Fatalf("签名用的时间应等于服务端时间：%v", c.Now())
			}
		})
	}
}
