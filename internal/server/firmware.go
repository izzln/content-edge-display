package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// updateCommand 判断是否要给设备下发更新指令：
// 有目标 && 到达 not_before && 设备上报版本 ≠ 目标版本 && 固件仍存在。
func (s *Server) updateCommand(deviceID string, now time.Time) (manifest.Command, bool) {
	target, ok := s.store.UpdateTarget(deviceID)
	if !ok || now.Before(target.NotBefore) {
		return manifest.Command{}, false
	}
	if s.agentVersion(deviceID) == target.Version {
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

// handleFirmwareDownload 设备侧下载固件（签名认证 + Range 续传）。
func (s *Server) handleFirmwareDownload(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("file")
	if name == "" || strings.HasPrefix(name, ".") ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.Error(w, "bad file name", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.firmwareDir(), name))
}

// ---- 管理接口 ----

func (s *Server) handleListFirmware(w http.ResponseWriter, r *http.Request) {
	var out []store.Firmware
	s.store.View(func(st *store.State) {
		for _, f := range st.Firmware {
			out = append(out, f)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UploadedAt.After(out[j].UploadedAt) })
	if out == nil {
		out = []store.Firmware{}
	}
	writeJSON(w, out)
}

// handleUploadFirmware 接收 multipart: file + version + notes。
func (s *Server) handleUploadFirmware(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "bad multipart body", http.StatusBadRequest)
		return
	}
	version := strings.TrimSpace(r.FormValue("version"))
	if !versionPattern.MatchString(version) {
		http.Error(w, "版本号非法（字母数字._-，≤64）", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer f.Close()

	name := "display-agent-" + version
	dst := filepath.Join(s.firmwareDir(), name)
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, 64<<20))
	out.Close()
	if err != nil || n == 0 {
		os.Remove(tmp)
		http.Error(w, "empty or unreadable file", http.StatusBadRequest)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fw := store.Firmware{
		Version: version, File: name, SHA256: hex.EncodeToString(h.Sum(nil)),
		Size: n, Notes: r.FormValue("notes"), UploadedAt: s.now(),
	}
	if err := s.store.Update(func(st *store.State) error { st.Firmware[version] = fw; return nil }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, fw)
}

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
	err := s.store.Update(func(st *store.State) error {
		file = st.Firmware[version].File
		delete(st.Firmware, version)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
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
			if _, ok := s.deviceByID(id); !ok {
				http.Error(w, "未知设备 "+id, http.StatusBadRequest)
				return
			}
		}
	}
	now := s.now()
	err := s.store.Update(func(st *store.State) error {
		for _, id := range targets {
			if req.Version == "" {
				delete(st.Updates, id)
			} else {
				st.Updates[id] = store.UpdateTarget{Version: req.Version, NotBefore: req.NotBefore, CreatedAt: now}
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"devices": targets, "version": req.Version, "not_before": req.NotBefore})
}
