package server

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig 需要
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

// 上传限制。
//
// 图片同时限体积和像素：屏幕只有 1440×900，而设备只有 1GB 内存——
// 一张 12 兆像素的图解码成 RGBA 就要占 48MB，再大就有把播放器挤爆的风险。
// 视频限 500MB，且只收 H.264（见 mp4.go）。
const (
	maxImageUploadBytes = 20 << 20
	maxImagePixels      = 12_000_000
	maxVideoUploadBytes = 500 << 20
)

// mediaNamePattern 限制上传文件名（后缀另行按类型校验）。
var mediaNamePattern = regexp.MustCompile(`^[\p{L}\p{N}_ .-]{1,128}$`)

// MediaFile 是设备播放列表里的一项。
type MediaFile struct {
	Name string `json:"name"`
	Type string `json:"type"` // image | video
	Size int64  `json:"size"`
}

// mediaList 返回设备当前生效的播放列表（顺序即播放顺序）。
// 以 state 里记录的顺序为准；顺序缺失时（例如文件是直接拷进目录的）按文件名排。
func (s *Server) mediaList(deviceID string) ([]MediaFile, error) {
	dir := s.deviceMediaDir(deviceID)
	onDisk, err := manifest.ListMedia(dir)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, n := range onDisk {
		present[n] = true
	}
	out := []MediaFile{}
	add := func(name string) {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return
		}
		out = append(out, MediaFile{Name: name, Type: manifest.TypeOf(name), Size: info.Size()})
	}
	seen := map[string]bool{}
	for _, name := range s.store.Display(deviceID).Playlist {
		if present[name] && !seen[name] {
			seen[name] = true
			add(name)
		}
	}
	for _, name := range onDisk {
		if !seen[name] {
			add(name)
		}
	}
	return out, nil
}

// setPlaylist 覆盖设备的播放顺序。
func (s *Server) setPlaylist(deviceID string, names []string) error {
	return s.store.Update(func(st *store.State) error {
		d := st.Displays[deviceID]
		if d.Mode == "" {
			d.Mode = store.ModeGlobal
		}
		d.Playlist = names
		st.Displays[deviceID] = d
		return nil
	})
}

func (s *Server) handleListDeviceMedia(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	files, err := s.mediaList(dev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, files)
}

// handleUploadDeviceMedia 接收多文件上传（字段名 file 或 files），逐个校验后追加到播放列表末尾。
// 逐个文件流式落盘，不把 500MB 的视频整进内存。
func (s *Server) handleUploadDeviceMedia(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad multipart body", http.StatusBadRequest)
		return
	}
	dir := s.deviceMediaDir(dev.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type rejection struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	accepted := []string{}
	rejected := []rejection{}
	existing, err := s.mediaList(dev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	taken := map[string]bool{}
	for _, f := range existing {
		taken[f.Name] = true
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad multipart body", http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" && part.FormName() != "files" {
			part.Close()
			continue
		}
		name, reason := s.saveUploadedMedia(dir, part, taken)
		part.Close()
		if reason != "" {
			rejected = append(rejected, rejection{Name: filepath.Base(part.FileName()), Reason: reason})
			continue
		}
		taken[name] = true
		accepted = append(accepted, name)
	}

	if len(accepted) > 0 {
		order := make([]string, 0, len(existing)+len(accepted))
		for _, f := range existing {
			order = append(order, f.Name)
		}
		order = append(order, accepted...)
		if err := s.setPlaylist(dev.ID, order); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	files, err := s.mediaList(dev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"accepted": accepted, "rejected": rejected, "playlist": files})
}

// saveUploadedMedia 把一个上传分片落盘并校验；返回最终文件名，或面向使用者的拒收原因。
func (s *Server) saveUploadedMedia(dir string, part *multipart.Part, taken map[string]bool) (string, string) {
	name := filepath.Base(part.FileName())
	if !mediaNamePattern.MatchString(name) {
		return "", "文件名非法（仅支持字母、数字、空格和 . _ -，最长 128 字符）"
	}
	kind := manifest.TypeOf(name)
	limit := int64(maxImageUploadBytes)
	switch kind {
	case "image":
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			return "", "图片仅支持 png/jpg"
		}
	case "video":
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".mp4" && ext != ".mov" {
			return "", "视频仅支持 mp4/mov 容器，且编码须为 H.264"
		}
		limit = maxVideoUploadBytes
	default:
		return "", "不支持的文件类型（图片 png/jpg，视频 mp4/mov）"
	}

	name = uniqueName(name, taken)
	tmp := filepath.Join(dir, "."+name+".part")
	out, err := os.Create(tmp)
	if err != nil {
		return "", err.Error()
	}
	// 多读 1 字节：读得出来说明超限了。
	n, err := io.Copy(out, io.LimitReader(part, limit+1))
	out.Close()
	cleanup := func() { os.Remove(tmp) }
	if err != nil {
		cleanup()
		return "", "写入失败：" + err.Error()
	}
	if n > limit {
		cleanup()
		return "", fmt.Sprintf("文件超过上限 %dMB", limit>>20)
	}
	if reason := validateMediaFile(tmp, kind); reason != "" {
		cleanup()
		return "", reason
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		cleanup()
		return "", err.Error()
	}
	return name, ""
}

