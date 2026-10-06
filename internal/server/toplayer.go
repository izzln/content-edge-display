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

// 模板叠图：节日装饰、相框等区域画不出来的画面，整张盖在模板最上面——区域底色与文字之上，
// 媒体区里它透明的地方露出视频（render.Render）。文件按内容命名放在 data_dir/top-layers/（store.TopLayerFile）：
// 换图就换名，清单缓存键自然跟着变；不再被任何模板引用的文件由每小时（或模板变动后）的维护清掉。
// 叠图字段只由这里的接口改，模板的编辑接口保留原值。

// topLayerGrace：刚落盘、还没写进模板的叠图不回收（上传与回收并发时）。
const topLayerGrace = 10 * time.Minute

// handleUploadTopLayer 接收 multipart file（只收 PNG：要用透明通道），设为模板的叠图；?mirror=1 设为对调版叠图。
// 叠图盖在最上面，所以要检查它在媒体区里透明：完全不透明就拒收（视频会被整个盖住），透明不到一半照收但提示；
// 属性/文字区域大半不透明的也照收，但在回复里列出来（text_covered），后台提示文字可能被盖住。
// 比画布大一倍以上的先缩小再存：渲染时不必每次解码一张几千万像素的原图。
func (s *Server) handleUploadTopLayer(w http.ResponseWriter, r *http.Request) {
	id, mirror := r.PathValue("id"), r.URL.Query().Get("mirror") == "1"
	tpl, ok := s.store.Template(id)
	if !ok {
		writeError(w, errNotFound("模板"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes+1<<20)
	up, err := s.receiveUpload(r, maxImageUploadBytes)
	if err != nil {
		writeError(w, uploadError(err, "叠图", maxImageUploadBytes))
		return
	}
	defer os.Remove(up.path)
	if cfg, format, reason := checkImage(up.path); reason != nil || format != "png" {
		http.Error(w, "叠图只支持 PNG（要用透明通道：媒体区与文字处透明）", http.StatusBadRequest)
		return
	} else if cfg.Width > 2*tpl.W || cfg.Height > 2*tpl.H {
		up.sha = "" // 要缩小：存缩小后的
	}
	img, err := decodePNG(up.path)
	if err != nil {
		http.Error(w, "叠图文件已损坏："+err.Error(), http.StatusBadRequest)
		return
	}
	onCanvas := render.CoverImage(img, tpl.W, tpl.H)
	share := 1.0 // 媒体区里透明的比例
	if m, ok := tpl.MediaRect(mirror); ok {
		share = render.TransparentShare(onCanvas, m)
	}
	if share < 0.01 {
		http.Error(w, "叠图在媒体区必须是透明的（PNG 透明通道），否则视频会被整个盖住——下载参考图看媒体区的位置", http.StatusBadRequest)
		return
	}
	covered := []string{} // 大半被叠图盖住的属性/文字区域
	for _, reg := range tpl.Regions {
		if mirror {
			reg = store.Mirrored(reg, tpl.W)
		}
		if reg.Type == store.RegionMedia || render.TransparentShare(onCanvas, reg.Rect()) >= 0.5 {
			continue
		}
		if reg.Type == store.RegionAttribute {
			covered = append(covered, "属性 "+reg.Key)
		} else {
			covered = append(covered, "文字「"+reg.Key+"」")
		}
	}
	name, err := s.saveTopLayer(up, img, tpl)
	if err != nil {
		writeError(w, err)
		return
	}
	if !s.setTopLayer(w, id, mirror, name) {
		return
	}
	b := img.Bounds()
	log.Printf("template %s: top layer%s set to %s (%d×%d uploaded)", id, mirrorSuffix(mirror), name, b.Dx(), b.Dy())
	writeJSON(w, map[string]any{"media_transparent": share, "text_covered": covered})
}

// saveTopLayer 把上传的叠图存进叠图目录，返回文件名。up.sha 为空表示要先缩小到画布的两倍（铺满后裁切）。
func (s *Server) saveTopLayer(up uploadedFile, img image.Image, tpl store.Template) (string, error) {
	if up.sha != "" {
		name := store.TopLayerFile(up.sha)
		return name, fsutil.MoveFile(up.path, filepath.Join(s.topLayersDir(), name))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, render.CoverImage(img, 2*tpl.W, 2*tpl.H)); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	name := store.TopLayerFile(hex.EncodeToString(sum[:]))
	return name, fsutil.WriteFile(filepath.Join(s.topLayersDir(), name), buf.Bytes(), 0o644)
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

// handleDeleteTopLayer 去掉模板的叠图（?mirror=1 只去掉对调版）。去掉常规叠图时对调版一并去掉。
func (s *Server) handleDeleteTopLayer(w http.ResponseWriter, r *http.Request) {
	if s.setTopLayer(w, r.PathValue("id"), r.URL.Query().Get("mirror") == "1", "") {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) setTopLayer(w http.ResponseWriter, id string, mirror bool, name string) bool {
	ok := s.update(w, func(st *store.State) error {
		t, ok := st.Templates[id]
		switch {
		case !ok:
			return errNotFound("模板")
		case !mirror:
			t.TopLayer = name
			if name == "" {
				t.TopLayerMirror = ""
			}
		case name != "" && t.TopLayer == "":
			return errBadRequest("请先上传常规叠图，再上传对调版")
		default:
			t.TopLayerMirror = name
		}
		st.Templates[id] = t
		return nil
	})
	if ok {
		s.kickCache() // 维护协程回收不再用到的叠图
	}
	return ok
}

// handleTopLayerGuide 生成叠图设计参考图（?mirror=1 为对调版）：媒体区标红并写明位置，设计叠图时这块留空。
func (s *Server) handleTopLayerGuide(w http.ResponseWriter, r *http.Request) {
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

// handleGetTopLayer 给后台看叠图原图。文件按内容命名、内容永不改变，可以长期缓存。
func (s *Server) handleGetTopLayer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !store.IsTopLayerFile(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, filepath.Join(s.topLayersDir(), name))
}

// gcTopLayers 删除不再被任何模板引用的叠图文件。
func (s *Server) gcTopLayers() {
	used := map[string]bool{}
	s.store.View(func(st *store.State) {
		for _, t := range st.Templates {
			used[t.TopLayer], used[t.TopLayerMirror] = true, true
		}
	})
	for _, name := range pruneDir(s.topLayersDir(), func(name string, info os.FileInfo) bool {
		return used[name] || !store.IsTopLayerFile(name) || time.Since(info.ModTime()) < topLayerGrace
	}) {
		log.Printf("top layer %s removed (no template uses it)", name)
	}
}
