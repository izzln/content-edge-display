package agent

import (
	"net"
	"os"
	"strings"
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
