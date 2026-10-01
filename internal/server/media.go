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
	"github.com/izzln/content-edge-display/internal/transcode"
)

// 上传限制。
//
// 图片上传后在服务端缩到画布尺寸以内（transcode.ShrinkImage），设备永远只解 1440×900 的图，
// 所以原图可以直接用手机拍的；像素上限只是防止服务端被超大图（解压炸弹）拖垮。
// 视频限 500MB，一律转码成设备吃得消的 H.264（见 transcode 包）。
const (
	maxImageUploadBytes = 20 << 20
	maxImagePixels      = 50_000_000
	maxVideoUploadBytes = 500 << 20
)

// mediaNamePattern 限制上传文件名（后缀另行按类型校验）。
var mediaNamePattern = regexp.MustCompile(`^[\p{L}\p{N}_ .-]{1,128}$`)

// MediaFile 是设备播放列表里的一项。转码中/转码失败的视频也会列出来，
// 让运营方看到进度和失败原因；它们排在已就绪文件之后，不参与排序与播放。
type MediaFile struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // image | video
	Size     int64  `json:"size"`
	Status   string `json:"status"` // ready | queued | transcoding | failed
	Progress int    `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
}

const mediaReady = "ready"

// playlist 返回设备媒体区的播放顺序（只含已就绪、确实在磁盘上的文件）。
//
// 这是唯一的顺序来源，后台列表与设备清单都用它，保证"后台看到的"就是"设备在放的"：
// 先按 state 里记录的顺序（后台上传/拖拽排序写入），再把目录里有、记录里没有的文件
// （运营方直接拷进 media_root 的）按文件名追加在后面。
func (s *Server) playlist(deviceID string) ([]string, error) {
	onDisk, err := manifest.ListMedia(s.deviceMediaDir(deviceID))
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(onDisk))
	for _, n := range onDisk {
		present[n] = true
	}
	out := make([]string, 0, len(onDisk))
	for _, n := range append(s.store.Display(deviceID).Playlist, onDisk...) {
		if present[n] {
			out = append(out, n)
			delete(present, n) // 去重
		}
	}
	return out, nil
}

// mediaList 返回后台展示用的播放列表：就绪文件（顺序即播放顺序）+ 转码中/失败的任务。
func (s *Server) mediaList(deviceID string) ([]MediaFile, error) {
	names, err := s.playlist(deviceID)
	if err != nil {
		return nil, err
	}
	out := make([]MediaFile, 0, len(names))
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(s.deviceMediaDir(deviceID), name)); err == nil {
			out = append(out, MediaFile{Name: name, Type: manifest.TypeOf(name), Size: info.Size(), Status: mediaReady})
		}
	}
	for _, j := range s.jobs.snapshot(deviceID) {
		var size int64
		if info, err := os.Stat(j.src); err == nil {
			size = info.Size()
		}
		out = append(out, MediaFile{Name: j.name, Type: "video", Size: size,
			Status: j.status, Progress: j.progress, Error: j.err})
	}
	return out, nil
}

// setPlaylist 覆盖设备的播放顺序。
func (s *Server) setPlaylist(deviceID string, names []string) error {
	return s.store.Update(func(st *store.State) error {
		d := st.Displays[deviceID]
		d.Playlist = names
		st.Displays[deviceID] = d
		return nil
	})
}

// appendPlaylist 把文件按给定顺序追加到设备播放顺序末尾（上传完成、转码完成时用）。
// 调用时这些文件已经落盘，playlist() 会把它们当成"目录里有、记录里没有"的文件按名字排进去，
// 所以先剔掉再追加，才能保持上传顺序。
func (s *Server) appendPlaylist(deviceID string, names ...string) error {
	current, err := s.playlist(deviceID)
	if err != nil {
		return err
	}
	adding := make(map[string]bool, len(names))
	for _, n := range names {
		adding[n] = true
	}
	order := make([]string, 0, len(current)+len(names))
	for _, n := range current {
		if !adding[n] {
			order = append(order, n)
		}
	}
	return s.setPlaylist(deviceID, append(order, names...))
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
	accepted := []string{}    // 已就绪，直接进播放列表
	transcoding := []string{} // 已进入转码队列，完成后自动追加到播放列表末尾
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
		name, queued, reason := s.saveUploadedMedia(dev.ID, dir, part, taken)
		part.Close()
		if reason != "" {
			rejected = append(rejected, rejection{Name: filepath.Base(part.FileName()), Reason: reason})
			continue
		}
		taken[name] = true
		if queued {
			transcoding = append(transcoding, name)
		} else {
			accepted = append(accepted, name)
		}
	}

	if len(accepted) > 0 {
		if err := s.appendPlaylist(dev.ID, accepted...); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	files, err := s.mediaList(dev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"accepted": accepted, "transcoding": transcoding, "rejected": rejected, "playlist": files})
}

// saveUploadedMedia 把一个上传分片落盘并归一化。
// 返回最终文件名、是否进入了转码队列，或面向使用者的拒收原因。
//
//   - 图片：校验能解码，过大的缩到画布尺寸以内，立即就绪；
//   - 视频：原片放进暂存区排队转码，产物名统一为 .mp4。没有 ffmpeg 就不收视频——
//     未转码的原片码率过高，会让设备过热关机，宁可当场拒绝。
func (s *Server) saveUploadedMedia(deviceID, dir string, part *multipart.Part, taken map[string]bool) (string, bool, string) {
	name := filepath.Base(part.FileName())
	if !mediaNamePattern.MatchString(name) {
		return "", false, "文件名非法（仅支持字母、数字、空格和 . _ -，最长 128 字符）"
	}
	isVideo := false
	switch manifest.TypeOf(name) {
	case "image":
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			return "", false, "图片仅支持 png/jpg"
		}
	case "video":
		if s.encoder == nil {
			return "", false, "服务端未安装 ffmpeg，暂不能上传视频（未转码的视频会让设备过热）"
		}
		isVideo, name = true, transcode.OutputName(name)
	default:
		return "", false, "不支持的文件类型（图片 png/jpg，视频 mp4/mov/mkv/webm 等）"
	}
	name = uniqueName(name, taken)

	limit, stageDir := int64(maxImageUploadBytes), dir
	if isVideo {
		limit, stageDir = maxVideoUploadBytes, filepath.Join(s.incomingDir(), deviceID)
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return "", false, err.Error()
		}
	}
	tmp := filepath.Join(stageDir, "."+name+".part")
	out, err := os.Create(tmp)
	if err != nil {
		return "", false, err.Error()
	}
	// 多读 1 字节：读得出来说明超限了。
	n, err := io.Copy(out, io.LimitReader(part, limit+1))
	out.Close()
	fail := func(reason string) (string, bool, string) {
		os.Remove(tmp)
		return "", false, reason
	}
	switch {
	case err != nil:
		return fail("写入失败：" + err.Error())
	case n > limit:
		return fail(fmt.Sprintf("文件超过上限 %dMB", limit>>20))
	}

	if isVideo {
		// 编码不用查：转码会统一成 H.264。是不是真视频交给 ffmpeg 判断，失败会显示在列表里。
		s.jobs.add(&transcodeJob{deviceID: deviceID, name: name, src: tmp, status: jobQueued})
		return name, true, ""
	}
	if reason := checkImage(tmp); reason != "" {
		return fail(reason)
	}
	// 文件头能读不代表整张图完好（截断的文件过得了 DecodeConfig），完整解码在这一步才发生
	if _, err := transcode.ShrinkImage(tmp, canvasW, canvasH); err != nil {
		return fail("图片无法解码（文件损坏或不完整）")
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fail(err.Error())
	}
	return name, false, ""
}

// checkImage 在完整解码之前先读文件头：挡住伪装成图片的文件与超大图（解压炸弹）。
func checkImage(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "图片无法解码（文件损坏或不是真正的 png/jpg）"
	}
	if cfg.Width*cfg.Height > maxImagePixels {
		return fmt.Sprintf("图片 %d×%d 像素过大（上限约 %d 万像素），请先缩小再上传",
			cfg.Width, cfg.Height, maxImagePixels/10000)
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
	s.jobs.remove(dev.ID, name) // 转码中的会被取消，失败的直接移除
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
