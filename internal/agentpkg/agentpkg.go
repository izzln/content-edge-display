// Package agentpkg 读取设备端程序包（make package 产出的 display-agent-<版本>-armv7.tar.gz）。
//
// 程序包既是装机包（install.sh 下载后执行包内 install-agent.sh），也是 OTA 包（代理下载、解开后执行包内
// update.sh）：程序和配套脚本一起更新、一起回滚。包内有一层顶层目录 display-agent-<版本>/，解包时去掉。
package agentpkg

import (
	"archive/tar"
	"compress/gzip"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// 包内的固定文件。版本号只有一份：程序构建时注入的版本（Inspect 从构建信息里读）。
const (
	Binary        = "display-agent"    // 代理程序（linux/arm）
	UpdateScript  = "update.sh"        // 安装步骤：首次安装与 OTA 都执行
	InstallScript = "install-agent.sh" // 装机入口（install.sh 下载最新的包后执行）
)

// MaxExtracted 是解包后的总大小上限：真正的包不到 20MB，挡住解出几十 GB 的压缩炸弹。
const MaxExtracted = 256 << 20

// 设备端的目标平台（Orange Pi One = ARMv7）。
const GOOS, GOARCH = "linux", "arm"

var (
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// 构建信息的 -ldflags 里注入的代理版本号
	versionLdflagPattern = regexp.MustCompile(`-X\s+\S*internal/agent\.Version=(\S+)`)
)

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

// Check 检查解开后的包是否齐全：代理程序、update.sh、install-agent.sh。
func Check(dir string) error {
	for _, name := range []string{Binary, UpdateScript, InstallScript} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("package has no %s", name)
		}
	}
	return nil
}

// Inspect 在 Check 之外再确认包里的代理程序是 linux/arm 的 Go 程序、注入了版本号，返回这个版本号。
//
// 没有这道校验时，本机架构的程序、没注入版本号的程序都会被原样分发到所有设备：设备装上后
// systemd 执行失败，要连续失败 3 次才触发回滚，期间屏幕是黑的。Go 的构建信息可跨架构读取，
// 因此这些错误都能在上传时当场挡住。
func Inspect(dir string) (string, error) {
	if err := Check(dir); err != nil {
		return "", err
	}
	info, err := buildinfo.ReadFile(filepath.Join(dir, Binary))
	if err != nil {
		return "", fmt.Errorf("包里的 %s 不是 Go 程序", Binary)
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if goos, goarch := settings["GOOS"], settings["GOARCH"]; goos != GOOS || goarch != GOARCH {
		return "", fmt.Errorf("包里程序的目标平台是 %s/%s，设备需要 %s/%s", goos, goarch, GOOS, GOARCH)
	}
	m := versionLdflagPattern.FindStringSubmatch(settings["-ldflags"])
	if m == nil {
		return "", errors.New("包里的程序没有注入版本号")
	}
	version := strings.Trim(m[1], `"'`)
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("程序内置的版本号 %q 不合法", version)
	}
	return version, nil
}
