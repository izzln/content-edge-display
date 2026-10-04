package server

import (
	"archive/tar"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/agentpkg"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// 设备端的目标平台（Orange Pi One = ARMv7）。
const agentGOOS, agentGOARCH = "linux", "arm"

// 从构建信息的 -ldflags 中取出注入的代理版本号。
var versionLdflagPattern = regexp.MustCompile(`-X\s+\S*internal/agent\.Version=(\S+)`)

// validateAgentBinary 校验包里的确实是设备端能执行的代理程序，且其内置版本与包的 VERSION 一致。
//
// 没有这道校验时，本机架构的程序、没注入版本号的程序都会被原样分发到所有设备：设备装上后
// systemd 执行失败，要连续失败 3 次才触发回滚，期间屏幕是黑的。Go 的构建信息可跨架构读取，
// 因此这些错误都能在上传时当场挡住。
func validateAgentBinary(path, version string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("包里的 %s 不是 Go 程序", agentpkg.Binary)
	}
	var goos, goarch, ldflags string
	for _, s := range info.Settings {
		switch s.Key {
		case "GOOS":
			goos = s.Value
		case "GOARCH":
			goarch = s.Value
		case "-ldflags":
			ldflags = s.Value
		}
	}
	if goos != agentGOOS || goarch != agentGOARCH {
		return fmt.Errorf("包里程序的目标平台是 %s/%s，设备需要 %s/%s——请用 make package 打包", goos, goarch, agentGOOS, agentGOARCH)
	}
	m := versionLdflagPattern.FindStringSubmatch(ldflags)
	if m == nil {
		return fmt.Errorf("包里的程序没有注入版本号，请用 make package 打包")
	}
	if got := strings.Trim(m[1], `"'`); got != version {
		return fmt.Errorf("程序内置版本是 %q，与包的 VERSION %q 不一致", got, version)
	}
	return nil
}

