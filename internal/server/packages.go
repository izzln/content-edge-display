package server

import (
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/agentpkg"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

// 设备端程序包（make package 产出的 tar.gz）：后台上传 → 立即/定时下发 → 设备执行包内 update.sh 切换；
// 最新上传的包同时也是新设备一键装机下载的包（bootstrap.go）。离线依赖包从同一个入口上传（deps.go）。

// maxPackageBytes 是上传的程序包/离线依赖包的大小上限（依赖包一两百 MB）。
const maxPackageBytes = 512 << 20

func invalidPackage(err error) error {
	return errBadRequest("这不是有效的设备程序包（%v）——请上传 make package 产出的 %s", err, agentpkg.FileName("<版本>"))
}

// pendingUpdate 返回要随清单下发给设备的程序更新：有目标、到了 not_before、设备上报的版本还不是目标版本、
// 程序包仍在。定时由服务端判定，设备不需要可信的时钟。
func pendingUpdate(st *store.State, d *store.Device, now time.Time) *manifest.Update {
	target := d.Update
	if target == nil || now.Before(target.NotBefore) || d.AgentVersion == target.Version {
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

// handleUploadPackage 接收 multipart：file（程序包或离线依赖包，按内容区分）+ notes（可选）。版本号从包里读。
// 流式写进暂存区，不把整个包读进内存。
func (s *Server) handleUploadPackage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPackageBytes+1<<20)
	up, err := s.receiveUpload(r, maxPackageBytes)
	if err != nil {
		writeError(w, uploadError(err, "程序包", maxPackageBytes))
		return
	}
	defer os.Remove(up.path)
	dir, err := os.MkdirTemp(s.incomingDir(), "unpack-")
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.RemoveAll(dir)
	f, err := os.Open(up.path)
	if err != nil {
		writeError(w, err)
		return
	}
	err = agentpkg.Extract(f, dir)
	f.Close()
	if err != nil {
		writeError(w, invalidPackage(err))
		return
	}
	if isDepsBundle(dir) {
		repo, err := s.installDeps(dir)
		if err != nil {
			writeError(w, err)
			return
		}
		log.Printf("dependency repo %s uploaded (%d packages, %s)", repo.Codename, repo.Packages, humanBytes(repo.Size))
		writeJSON(w, repo)
		return
	}
	// 在落库前把"传错文件"挡住——这个包会分发到所有设备。
	version, err := agentpkg.Inspect(dir)
	if err != nil {
		writeError(w, invalidPackage(err))
		return
	}
	name := agentpkg.FileName(version)
	if err := os.Rename(up.path, filepath.Join(s.packagesDir(), name)); err != nil {
		writeError(w, err)
		return
	}
	p := store.Package{Version: version, File: name, SHA256: up.sha, Size: up.size, Notes: up.fields["notes"], UploadedAt: s.now()}
	if s.update(w, func(st *store.State) error { st.Packages[version] = p; return nil }) {
		log.Printf("agent package %s uploaded (%s)", version, humanBytes(up.size))
		writeJSON(w, p)
	}
}

// handleDeletePackage 删除程序包；仍是某台设备更新目标的不能删。
func (s *Server) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	var file string
	if !s.update(w, func(st *store.State) error {
		var inUse []string
		for _, d := range sortedByID(st.Devices) {
			if d.Update != nil && d.Update.Version == version {
				inUse = append(inUse, d.ID)
			}
		}
		if len(inUse) > 0 {
			return errConflict("该版本仍是设备的更新目标: %s", strings.Join(inUse, ", "))
		}
		p, ok := st.Packages[version]
		if !ok {
			return errNotFound("程序包")
		}
		file = p.File
		delete(st.Packages, version)
		return nil
	}) {
		return
	}
	os.Remove(filepath.Join(s.packagesDir(), file))
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
	if s.update(w, func(st *store.State) error {
		if _, ok := st.Packages[req.Version]; req.Version != "" && !ok {
			return errBadRequest("程序包版本不存在")
		}
		if len(req.Devices) == 0 {
			req.Devices = slices.Collect(maps.Keys(st.Devices))
		}
		for _, id := range req.Devices {
			if st.Devices[id] == nil {
				return errBadRequest("未知设备 %s", id)
			}
		}
		for _, id := range req.Devices {
			st.Devices[id].Update = nil
			if req.Version != "" {
				st.Devices[id].Update = &store.UpdateTarget{Version: req.Version, NotBefore: req.NotBefore}
			}
		}
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
