package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockCacheDir 独占缓存目录：同一目录只允许一个代理进程。
//
// 两个进程共用一个 cache_dir（例如服务在跑时又手工启动了一个来调试）会互相删对方下载的文件，
// 首次启动时还会各自生成一把密钥、后写的覆盖先写的——留在 identity.json 里的那把可能
// 正好不是服务端登记的那把，从此每次启动都被拒绝注册。
func lockCacheDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("另一个 display-agent 正在使用 %s（systemctl status display-agent），不要同时运行两个", dir)
	}
	return func() { f.Close() }, nil // 关闭即释放锁；进程退出时内核也会释放
}
