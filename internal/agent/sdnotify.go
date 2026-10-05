package agent

import (
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

// keepFeeding 在 fn 执行期间持续喂狗。只用于有明确上限、但可能超过看门狗时限的操作（如执行 update.sh）；
// 主循环其余时候由 Run 里的定时器喂狗，真卡死时看门狗照样生效。
func keepFeeding(fn func()) {
	done := make(chan struct{})
	t := time.NewTicker(watchdogInterval) // 在这里读间隔：协程可能在 fn 返回后才开始跑
	go func() {
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sdNotify("WATCHDOG=1")
			}
		}
	}()
	fn()
	close(done)
}