// inspectPackage 检查上传的整包：必须有 VERSION、update.sh 和 linux/arm 的代理程序，返回版本号。
func inspectPackage(pkgPath string) (string, error) {
	f, err := os.Open(pkgPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	bin, err := os.CreateTemp(filepath.Dir(pkgPath), ".agent-bin-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(bin.Name())
	defer bin.Close()
	var version string
	var hasBinary, hasUpdate bool
	err = agentpkg.Walk(f, func(name string, _ *tar.Header, body io.Reader) error {
		switch name {
		case agentpkg.VersionFile:
			b, err := io.ReadAll(io.LimitReader(body, 256))
			version = strings.TrimSpace(string(b))
			return err
		case agentpkg.Binary:
			hasBinary = true
			_, err := io.Copy(bin, body)
			return err
		case agentpkg.UpdateScript:
			hasUpdate = true
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("这不是设备程序包（%v）——请上传 make package 产出的 display-agent-<版本>-armv7.tar.gz", err)
	}
	switch {
	case !versionPattern.MatchString(version):
		return "", fmt.Errorf("包里没有合法的 %s——请上传 make package 产出的设备程序包", agentpkg.VersionFile)
	case !hasBinary:
		return "", fmt.Errorf("包里没有 %s", agentpkg.Binary)
	case !hasUpdate:
		return "", fmt.Errorf("包里没有 %s", agentpkg.UpdateScript)
	}
	return version, validateAgentBinary(bin.Name(), version)
}

// updateCommand 判断是否要给设备下发更新指令：
// 有目标 && 到达 not_before && 设备上报版本 ≠ 目标版本 && 固件仍存在。
func (s *Server) updateCommand(deviceID string, now time.Time) (manifest.Command, bool) {
	target, ok := s.store.UpdateTarget(deviceID)
	if !ok || now.Before(target.NotBefore) {
		return manifest.Command{}, false
	}
	if dev, _ := s.store.Device(deviceID); dev.AgentVersion == target.Version {
		return manifest.Command{}, false
	}
	fw, ok := s.store.FirmwareByVersion(target.Version)
	if !ok {
		return manifest.Command{}, false
	}
	return manifest.Command{
		Type:    "update",
		Version: fw.Version,
		URL:     "/firmware/" + url.PathEscape(fw.File),
		SHA256:  fw.SHA256,
		Size:    fw.Size,
	}, true
}

// ---- 管理接口 ----

func (s *Server) handleListFirmware(w http.ResponseWriter, r *http.Request) {
	var out []store.Firmware
	s.store.View(func(st *store.State) {
		out = sortedValues(st.Firmware, func(a, b store.Firmware) int { return b.UploadedAt.Compare(a.UploadedAt) }) // 新的在前
	})
	writeJSON(w, out)
}

// handleUploadFirmware 接收 multipart: file（整包）+ notes。版本号从包里读。
func (s *Server) handleUploadFirmware(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "bad multipart body", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer f.Close()

	tmp, err := os.CreateTemp(s.firmwareDir(), ".upload-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(f, maxPackageBytes+1))
	tmp.Close()
	switch {
	case err != nil || n == 0:
		http.Error(w, "empty or unreadable file", http.StatusBadRequest)
		return
	case n > maxPackageBytes:
		http.Error(w, fmt.Sprintf("程序包超过 %dMB", maxPackageBytes>>20), http.StatusBadRequest)
		return
	}
	// 在落库前把"传错文件"挡住——这个包会分发到所有设备。
	version, err := inspectPackage(tmp.Name())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := "display-agent-" + version + "-armv7.tar.gz"
	if err := os.Rename(tmp.Name(), filepath.Join(s.firmwareDir(), name)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fw := store.Firmware{
		Version: version, File: name, SHA256: hex.EncodeToString(h.Sum(nil)),
		Size: n, Notes: r.FormValue("notes"), UploadedAt: s.now(),
	}
	if s.update(w, func(st *store.State) { st.Firmware[version] = fw }) {
		log.Printf("agent package %s uploaded (%s)", version, humanBytes(n))
		writeJSON(w, fw)
	}
}

// maxPackageBytes 是设备程序包的大小上限。
const maxPackageBytes = 64 << 20

func (s *Server) handleDeleteFirmware(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	var inUse []string
	s.store.View(func(st *store.State) {
		for dev, u := range st.Updates {
			if u.Version == version {
				inUse = append(inUse, dev)
			}
		}
	})
	if len(inUse) > 0 {
		http.Error(w, fmt.Sprintf("该版本仍是设备的更新目标: %s", strings.Join(inUse, ", ")), http.StatusConflict)
		return
	}
	var file string
	if !s.update(w, func(st *store.State) {
		file = st.Firmware[version].File
		delete(st.Firmware, version)
	}) {
		return
	}
	if file != "" {
		os.Remove(filepath.Join(s.firmwareDir(), file))
	}
	w.WriteHeader(http.StatusNoContent)
}

// RolloutRequest 是下发更新的请求体。
type RolloutRequest struct {
	Version   string    `json:"version"`              // 空串 = 取消目标
	NotBefore time.Time `json:"not_before,omitempty"` // 零值 = 立即
	Devices   []string  `json:"devices,omitempty"`    // 空 = 全部设备
}

// handleRollout 为指定（或全部）设备设置更新目标；version 为空则取消。
func (s *Server) handleRollout(w http.ResponseWriter, r *http.Request) {
	var req RolloutRequest
	if !decodeJSON(w, r, 64<<10, &req) {
		return
	}
	if req.Version != "" {
		if _, ok := s.store.FirmwareByVersion(req.Version); !ok {
			http.Error(w, "固件版本不存在", http.StatusBadRequest)
			return
		}
	}
	targets := req.Devices
	if len(targets) == 0 {
		for _, d := range s.allDevices() {
			targets = append(targets, d.ID)
		}
	} else {
		for _, id := range targets {
			if _, ok := s.store.Device(id); !ok {
				http.Error(w, "未知设备 "+id, http.StatusBadRequest)
				return
			}
		}
	}
	now := s.now()
	if s.update(w, func(st *store.State) {
		for _, id := range targets {
			if req.Version == "" {
				delete(st.Updates, id)
			} else {
				st.Updates[id] = store.UpdateTarget{Version: req.Version, NotBefore: req.NotBefore, CreatedAt: now}
			}
		}
	}) {
		writeJSON(w, map[string]any{"devices": targets, "version": req.Version, "not_before": req.NotBefore})
	}
}
