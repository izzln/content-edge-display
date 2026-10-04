package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/testutil"
)

// inputEvent 按给定的 struct input_event 大小（16 或 24 字节）编码一个事件。
func inputEvent(size int, typ, code uint16, value int32) []byte {
	b := make([]byte, size)
	off := size - 8
	binary.LittleEndian.PutUint16(b[off:], typ)
	binary.LittleEndian.PutUint16(b[off+2:], code)
	binary.LittleEndian.PutUint32(b[off+4:], uint32(value))
	return b
}

// 两种结构大小都要解析对：只数按下（value=1），松开、自动重复、同步事件都不算。
func TestCountKeyPresses(t *testing.T) {
	for _, size := range []int{16, 24} {
		var buf []byte
		buf = append(buf, inputEvent(size, 0x04, 0x04, 30)...) // EV_MSC 扫描码
		buf = append(buf, inputEvent(size, evKey, 30, 1)...)   // A 按下
		buf = append(buf, inputEvent(size, 0x00, 0, 0)...)     // EV_SYN
		buf = append(buf, inputEvent(size, evKey, 30, 2)...)   // 自动重复
		buf = append(buf, inputEvent(size, evKey, 30, 0)...)   // 松开
		buf = append(buf, inputEvent(size, evKey, 28, 1)...)   // 回车按下
		buf = append(buf, 0x01, 0x02)                          // 半截事件忽略
		if n := countKeyPresses(buf, size); n != 2 {
			t.Fatalf("size %d: 应数出 2 次按下，得到 %d", size, n)
		}
	}
}

func TestIsKeyboard(t *testing.T) {
	bits := func(codes ...int) []byte {
		b := make([]byte, 96)
		for _, c := range codes {
			b[c/8] |= 1 << (c % 8)
		}
		return b
	}
	if !isKeyboard(bits(1, 28, 30, 44, 57, 100)) {
		t.Fatal("有字母、回车、空格的应判为键盘")
	}
	for name, b := range map[string][]byte{
		"电源键":  bits(116),
		"红外遥控": bits(28, 103, 108, 105, 106, 352),
		"空":    nil,
	} {
		if isKeyboard(b) {
			t.Fatalf("%s 不应判为键盘", name)
		}
	}
}

type fakeConsole struct {
	mu                   sync.Mutex
	shows, hides, closes int
	info                 string
}

func (c *fakeConsole) show(info string) { c.mu.Lock(); c.shows++; c.info = info; c.mu.Unlock() }
func (c *fakeConsole) hide()            { c.mu.Lock(); c.hides++; c.mu.Unlock() }
func (c *fakeConsole) close()           { c.mu.Lock(); c.closes++; c.mu.Unlock() }
func (c *fakeConsole) counts() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shows, c.hides, c.closes
}

// 按键即暂停播放并显示信息；持续按键只延长、不重复显示；空闲到时恢复播放；再按又进入。
func TestRescueLoop(t *testing.T) {
	p := player.NewNull()
	a := New(&Config{ServerURL: "http://srv:9001", CacheDir: t.TempDir()}, p)
	a.identity.DeviceID = "scr-0017"
	con := &fakeConsole{}
	keys := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	const idle = 150 * time.Millisecond
	go func() { a.rescueLoop(ctx, keys, con, idle); close(done) }()

	keys <- struct{}{}
	testutil.WaitFor(t, time.Second, "rescue", func() bool { s, _, _ := con.counts(); return s == 1 && p.Paused() })
	for i := 0; i < 3; i++ { // 一直有按键：不恢复
		time.Sleep(idle / 2)
		keys <- struct{}{}
	}
	if s, h, _ := con.counts(); s != 1 || h != 0 || !p.Paused() {
		t.Fatalf("持续按键时应保持救援模式：shows=%d hides=%d", s, h)
	}
	testutil.WaitFor(t, time.Second, "rescue", func() bool { _, h, _ := con.counts(); return h == 1 })
	if p.Paused() {
		t.Fatal("空闲到时应恢复播放")
	}
	if !strings.Contains(con.info, "scr-0017") {
		t.Fatalf("信息里应有设备编号：%q", con.info)
	}

	keys <- struct{}{}
	testutil.WaitFor(t, time.Second, "rescue", func() bool { s, _, _ := con.counts(); return s == 2 && p.Paused() })
	cancel() // 代理退出（如在控制台里 systemctl stop）：只清信息，不注销会话
	<-done
	if _, h, c := con.counts(); h != 1 || c != 1 {
		t.Fatalf("退出时应 close 而不是 hide：hides=%d close=%d", h, c)
	}
}

func TestRescueInfo(t *testing.T) {
	a := New(&Config{ServerURL: "https://display.lan:9001", CacheDir: t.TempDir()}, player.NewNull())
	a.identity.DeviceID, a.hw.Hostname = "opi-1a2b3c4d", "orangepione"
	ifaces := []netIface{
		{Name: "eth0", MAC: "02:81:aa:bb:cc:dd", Up: true, Addrs: []string{"192.168.1.23/24"}},
		{Name: "wlan0", MAC: "12:34:56:78:9a:bc"},
	}
	now := time.Now()

	info := a.rescueInfo(now, ifaces, 15*time.Minute)
	for _, want := range []string{"opi-1a2b3c4d", "orangepione", "192.168.1.23/24", "02:81:aa:bb:cc:dd",
		"wlan0", "no IPv4 address", "https://display.lan:9001", "has not reached the server", "15 minutes", "systemctl stop display-agent"} {
		if !strings.Contains(info, want) {
			t.Fatalf("信息里缺 %q：\n%s", want, info)
		}
	}

	a.link.record(nil)
	a.link.record(errors.New("dial tcp 192.168.1.10:9001: connect: no route to host"))
	info = a.rescueInfo(now.Add(time.Minute), ifaces, 15*time.Minute)
	if !strings.Contains(info, "last contact") || !strings.Contains(info, "no route to host") {
		t.Fatalf("应显示最近联系时间与之后的错误：\n%s", info)
	}
	a.link.record(nil) // 恢复后旧错误不再显示
	if info = a.rescueInfo(time.Now(), ifaces, time.Minute); strings.Contains(info, "Last error") {
		t.Fatalf("成功之后不应再显示旧错误：\n%s", info)
	}
}

func TestEscapeIssue(t *testing.T) {
	if got := escapeIssue(`C:\n \4`); got != `C:\\n \\4` {
		t.Fatalf("反斜杠要转义：%q", got)
	}
}
