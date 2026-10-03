package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// downloadFile 经 .part 临时文件断点续传下载 urlPath 到 dst，完成后校验 sha256 再改名。
// 媒体、渲染图、固件共用此路径。
func (a *Agent) downloadFile(ctx context.Context, urlPath, wantSHA string, size int64, dst string) error {
	part := dst + ".part"

	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}
	if offset > size {
		// 残留的 .part 比目标还大，只能重来。
		if err := os.Remove(part); err != nil {
			return err
		}
		offset = 0
	}

	// manifest 中的 URL 是转义后的相对路径；签名须基于解码后的 path，
	// newRequest 内部已按 req.URL.Path（解码形式）签名，这里直接透传。
	u, err := url.Parse(urlPath)
	if err != nil {
		return err
	}
	// 不设总超时（几百 MB 的视频在慢网上要下很久），改为停滞检测：stallTimeout 内一个字节都没收到
	// 就放弃，下次轮询从 .part 续传。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := a.newRequest(ctx, http.MethodGet, u.EscapedPath(), nil)
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

	var f *os.File
	switch resp.StatusCode {
	case http.StatusPartialContent: // 断点续传
		f, err = os.OpenFile(part, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	case http.StatusOK: // 服务器不认续传或从头下载
		f, err = os.Create(part)
	default:
		return statusError(resp)
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, newProgressReader(resp.Body, cancel)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// 完整性校验：不符则删除，等下次重下。
	sum, err := manifest.FileSHA256(part)
	if err != nil {
		return err
	}
	if sum != wantSHA {
		os.Remove(part)
		return fmt.Errorf("sha256 mismatch: got %s want %s", sum, wantSHA)
	}
	return os.Rename(part, dst)
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