// validateMediaFile 校验已落盘文件的内容：图片限像素，视频限编码。
func validateMediaFile(path, kind string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	if kind == "image" {
		cfg, _, err := image.DecodeConfig(f)
		if err != nil {
			return "图片无法解码（文件损坏或不是真正的 png/jpg）"
		}
		if cfg.Width*cfg.Height > maxImagePixels {
			return fmt.Sprintf("图片 %d×%d 像素过大（上限约 %d 万像素）；屏幕只有 %d×%d，"+
				"请先缩小再上传，否则设备解码会占用过多内存",
				cfg.Width, cfg.Height, maxImagePixels/10000, canvasW, canvasH)
		}
		return ""
	}
	info, err := f.Stat()
	if err != nil {
		return err.Error()
	}
	if err := checkVideoPlayable(f, info.Size()); err != nil {
		return err.Error()
	}
	return ""
}

// uniqueName 在同名文件已存在时加后缀，保留原名便于运营方辨认。
func uniqueName(name string, taken map[string]bool) string {
	if !taken[name] {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if !taken[candidate] {
			return candidate
		}
	}
}

// handleReorderDeviceMedia 整表替换播放顺序（后台拖拽排序后提交）。
func (s *Server) handleReorderDeviceMedia(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var names []string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&names); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	onDisk, err := manifest.ListMedia(s.deviceMediaDir(dev.ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	present := map[string]bool{}
	for _, n := range onDisk {
		present[n] = true
	}
	seen := map[string]bool{}
	order := make([]string, 0, len(names))
	for _, n := range names {
		if !present[n] {
			http.Error(w, fmt.Sprintf("文件 %q 不存在", n), http.StatusBadRequest)
			return
		}
		if seen[n] {
			http.Error(w, fmt.Sprintf("文件 %q 重复", n), http.StatusBadRequest)
			return
		}
		seen[n] = true
		order = append(order, n)
	}
	if err := s.setPlaylist(dev.ID, order); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	files, err := s.mediaList(dev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, files)
}

// handleDeleteDeviceMedia 删除一个媒体文件及其播放列表项。
func (s *Server) handleDeleteDeviceMedia(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	name := r.PathValue("file")
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		http.Error(w, "bad file name", http.StatusBadRequest)
		return
	}
	if err := os.Remove(filepath.Join(s.deviceMediaDir(dev.ID), name)); err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kept := []string{}
	for _, n := range s.store.Display(dev.ID).Playlist {
		if n != name {
			kept = append(kept, n)
		}
	}
	if err := s.setPlaylist(dev.ID, kept); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	files, err := s.mediaList(dev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, files)
}
