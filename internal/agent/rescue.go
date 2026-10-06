package agent

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 现场救援：设备连不上服务端（后台看不到它的 IP）而屏幕被播放画面占着时，插上 USB 键盘按任意键——
// 播放暂停、显示屏交还给 tty1 控制台，登录提示上方列出设备编号、IP、MAC、服务端地址与连接状态，
// 可直接登录 root。键盘 rescueIdle 内没有任何输入就自动恢复播放（并注销控制台上留下的会话）。
// 设备在线时后台就有它的 IP，直接 SSH 即可，这里只解决"连不上、又不知道 IP"的情形。

// rescueIdle 是救援控制台在键盘无操作后自动恢复播放的时间。
const rescueIdle = time.Minute

// console 显示/收起救援信息（Linux 实现见 rescue_linux.go）。
type console interface {
	show(info string) // 把控制台切到前台并显示 info 与登录提示
	hide()            // 恢复播放前：清掉信息、注销控制台上的会话
	close()           // 代理退出时：只清掉信息（不动可能正在使用的会话）
}

// rescueLoop 每收到一次按键就进入（或延长）救援模式；空闲 idle 后恢复播放。
func (a *Agent) rescueLoop(ctx context.Context, keys <-chan struct{}, con console, idle time.Duration) {
	timer := time.NewTimer(idle)
	timer.Stop()
	active := false // Go 1.23 起 Stop/Reset 不再需要手工排空通道
	for {
		select {
		case <-ctx.Done():
			if active {
				con.close()
			}
			return
		case <-keys:
			if !active {
				active = true
				log.Printf("agent: keyboard input: pausing playback and showing the rescue console on tty1 (resumes after %s without input)", idle)
				a.player.SetPaused(true)
				con.show(a.rescueInfo(time.Now(), localInterfaces(), idle))
			}
			timer.Reset(idle)
		case <-timer.C:
			active = false
			log.Printf("agent: no keyboard input for %s: closing the rescue console and resuming playback", idle)
			con.hide()
			a.player.SetPaused(false)
		}
	}
}

// netIface 是救援信息里列出的一个网口。
type netIface struct {
	Name  string
	MAC   string
	Up    bool
	Addrs []string // IPv4/掩码位数
}

// localInterfaces 列出除回环以外的网口（含没拿到地址的，便于看出网线没插、DHCP 没成功）。
func localInterfaces() []netIface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netIface
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		n := netIface{Name: ifc.Name, MAC: ifc.HardwareAddr.String(), Up: ifc.Flags&net.FlagUp != 0 && ifc.Flags&net.FlagRunning != 0}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() != nil {
				ones, _ := ipn.Mask.Size()
				n.Addrs = append(n.Addrs, fmt.Sprintf("%s/%d", ipn.IP, ones))
			}
		}
		out = append(out, n)
	}
	// 有地址的排前面：现场要找的就是 IP
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Addrs) > 0 && len(out[j].Addrs) == 0 })
	return out
}

// rescueInfo 生成救援控制台上显示的设备信息（英文：控制台字体没有中文）。
func (a *Agent) rescueInfo(now time.Time, ifaces []netIface, idle time.Duration) string {
	var b strings.Builder
	row := func(k, v string) { fmt.Fprintf(&b, "  %-12s %s\n", k, v) }
	b.WriteString("\n===== display-agent rescue console =====\n\n")
	row("Device ID", a.identity.DeviceID)
	row("Hostname", a.hw.Hostname)
	if len(ifaces) == 0 {
		row("Network", "no network interfaces")
	}
	for i, n := range ifaces {
		k := ""
		if i == 0 {
			k = "Network"
		}
		state := "down"
		if n.Up {
			state = "up"
		}
		addrs := "no IPv4 address"
		if len(n.Addrs) > 0 {
			addrs = strings.Join(n.Addrs, ", ")
		}
		mac := n.MAC
		if mac == "" {
			mac = "-"
		}
		row(k, fmt.Sprintf("%-8s %-4s %s  (MAC %s)", n.Name, state, addrs, mac))
	}
	row("Server", a.cfg.ServerURL)
	okAt, errMsg, errAt := a.link.snapshot()
	if okAt.IsZero() {
		row("Connection", "has not reached the server since the agent started")
	} else {
		row("Connection", fmt.Sprintf("last contact %s (%s ago)", okAt.Format("2006-01-02 15:04:05"), now.Sub(okAt).Round(time.Second)))
	}
	if errMsg != "" && errAt.After(okAt) {
		if len(errMsg) > 300 {
			errMsg = errMsg[:300] + "..."
		}
		row("Last error", fmt.Sprintf("%s (%s)", errMsg, errAt.Format("15:04:05")))
	}
	row("Agent", Version)
	fmt.Fprintf(&b, "\n  Playback is paused. It resumes after %.0f minutes without keyboard input\n", idle.Minutes())
	b.WriteString("  (any session left open here is logged out then).\n")
	b.WriteString("  To keep playback stopped: systemctl stop display-agent\n")
	b.WriteString("  Diagnostics: journalctl -u display-agent -n 50 ; check-display.sh (in the install directory)\n\n")
	return b.String()
}

// linkStatus 记录与服务端的最近一次成功与失败，供救援信息显示。主循环写、救援协程读。
type linkStatus struct {
	mu     sync.Mutex
	okAt   time.Time
	errMsg string
	errAt  time.Time
}

func (l *linkStatus) record(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		l.okAt = time.Now()
		return
	}
	l.errMsg, l.errAt = err.Error(), time.Now()
}

func (l *linkStatus) snapshot() (okAt time.Time, errMsg string, errAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.okAt, l.errMsg, l.errAt
}

// Linux 输入事件（struct input_event）：timeval（两个 long）+ type u16 + code u16 + value s32。
// long 随平台：ARMv7 上 16 字节，64 位平台 24 字节。
const (
	evKey      = 0x01 // EV_KEY
	keyPressed = 1    // value：1 按下，0 松开，2 自动重复
)

var inputEventSize = 2*(strconv.IntSize/8) + 8

// countKeyPresses 数出一段原始输入事件里的按键按下次数。
func countKeyPresses(buf []byte, size int) int {
	n := 0
	off := size - 8
	for i := 0; i+size <= len(buf); i += size {
		ev := buf[i+off:]
		if binary.LittleEndian.Uint16(ev[0:2]) == evKey && int32(binary.LittleEndian.Uint32(ev[4:8])) == keyPressed {
			n++
		}
	}
	return n
}

// isKeyboard 根据输入设备的 EV_KEY 能力位图判断是不是键盘：要有字母键、回车和空格。
// 这样红外遥控、HDMI-CEC、板载按键之类不会误触发救援。
func isKeyboard(keyBits []byte) bool {
	has := func(code int) bool { return code/8 < len(keyBits) && keyBits[code/8]&(1<<(code%8)) != 0 }
	const keyEnter, keyA, keyZ, keySpace = 28, 30, 44, 57
	return has(keyA) && has(keyZ) && has(keyEnter) && has(keySpace)
}

// escapeIssue 转义 agetty issue 文件里的反斜杠（agetty 把 \x 当作转义序列）。
func escapeIssue(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
