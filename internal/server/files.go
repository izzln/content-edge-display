package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// 上传落盘与目录清理：媒体、程序包/依赖包、底图共用。上传一律先流式写进暂存区 incoming/（边写边算 sha256，
// 超限即停，不把整个文件读进内存），检查通过后再移到最终位置；暂存区在启动时整个清掉。

var (
	errTooLarge    = errors.New("upload exceeds the size limit")
	errMissingFile = errors.New("upload has no file")
)

// receive 把 r 写进暂存区的临时文件，至多 limit 字节（超了返回 errTooLarge）。出错时临时文件已删掉；
// 成功后由调用方移走或删除 path。
func (s *Server) receive(r io.Reader, limit int64) (path, sha string, n int64, err error) {
	f, err := os.CreateTemp(s.incomingDir(), "upload-*")
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1)) // 多读 1 字节：读得出来说明超限了
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = errTooLarge
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", n, err
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), n, nil
}

// uploadedFile 是一个单文件上传表单：字段 file 落在暂存区。
type uploadedFile struct {
	path, sha string
	size      int64
}

// receiveUpload 读取 multipart 表单（字段 file 至多 limit 字节，其余字段忽略）。成功后由调用方移走或删除 up.path。
func (s *Server) receiveUpload(r *http.Request, limit int64) (up uploadedFile, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return up, err
	}
	defer func() {
		if err != nil && up.path != "" {
			os.Remove(up.path)
		}
	}()
	for {
		part, err := mr.NextPart()
		switch {
		case err == io.EOF && up.path == "":
			return up, errMissingFile
		case err == io.EOF:
			return up, nil
		case err != nil:
			return up, err
		}
		if part.FormName() == "file" && up.path == "" {
			up.path, up.sha, up.size, err = s.receive(part, limit)
		}
		part.Close()
		if err != nil {
			return up, err
		}
	}
}

// uploadError 把 receiveUpload 的错误换成给后台看的说明。
func uploadError(err error, what string, limit int64) error {
	switch {
	case errors.Is(err, errTooLarge):
		return errBadRequest("%s超过 %dMB", what, limit>>20)
	case errors.Is(err, errMissingFile):
		return errBadRequest("缺少%s文件", what)
	}
	return errBadRequest("%s上传中断：%v", what, err)
}

// pruneDir 删掉 dir 里 keep 不保留的条目（文件或目录），返回删掉的名字。
func pruneDir(dir string, keep func(name string, info os.FileInfo) bool) []string {
	entries, _ := os.ReadDir(dir)
	var removed []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || keep(e.Name(), info) {
			continue
		}
		if os.RemoveAll(filepath.Join(dir, e.Name())) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}

// humanBytes 把字节数格式化成 KB/MB（日志与提示用）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}
