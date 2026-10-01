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
			a, p, _, _, devDir := newTestEnv(t) // newTestEnv 已经走过一次注册
			_ = devDir
			a.clock.local = func() time.Time { return time.Now().Add(skew) }

			// 问题复现：直接用本机时钟签名，服务端拒绝并说明是时钟偏差
			req, _ := http.NewRequest("GET", a.cfg.ServerURL+"/api/v1/device/manifest", nil)
			ts := strconv.FormatInt(time.Now().Add(skew).Unix(), 10)
			req.Header.Set(sign.HeaderDeviceID, a.DeviceID())
			req.Header.Set(sign.HeaderTimestamp, ts)
			req.Header.Set(sign.HeaderSign, sign.Sign(a.identity.Secret, ts, "GET", "/api/v1/device/manifest"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "clock skew") {
				t.Fatalf("用偏差时钟签名应被拒并说明原因：%d %s", resp.StatusCode, body)
			}

			// 修复：代理从服务端响应的 Date 头学到偏差（注册响应就足够），按服务端时间签名
			if err := a.Register(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := a.PollOnce(context.Background()); err != nil {
				t.Fatalf("时钟偏差 %s 时轮询应成功：%v", skew, err)
			}
			if err := a.Heartbeat(context.Background()); err != nil {
				t.Fatalf("时钟偏差 %s 时心跳应成功：%v", skew, err)
			}
			if len(p.Scene().Items) == 0 {
				t.Fatal("应已下载并加载内容")
			}
		})
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
