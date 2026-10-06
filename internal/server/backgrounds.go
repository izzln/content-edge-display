package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/render"
	"github.com/izzln/content-edge-display/internal/store"
)

// 模板底图：节日主题等区域画不出来的画面，压在媒体区上方（render.Render）。文件按内容命名放在
// data_dir/backgrounds/（store.BackgroundFile）：换图就换名，清单缓存键自然跟着变；不再被任何模板引用的文件
// 由每小时（或模板变动后）的维护清掉。底图字段只由这里的接口改，模板的编辑接口保留原值。

// backgroundGrace：刚落盘、还没写进模板的底图不回收（上传与回收并发时）。
const backgroundGrace = 10 * time.Minute

// handleUploadBackground 接收 multipart file（PNG），设为模板的底图；?mirror=1 设为对调版底图。
// 底图压在媒体区上方，所以要检查它在媒体区里透明：完全不透明就拒收（视频会被整个盖住），透明不到一半照收但提示。
// 比画布大一倍以上的先缩小再存：渲染时不必每次解码一张几千万像素的原图。
func (s *Server) handleUploadBackground(w http.ResponseWriter, r *http.Request) {
	id, mirror := r.PathValue("id"), r.URL.Query().Get("mirror") == "1"
	tpl, ok := s.store.Template(id)
	if !ok {
		writeError(w, errNotFound("模板"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes+1<<20)
	up, err := s.receiveUpload(r, maxImageUploadBytes)
	if err != nil {
		writeError(w, uploadError(err, "底图", maxImageUploadBytes))
		return
	}
	defer os.Remove(up.path)
	if cfg, format, reason := checkImage(up.path); reason != nil || format != "png" {
		http.Error(w, "底图只支持 PNG（媒体区要透明，视频从透明处露出）", http.StatusBadRequest)
		return
	} else if cfg.Width > 2*tpl.W || cfg.Height > 2*tpl.H {
		up.sha = "" // 要缩小：存缩小后的
	}
	img, err := decodePNG(up.path)
	if err != nil {
		http.Error(w, "底图文件已损坏："+err.Error(), http.StatusBadRequest)
		return
	}
	share := 1.0 // 媒体区里透明的比例
	if m, ok := tpl.MediaRect(mirror); ok {
		share = render.TransparentShare(render.CoverImage(img, tpl.W, tpl.H), m)
	}
	if share < 0.01 {
		http.Error(w, "底图在媒体区必须是透明的（PNG 透明通道），否则视频会被整个盖住——下载参考图看媒体区的位置", http.StatusBadRequest)
		return
	}
	name, err := s.saveBackground(up, img, tpl)
	if err != nil {
		writeError(w, err)
		return
	}
	if !s.setBackground(w, id, mirror, name) {
		return
	}
	b := img.Bounds()
	log.Printf("template %s: background%s set to %s (%d×%d uploaded)", id, mirrorSuffix(mirror), name, b.Dx(), b.Dy())
	writeJSON(w, map[string]any{"media_transparent": share})
}

// saveBackground 把上传的底图存进底图目录，返回文件名。up.sha 为空表示要先缩小到画布的两倍（铺满后裁切）。
func (s *Server) saveBackground(up uploadedFile, img image.Image, tpl store.Template) (string, error) {
	if up.sha != "" {
		name := store.BackgroundFile(up.sha)
		return name, fsutil.MoveFile(up.path, filepath.Join(s.backgroundsDir(), name))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, render.CoverImage(img, 2*tpl.W, 2*tpl.H)); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	name := store.BackgroundFile(hex.EncodeToString(sum[:]))
	return name, fsutil.WriteFile(filepath.Join(s.backgroundsDir(), name), buf.Bytes(), 0o644)
}

func decodePNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

func mirrorSuffix(mirror bool) string {
	if mirror {
		return " (mirrored)"
	}
	return ""
}

// handleDeleteBackground 去掉模板的底图（?mirror=1 只去掉对调版）。去掉常规底图时对调版一并去掉。
func (s *Server) handleDeleteBackground(w http.ResponseWriter, r *http.Request) {
	if s.setBackground(w, r.PathValue("id"), r.URL.Query().Get("mirror") == "1", "") {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) setBackground(w http.ResponseWriter, id string, mirror bool, name string) bool {
	ok := s.update(w, func(st *store.State) error {
		t, ok := st.Templates[id]
		switch {
		case !ok:
			return errNotFound("模板")
		case !mirror:
			t.BackgroundImage = name
			if name == "" {
				t.BackgroundImageMirror = ""
			}
		case name != "" && t.BackgroundImage == "":
			return errBadRequest("请先上传常规底图，再上传对调版")
		default:
			t.BackgroundImageMirror = name
		}
		st.Templates[id] = t
		return nil
	})
	if ok {
		s.kickCache() // 维护协程回收不再用到的底图
	}
	return ok
}

// handleBackgroundGuide 生成底图设计参考图（?mirror=1 为对调版）：媒体区标红并写明位置，设计底图时这块留空。
func (s *Server) handleBackgroundGuide(w http.ResponseWriter, r *http.Request) {
	t, ok := s.store.Template(r.PathValue("id"))
	if !ok {
		writeError(w, errNotFound("模板"))
		return
	}
	img, err := s.renderer.RenderGuide(t, r.URL.Query().Get("mirror") == "1")
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	png.Encode(w, img)
}

// handleGetBackground 给后台看底图原图。文件按内容命名、内容永不改变，可以长期缓存。
func (s *Server) handleGetBackground(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !store.IsBackgroundFile(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
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
	for _, name := range pruneDir(s.backgroundsDir(), func(name string, info os.FileInfo) bool {
		return used[name] || !store.IsBackgroundFile(name) || time.Since(info.ModTime()) < backgroundGrace
	}) {
		log.Printf("background %s removed (no template uses it)", name)
	}
}
