package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// download 将条目下载到 dst：经 .part 临时文件断点续传，完成后校验 sha256 再改名。
func (a *Agent) download(ctx context.Context, item manifest.Item, dst string) error {
	part := dst + ".part"

	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}
	if offset > item.Size {
		// 残留的 .part 比目标还大，只能重来。
		if err := os.Remove(part); err != nil {
			return err
		}
		offset = 0
	}

	// manifest 中的 URL 是转义后的相对路径；签名须基于解码后的 path，
	// newRequest 内部已按 req.URL.Path（解码形式）签名，这里直接透传。
	u, err := url.Parse(item.URL)
	if err != nil {
		return err
	}
	req, err := a.newRequest(ctx, http.MethodGet, u.EscapedPath(), nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := a.http.Do(req)
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
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// 完整性校验：不符则删除，等下次轮询重下。
	sum, err := fileSHA256(part)
	if err != nil {
		return err
	}
	if sum != item.SHA256 {
		os.Remove(part)
		return fmt.Errorf("sha256 mismatch: got %s want %s", sum, item.SHA256)
	}
	return os.Rename(part, dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
