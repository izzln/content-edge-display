package agent

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// sdNotify 向 systemd 发送状态通知（READY=1 / WATCHDOG=1）。
// 未运行在 systemd Type=notify 下（无 NOTIFY_SOCKET）时为空操作。
// 纯标准库实现，无 cgo，便于交叉编译。
func sdNotify(state string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if strings.HasPrefix(addr, "@") { // abstract socket
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}

// watchdogInterval 是喂 systemd 看门狗的间隔，须明显小于单元文件里的 WatchdogSec=90。
var watchdogInterval = 30 * time.Second

// sleepFeeding 等待 d（或 ctx 取消），期间按 watchdogInterval 喂狗；ctx 取消时返回 false。
//
// 服务端连不上时（断网、服务器关机）重试间隔会退避到几分钟。直接 time.After 干等的话，
// 超过 90 秒 systemd 就判定代理假死，连同 mpv 一起杀掉重启——屏幕每隔一分半黑一下。
func sleepFeeding(ctx context.Context, d time.Duration) bool {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(watchdogInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return true
		case <-tick.C:
			sdNotify("WATCHDOG=1")
		}
	}
}
