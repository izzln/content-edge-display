package server

import (
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/izzln/content-edge-display/internal/agentpkg"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

// 设备端程序包（make package 产出的 tar.gz）：后台上传 → 立即/定时下发 → 设备执行包内 update.sh 切换；
// 服务端只保留一个程序包，它同时也是新设备一键装机下载的包（bootstrap.go）。离线依赖包从同一个入口上传（deps.go）。

// maxPackageBytes 是上传的程序包/离线依赖包的大小上限（依赖包一两百 MB）。
const maxPackageBytes = 512 << 20

func invalidPackage(err error) error {
	return errBadRequest("这不是有效的设备程序包（%v）——请上传 make package 产出的 %s", err, agentpkg.FileName("<版本>"))
}

// pendingUpdate 返回要随清单下发给设备的程序更新：有目标、到了 not_before、设备上报的版本还不是目标版本、
// 目标就是当前程序包。定时由服务端判定，设备不需要可信的时钟。
func pendingUpdate(st *store.State, d *store.Device, now time.Time) *manifest.Update {
	target, p := d.Update, st.Package
	if target == nil || p == nil || target.Version != p.Version || now.Before(target.NotBefore) || d.AgentVersion == target.Version {
		return nil
	}
	return &manifest.Update{Version: p.Version, URL: "/packages/" + url.PathEscape(p.File), SHA256: p.SHA256, Size: p.Size}
}

// ---- 管理接口 ----

// handleGetPackage 返回当前的程序包；还没上传过时返回 null。
func (s *Server) handleGetPackage(w http.ResponseWriter, r *http.Request) {
	var p *store.Package
	s.store.View(func(st *store.State) {
		if st.Package != nil {
			c := *st.Package
			p = &c
		}
	})
	writeJSON(w, p)
}

// handleUploadPackage 接收 multipart：file（程序包或离线依赖包，按内容区分）+ notes（可选）。版本号从包里读。
// 流式写进暂存区，不把整个包读进内存。
//
// 程序包只保留一个：新包替换旧包。设备上仍在等待的更新目标改指向新版本（定时不变），
// 已经更新到位的目标清掉——否则它们会被当成"要更新到新版本"。
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
	var old string
	retargeted := 0
	if !s.update(w, func(st *store.State) error {
		if st.Package != nil {
			old = st.Package.Version
		}
		st.Package = &p
		for _, d := range st.Devices {
			switch {
			case d.Update == nil:
			case d.Update.Version != d.AgentVersion && d.AgentVersion != version:
				d.Update.Version = version
				retargeted++
			default:
				d.Update = nil
			}
		}
		return nil
	}) {
		return
	}
	s.prunePackages(name)
	if old != "" && old != version {
		log.Printf("agent package %s uploaded (%s), replacing %s; %d pending update(s) now target it", version, humanBytes(up.size), old, retargeted)
	} else {
		log.Printf("agent package %s uploaded (%s)", version, humanBytes(up.size))
	}
	writeJSON(w, map[string]any{"package": p, "retargeted": retargeted})
}

// prunePackages 删掉程序包目录里除 keep 以外的文件（被替换的旧包、上次没删干净的残留）。
func (s *Server) prunePackages(keep string) {
	entries, _ := os.ReadDir(s.packagesDir())
	for _, e := range entries {
		if e.Name() != keep {
			os.Remove(filepath.Join(s.packagesDir(), e.Name()))
		}
	}
}

// handleRollout 为指定（或全部）设备设置更新目标：当前程序包，立即或定时。
func (s *Server) handleRollout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NotBefore time.Time `json:"not_before,omitempty"` // 零值 = 立即
		Devices   []string  `json:"devices,omitempty"`    // 空 = 全部设备
	}
	if !decodeJSON(w, r, 64<<10, &req) {
		return
	}
	if s.update(w, func(st *store.State) error {
		if st.Package == nil {
			return errBadRequest("还没有上传程序包")
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
			st.Devices[id].Update = &store.UpdateTarget{Version: st.Package.Version, NotBefore: req.NotBefore}
		}
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleCancelRollout 取消所有设备的待更新。
func (s *Server) handleCancelRollout(w http.ResponseWriter, r *http.Request) {
	if s.update(w, func(st *store.State) error {
		for _, d := range st.Devices {
			d.Update = nil
		}
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
