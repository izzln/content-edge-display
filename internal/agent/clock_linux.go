//go:build linux

package agent

import (
	"syscall"
	"time"
)

// ntpSynced 判断系统时钟是否已被 NTP 守护进程（systemd-timesyncd、chrony 等）同步：
// 它们同步后会清掉内核的 STA_UNSYNC 标志（timedatectl 的 NTPSynchronized 也是看这个）。
func ntpSynced() bool {
	const staUnsync = 0x0040
	var tx syscall.Timex
	if _, err := syscall.Adjtimex(&tx); err != nil {
		return false
	}
	return tx.Status&staUnsync == 0
}

// setSystemClock 把系统时间设为 t（需要 root；代理以 root 运行）。
func setSystemClock(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}
