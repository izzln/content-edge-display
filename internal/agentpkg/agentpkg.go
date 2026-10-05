// Package agentpkg 读取设备端程序包（make package 产出的 display-agent-<版本>-armv7.tar.gz）。
//
// 程序包既是装机包（install.sh 下载后执行包内 install-agent.sh），也是 OTA 包（代理下载、解开后执行包内
// update.sh）：程序和配套脚本一起更新、一起回滚。包内有一层顶层目录 display-agent-<版本>/，解包时去掉。
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
	"regexp"
	"strings"
)

// 包内的固定文件。
const (
	Binary        = "display-agent"    // 代理程序（linux/arm）
	VersionFile   = "VERSION"          // 版本号，与程序内置版本一致
	UpdateScript  = "update.sh"        // 安装步骤：首次安装与 OTA 都执行
	InstallScript = "install-agent.sh" // 装机入口（install.sh 下载最新的包后执行）
)

// MaxExtracted 是解包后的总大小上限：真正的包不到 20MB，挡住解出几十 GB 的压缩炸弹。
const MaxExtracted = 256 << 20

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// FileName 是版本 version 的程序包文件名。
func FileName(version string) string { return "display-agent-" + version + "-armv7.tar.gz" }

// Extract 把包解到 dir（不存在会创建），保留文件的可执行权限。只接受普通文件与目录，
// 路径一律收在 dir 之内，总大小不超过 MaxExtracted。
func Extract(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return errors.New("not a gzip file")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("broken tar: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return fmt.Errorf("unsupported entry %q in package (only files and directories)", h.Name)
		}
		// 去掉顶层目录；Clean 掉 .. 之后路径不会跳出 dir
		_, name, ok := strings.Cut(strings.TrimPrefix(path.Clean("/"+h.Name), "/"), "/")
		if !ok {
			continue
		}
		if total += h.Size; total > MaxExtracted {
			return fmt.Errorf("package unpacks to more than %dMB", MaxExtracted>>20)
		}
		if err := writeEntry(filepath.Join(dir, filepath.FromSlash(name)), h, tr); err != nil {
			return err
		}
	}
}

func writeEntry(dst string, h *tar.Header, body io.Reader) error {
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
}

// Check 检查解开后的包是否齐全（VERSION、代理程序、update.sh、install-agent.sh），返回版本号。
func Check(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, VersionFile))
	if err != nil {
		return "", fmt.Errorf("package has no %s", VersionFile)
	}
	version := strings.TrimSpace(string(data))
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("package %s %q is invalid", VersionFile, version)
	}
	for _, name := range []string{Binary, UpdateScript, InstallScript} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return "", fmt.Errorf("package has no %s", name)
		}
	}
	return version, nil
}
