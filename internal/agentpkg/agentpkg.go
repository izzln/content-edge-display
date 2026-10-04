// Package agentpkg 读取设备端整包（make package 产出的 display-agent-<版本>-armv7.tar.gz）。
//
// 整包既是装机包（install.sh 下载后执行包内 install-agent.sh），也是 OTA 包（代理下载、解开后执行包内
// update.sh）：程序和配套脚本一起更新、一起回滚。包内有一层顶层目录 display-agent-<版本>/，读取时去掉。
package agentpkg

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 包内的固定文件。
const (
	Binary       = "display-agent" // 代理程序（linux/arm）
	VersionFile  = "VERSION"       // 版本号，与程序内置版本一致
	UpdateScript = "update.sh"     // 安装步骤：首次安装与 OTA 都执行
)

// Walk 逐个读出包内的普通文件（路径已去掉顶层目录），交给 fn。
func Walk(r io.Reader, fn func(name string, h *tar.Header, body io.Reader) error) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return errors.New("not a gzip file")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("broken tar: %w", err)
		}
		name, err := relName(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if name == "" {
				continue
			}
			if err := fn(name, h, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q in package (only files and directories)", h.Name)
		}
	}
}

// relName 去掉顶层目录并检查路径安全（不能跳出解包目录）。
func relName(name string) (string, error) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if _, rest, ok := strings.Cut(name, "/"); ok {
		return rest, nil
	}
	return "", nil // 顶层目录本身
}

// Extract 把包解到 dir（不存在会创建），保留文件的可执行权限。
func Extract(r io.Reader, dir string) error {
	return Walk(r, func(name string, h *tar.Header, body io.Reader) error {
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
}

// Version 读取解开后的包目录里的版本号。
func Version(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, VersionFile))
	if err != nil {
		return "", fmt.Errorf("package has no %s", VersionFile)
	}
	return strings.TrimSpace(string(data)), nil
}
