package agent

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	inputGlob     = "/dev/input/event*"
	inputScan     = 3 * time.Second // 键盘热插拔：隔几秒扫一次
	rescueIssue   = "/etc/issue.d/display-agent.issue"
	rescueTTY     = "/dev/tty1"
	rescueGetty   = "getty@tty1"
	vtActivate    = 0x5606 // VT_ACTIVATE
	vtWaitActive  = 0x5607 // VT_WAITACTIVE
	keyBitsLength = 0x300 / 8
)

// evIOCGBitKey 是 ioctl EVIOCGBIT(EV_KEY, keyBitsLength)：读出设备支持的按键位图。
const evIOCGBitKey = 2<<30 | keyBitsLength<<16 | 'E'<<8 | (0x20 + evKey)

// watchKeyboards 监听所有键盘的按键（含之后插上的），每次按下发一个信号。
func watchKeyboards(ctx context.Context) <-chan struct{} {
	keys := make(chan struct{}, 1)
	go func() {
		var mu sync.Mutex
		reading := map[string]*os.File{} // 正在读的键盘
		skip := map[string]uint64{}      // 不是键盘的设备 → inode（设备节点被重建时重新判断）
		defer func() {
			mu.Lock()
			for _, f := range reading {
				f.Close()
			}
			mu.Unlock()
		}()
		t := time.NewTicker(inputScan)
		defer t.Stop()
		for {
			paths, _ := filepath.Glob(inputGlob)
			for _, p := range paths {
				var st syscall.Stat_t
				if syscall.Stat(p, &st) != nil {
					continue
				}
				mu.Lock()
				_, busy := reading[p]
				ino, skipped := skip[p]
				mu.Unlock()
				if busy || (skipped && ino == st.Ino) {
					continue
				}
				f, err := os.Open(p)
				if err != nil {
					continue
				}
				bits := make([]byte, keyBitsLength)
				if !ioctl(f.Fd(), evIOCGBitKey, unsafe.Pointer(&bits[0])) || !isKeyboard(bits) {
					f.Close()
					mu.Lock()
					skip[p] = st.Ino
					mu.Unlock()
					continue
				}
				log.Printf("agent: keyboard detected at %s (press any key for the rescue console)", p)
				mu.Lock()
				delete(skip, p)
				reading[p] = f
				mu.Unlock()
				go func(p string, f *os.File) {
					readKeys(f, keys)
					mu.Lock()
					if reading[p] == f {
						delete(reading, p)
					}
					mu.Unlock()
					f.Close()
				}(p, f)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return keys
}

// readKeys 读到设备拔出（读错误）为止，每次有按键按下就发信号（不阻塞：救援协程只关心"有没有按"）。
func readKeys(f *os.File, keys chan<- struct{}) {
	buf := make([]byte, 64*inputEventSize)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		if countKeyPresses(buf[:n], inputEventSize) > 0 {
			select {
			case keys <- struct{}{}:
			default:
			}
		}
	}
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	return errno == 0
}

func ioctlInt(fd uintptr, req uintptr, arg uintptr) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	return errno == 0
}

// vtConsole 用 tty1 显示救援信息：信息写进 agetty 的 issue 文件，重启 tty1 的 getty 让它显示在登录提示上方。
type vtConsole struct{}

func (vtConsole) show(info string) {
	if err := os.MkdirAll(filepath.Dir(rescueIssue), 0o755); err == nil {
		if err := writeFileAtomic(rescueIssue, []byte(escapeIssue(info)), 0o644); err != nil {
			log.Printf("agent: rescue console: %v", err)
		}
	}
	// 切到 tty1（播放时前台通常就是它；以防万一），清屏后重启 getty，登录提示上方就是最新信息
	if f, err := os.OpenFile("/dev/tty0", os.O_WRONLY, 0); err == nil {
		ioctlInt(f.Fd(), vtActivate, 1)
		ioctlInt(f.Fd(), vtWaitActive, 1)
		f.Close()
	}
	writeTTY("\033[H\033[2J")
	if out, err := exec.Command("systemctl", "restart", rescueGetty).CombinedOutput(); err != nil {
		// 没有 getty（极简系统）时直接把信息写到控制台上
		log.Printf("agent: rescue console: restart %s: %v %s; writing the info to %s directly", rescueGetty, err, out, rescueTTY)
		writeTTY(info)
	}
}

func (vtConsole) hide() {
	os.Remove(rescueIssue)
	// 播放画面会盖住控制台：注销留在 tty1 上的会话，免得有人对着播放画面盲打进一个已登录的 root shell
	if out, err := exec.Command("systemctl", "restart", rescueGetty).CombinedOutput(); err != nil {
		log.Printf("agent: rescue console: restart %s: %v %s", rescueGetty, err, out)
	}
}

func (vtConsole) close() { os.Remove(rescueIssue) }

func writeTTY(s string) {
	if f, err := os.OpenFile(rescueTTY, os.O_WRONLY|syscall.O_NOCTTY, 0); err == nil {
		f.WriteString(s)
		f.Close()
	}
}
