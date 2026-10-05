package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/render"
	"github.com/izzln/content-edge-display/internal/store"
)

// 模板底图：节日主题等区域画不出来的画面，垫在模板最下面（render.Render）。文件按内容命名放在
// data_dir/backgrounds/：换图就换名，清单缓存键自然跟着变；不再被任何模板引用的文件由 gcBackgrounds 清掉。

func (s *Server) backgroundsDir() string { return filepath.Join(s.cfg.DataDir, "backgrounds") }

// backgroundGrace：刚落盘、还没写进模板的底图不回收（上传与回收并发时）。
const backgroundGrace = 10 * time.Minute

// handleUploadBackground 接收 multipart file（PNG），设为模板的底图；?mirror=1 设为对调版底图。
// 底图压在媒体区上方，所以要检查它在媒体区里透明：完全不透明就拒收（视频会被整个盖住），透明不到一半照收但提示。
func (s *Server) handleUploadBackground(w http.ResponseWriter, r *http.Request) {
	id, mirror := r.PathValue("id"), r.URL.Query().Get("mirror") == "1"
	tpl, ok := s.store.Template(id)
	if !ok {
		http.Error(w, "unknown template", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes+1<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少底图文件", http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImageUploadBytes+1))
	if err != nil || len(data) > maxImageUploadBytes {
		http.Error(w, fmt.Sprintf("底图不能超过 %dMB", maxImageUploadBytes>>20), http.StatusBadRequest)
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	switch {
	case err != nil || format != "png":
		http.Error(w, "底图只支持 PNG（媒体区要透明，视频从透明处露出）", http.StatusBadRequest)
		return
	case cfg.Width*cfg.Height > maxImagePixels:
		http.Error(w, fmt.Sprintf("底图像素太多（%d×%d）", cfg.Width, cfg.Height), http.StatusBadRequest)
		return
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		http.Error(w, "底图文件已损坏："+err.Error(), http.StatusBadRequest)
		return
	}
	share := 1.0 // 媒体区里透明的比例
	if m, ok := tpl.MediaRegion(); ok {
		if mirror {
			m = store.Mirrored(m, tpl.W)
		}
		share = render.TransparentShare(render.CoverImage(img, tpl.W, tpl.H), image.Rect(m.X, m.Y, m.X+m.W, m.Y+m.H))
	}
	if share < 0.01 {
		http.Error(w, "底图在媒体区必须是透明的（PNG 透明通道），否则视频会被整个盖住——下载参考图看媒体区的位置", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(data)
	name := "bg-" + hex.EncodeToString(sum[:8]) + ".png"
	if err := fsutil.WriteFile(filepath.Join(s.backgroundsDir(), name), data, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.setBackground(w, id, mirror, name) {
		return
	}
	log.Printf("template %s: background%s set to %s (%d×%d)", id, map[bool]string{true: " (mirrored)"}[mirror], name, cfg.Width, cfg.Height)
	writeJSON(w, map[string]any{"file": name, "w": cfg.Width, "h": cfg.Height, "media_transparent": share})
}

// handleDeleteBackground 去掉模板的底图（?mirror=1 只去掉对调版）。去掉常规底图时对调版一并去掉。
func (s *Server) handleDeleteBackground(w http.ResponseWriter, r *http.Request) {
	if s.setBackground(w, r.PathValue("id"), r.URL.Query().Get("mirror") == "1", "") {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) setBackground(w http.ResponseWriter, id string, mirror bool, name string) bool {
	var found, noBase bool
	ok := s.update(w, func(st *store.State) {
		t, exists := st.Templates[id]
		if found = exists; !exists {
			return
		}
		switch {
		case !mirror:
			t.BackgroundImage = name
			if name == "" {
				t.BackgroundImageMirror = ""
			}
		case name != "" && t.BackgroundImage == "":
			noBase = true
			return
		default:
			t.BackgroundImageMirror = name
		}
		st.Templates[id] = t
	})
	switch {
	case !ok:
		return false
	case !found:
		http.Error(w, "unknown template", http.StatusNotFound)
		return false
	case noBase:
		http.Error(w, "请先上传常规底图，再上传对调版", http.StatusBadRequest)
		return false
	}
	s.gcBackgrounds()
	return true
}

// handleBackgroundGuide 生成底图设计参考图（?mirror=1 为对调版）：媒体区标红并写明位置，设计底图时这块留空。
func (s *Server) handleBackgroundGuide(w http.ResponseWriter, r *http.Request) {
	t, ok := s.store.Template(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown template", http.StatusNotFound)
		return
	}
	img, err := s.renderer.RenderGuide(t, r.URL.Query().Get("mirror") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	png.Encode(w, img)
}

// handleGetBackground 给后台看底图原图。
func (s *Server) handleGetBackground(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !store.IsBackgroundFile(name) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.backgroundsDir(), name))
}

// gcBackgrounds 删除不再被任何模板引用的底图文件。
func (s *Server) gcBackgrounds() {
	used := map[string]bool{}
	s.store.View(func(st *store.State) {
		for _, t := range st.Templates {
			used[t.BackgroundImage], used[t.BackgroundImageMirror] = true, true
		}
	})
	entries, _ := os.ReadDir(s.backgroundsDir())
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || used[e.Name()] || !store.IsBackgroundFile(e.Name()) || time.Since(info.ModTime()) < backgroundGrace {
			continue
		}
		if os.Remove(filepath.Join(s.backgroundsDir(), e.Name())) == nil {
			log.Printf("background %s removed (no template uses it)", e.Name())
		}
	}
}
