package server

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/agentpkg"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

// 设备端程序包（make package 产出的 tar.gz）：后台上传 → 立即/定时下发 → 设备执行包内 update.sh 切换；
// 最新上传的包同时也是新设备一键装机下载的包（bootstrap.go）。

// maxPackageBytes 是设备程序包的大小上限。
const maxPackageBytes = 64 << 20

// 设备端的目标平台（Orange Pi One = ARMv7）。
const agentGOOS, agentGOARCH = "linux", "arm"

// 从构建信息的 -ldflags 中取出注入的代理版本号。
var versionLdflagPattern = regexp.MustCompile(`-X\s+\S*internal/agent\.Version=(\S+)`)

// inspectPackage 解开上传的程序包检查：VERSION、update.sh 齐全，代理程序是 linux/arm 的 Go 程序且内置版本与
// VERSION 一致。返回版本号。
//
// 没有这道校验时，本机架构的程序、没注入版本号的程序都会被原样分发到所有设备：设备装上后
// systemd 执行失败，要连续失败 3 次才触发回滚，期间屏幕是黑的。Go 的构建信息可跨架构读取，
// 因此这些错误都能在上传时当场挡住。
func inspectPackage(pkg string) (string, error) {
	dir, err := os.MkdirTemp(filepath.Dir(pkg), ".inspect-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	f, err := os.Open(pkg)
	if err != nil {
		return "", err
	}
	defer f.Close()
	version := ""
	if err = agentpkg.Extract(f, dir); err == nil {
		version, err = agentpkg.Check(dir)
	}
	if err != nil {
		return "", fmt.Errorf("这不是有效的设备程序包（%v）——请上传 make package 产出的 %s", err, agentpkg.FileName("<版本>"))
	}
	info, err := buildinfo.ReadFile(filepath.Join(dir, agentpkg.Binary))
	if err != nil {
		return "", fmt.Errorf("包里的 %s 不是 Go 程序", agentpkg.Binary)
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if goos, goarch := settings["GOOS"], settings["GOARCH"]; goos != agentGOOS || goarch != agentGOARCH {
		return "", fmt.Errorf("包里程序的目标平台是 %s/%s，设备需要 %s/%s——请用 make package 打包", goos, goarch, agentGOOS, agentGOARCH)
	}
	m := versionLdflagPattern.FindStringSubmatch(settings["-ldflags"])
	if m == nil {
		return "", errors.New("包里的程序没有注入版本号，请用 make package 打包")
	}
	if got := strings.Trim(m[1], `"'`); got != version {
		return "", fmt.Errorf("程序内置版本是 %q，与包的 VERSION %q 不一致", got, version)
	}
	return version, nil
}

// pendingUpdate 返回要随清单下发给设备的程序更新：有目标、到了 not_before、设备上报的版本还不是目标版本、
// 程序包仍在。定时由服务端判定，设备不需要可信的时钟。
func pendingUpdate(st *store.State, deviceID string, now time.Time) *manifest.Update {
	target, ok := st.Updates[deviceID]
	if !ok || now.Before(target.NotBefore) || st.Devices[deviceID].AgentVersion == target.Version {
		return nil
	}
	p, ok := st.Packages[target.Version]
	if !ok {
		return nil
	}
	return &manifest.Update{Version: p.Version, URL: "/packages/" + url.PathEscape(p.File), SHA256: p.SHA256, Size: p.Size}
}

// ---- 管理接口 ----

func (s *Server) handleListPackages(w http.ResponseWriter, r *http.Request) {
	out := []store.Package{}
	s.store.View(func(st *store.State) { out = append(out, slices.Collect(maps.Values(st.Packages))...) })
	slices.SortFunc(out, func(a, b store.Package) int { return b.UploadedAt.Compare(a.UploadedAt) }) // 新的在前
	writeJSON(w, out)
}

// handleUploadPackage 接收 multipart：file（程序包）+ notes（可选）。版本号从包里读。
// 流式写盘，不把整个包读进内存。
func (s *Server) handleUploadPackage(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad multipart body", http.StatusBadRequest)
		return
	}
	var notes, tmp string
	h := sha256.New()
	var n int64
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad multipart body", http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "notes":
			b, _ := io.ReadAll(io.LimitReader(part, 1<<10))
			notes = strings.TrimSpace(string(b))
		case "file":
			f, err := os.CreateTemp(s.packagesDir(), ".upload-*")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			tmp = f.Name()
			n, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(part, maxPackageBytes+1))
			f.Close()
			if err != nil {
				http.Error(w, "upload interrupted", http.StatusBadRequest)
				return
			}
		}
		part.Close()
	}
	switch {
	case tmp == "" || n == 0:
		http.Error(w, "缺少程序包文件", http.StatusBadRequest)
		return
	case n > maxPackageBytes:
		http.Error(w, fmt.Sprintf("程序包超过 %dMB", maxPackageBytes>>20), http.StatusBadRequest)
		return
	}
	// 在落库前把"传错文件"挡住——这个包会分发到所有设备。
	version, err := inspectPackage(tmp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := agentpkg.FileName(version)
	if err := os.Rename(tmp, filepath.Join(s.packagesDir(), name)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp = ""
	p := store.Package{Version: version, File: name, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Notes: notes, UploadedAt: s.now()}
	if s.update(w, func(st *store.State) { st.Packages[version] = p }) {
		log.Printf("agent package %s uploaded (%s)", version, humanBytes(n))
		writeJSON(w, p)
	}
}

func (s *Server) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	var inUse []string
	var file string
	s.store.View(func(st *store.State) {
		for dev, u := range st.Updates {
			if u.Version == version {
				inUse = append(inUse, dev)
			}
		}
		file = st.Packages[version].File
	})
	if len(inUse) > 0 {
		http.Error(w, fmt.Sprintf("该版本仍是设备的更新目标: %s", strings.Join(inUse, ", ")), http.StatusConflict)
		return
	}
	if !s.update(w, func(st *store.State) { delete(st.Packages, version) }) {
		return
	}
	if file != "" {
		os.Remove(filepath.Join(s.packagesDir(), file))
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRollout 为指定（或全部）设备设置更新目标；version 为空则取消。
func (s *Server) handleRollout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version   string    `json:"version"`              // 空串 = 取消目标
		NotBefore time.Time `json:"not_before,omitempty"` // 零值 = 立即
		Devices   []string  `json:"devices,omitempty"`    // 空 = 全部设备
	}
	if !decodeJSON(w, r, 64<<10, &req) {
		return
	}
	var bad string
	s.store.View(func(st *store.State) {
		if _, ok := st.Packages[req.Version]; req.Version != "" && !ok {
			bad = "程序包版本不存在"
			return
		}
		if len(req.Devices) == 0 {
			for id := range st.Devices {
				req.Devices = append(req.Devices, id)
			}
		}
		for _, id := range req.Devices {
			if _, ok := st.Devices[id]; !ok {
				bad = "未知设备 " + id
			}
		}
	})
	if bad != "" {
		http.Error(w, bad, http.StatusBadRequest)
		return
	}
	now := s.now()
	if s.update(w, func(st *store.State) {
		for _, id := range req.Devices {
			if req.Version == "" {
				delete(st.Updates, id)
			} else {
				st.Updates[id] = store.UpdateTarget{Version: req.Version, NotBefore: req.NotBefore, CreatedAt: now}
			}
		}
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
