package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
)

// downloadFile 经 .part 临时文件断点续传下载 urlPath 到 dst，边写边算 sha256，校验通过才改名。
// 媒体、渲染图、程序包共用此路径。
func (a *Agent) downloadFile(ctx context.Context, urlPath, wantSHA string, size int64, dst string) error {
	part := dst + ".part"
	h := sha256.New()
	offset, err := hashExisting(part, size, h)
	if err != nil {
		return err
	}
	if offset < size {
		if err := a.fetch(ctx, urlPath, part, offset, h); err != nil {
			return err
		}
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != wantSHA {
		os.Remove(part) // 等下次重下
		return fmt.Errorf("sha256 mismatch: got %s want %s", sum, wantSHA)
	}
	// 先落盘再改名：current.json 是 fsync 过的，它引用的文件也必须是。否则下完不久断电（设备没有 UPS），
	// 重启后 current.json 还在、文件却是空的，断网时恢复不了播放。
	if err := syncFile(part); err != nil {
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	fsutil.SyncDir(filepath.Dir(dst))
	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// hashExisting 把上次下载中断留下的 .part 喂进 h，返回续传起点；比目标还大的残留只能重来。
func hashExisting(part string, size int64, h hash.Hash) (int64, error) {
	f, err := os.Open(part)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() > size {
		return 0, os.Remove(part)
	}
	return io.Copy(h, f)
}

// fetch 从 offset 处续传到 part（服务端不认续传时从头下载），写入的数据同时进 h。
func (a *Agent) fetch(ctx context.Context, urlPath, part string, offset int64, h hash.Hash) error {
	// manifest 中的 URL 是转义后的相对路径；newRequest 按解码后的 path 签名。
	u, err := url.Parse(urlPath)
	if err != nil {
		return err
	}
	// 不设总超时（几百 MB 的视频在慢网上要下很久），改为停滞检测：stallTimeout 内一个字节都没收到
	// 就放弃，下次轮询从 .part 续传。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := a.newRequest(ctx, http.MethodGet, u.EscapedPath(), nil, true)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := a.dl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flag := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		h.Reset()
	default:
		return statusError(resp)
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.MultiWriter(f, h), newProgressReader(resp.Body, cancel))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// stallTimeout 是下载停滞多久算失败；必须小于 systemd 看门狗的 90 秒。
const stallTimeout = 60 * time.Second

// progressReader 在下载过程中做两件事：
//   - 有数据进来就喂 systemd 看门狗。大文件下载可能持续几分钟，主循环这段时间不会回到喂狗点，
//     不喂的话 90 秒后 systemd 认定假死，连同播放进程一起杀掉重启——屏幕黑一下，下载从头再来；
//   - 超过 stallTimeout 没收到任何数据就取消请求（服务端或网络卡死），让主循环继续。
type progressReader struct {
	r      io.Reader
	stall  time.Duration
	timer  *time.Timer
	lastWD time.Time
}

func newProgressReader(r io.Reader, cancel context.CancelFunc) *progressReader {
	return &progressReader{r: r, stall: stallTimeout, timer: time.AfterFunc(stallTimeout, cancel)}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.timer.Reset(p.stall)
		if time.Since(p.lastWD) > 10*time.Second {
			sdNotify("WATCHDOG=1")
			p.lastWD = time.Now()
		}
	}
	if err != nil {
		p.timer.Stop()
	}
	return n, err
}
