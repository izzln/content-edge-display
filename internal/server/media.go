package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig 需要
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
)

// 上传限制（后台经 /info 拿到同样的数字做预检）。
//
// 图片上传后在服务端缩到画布尺寸以内（transcode.ShrinkImage），设备永远只解画布大小的图，
// 所以原图可以直接用手机拍的；像素上限只是防止服务端被超大图（解压炸弹）拖垮。
// 视频一律转码成设备吃得消的 H.264（见 transcode 包）。PDF 页数多了渲染慢、清单长，轮播一圈也没人看得完。
const (
	maxImageUploadBytes = 20 << 20
	maxImagePixels      = 50_000_000
	maxVideoUploadBytes = 500 << 20
	maxPDFUploadBytes   = 50 << 20
	maxPDFPages         = 100
	maxMediaStem        = 100 // 上传文件主名（不含后缀）的最大字符数
)

// MediaFile 是设备播放列表里的一项。转码中/转码失败的视频也会列出来，
// 让运营方看到进度和失败原因；它们排在已就绪文件之后，不参与排序与播放。
type MediaFile struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // image | video | document
	Size     int64  `json:"size"`
	Status   string `json:"status"` // ready | queued | transcoding | failed
	Progress int    `json:"progress,omitempty"`
	Pages    int    `json:"pages,omitempty"` // 文档的页数（渲染中为已知的总页数）
	Error    string `json:"error,omitempty"`
}

const mediaReady = "ready"

// playlist 返回设备媒体区的播放顺序（只含已就绪、确实在磁盘上的文件）。
//
// 这是唯一的顺序来源，后台列表与设备清单都用它，保证"后台看到的"就是"设备在放的"。
func (s *Server) playlist(deviceID string) ([]string, error) {
	var stored []string
	s.store.View(func(st *store.State) { stored = st.Displays[deviceID].Playlist })
	return orderPlaylist(s.deviceMediaDir(deviceID), stored)
}

// orderPlaylist 先按记录的顺序（后台上传/拖拽排序写入），再把目录里有、记录里没有的文件
// （运营方直接拷进 media_root 的）按文件名追加在后面；记录里有、磁盘上没有的跳过。
func orderPlaylist(dir string, stored []string) ([]string, error) {
	onDisk, err := manifest.ListMedia(dir)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(onDisk))
	for _, n := range onDisk {
		present[n] = true
	}
	out := make([]string, 0, len(onDisk))
	for _, n := range append(slices.Clone(stored), onDisk...) {
		if present[n] {
			out = append(out, n)
			delete(present, n) // 去重
		}
	}
	return out, nil
}

// mediaList 返回后台展示用的播放列表：就绪文件（顺序即播放顺序）+ 处理中/失败的任务。
func (s *Server) mediaList(deviceID string) ([]MediaFile, error) {
	names, err := s.playlist(deviceID)
	if err != nil {
		return nil, err
	}
	dir := s.deviceMediaDir(deviceID)
	out := make([]MediaFile, 0, len(names))
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			f := MediaFile{Name: name, Type: manifest.TypeOf(name), Size: info.Size(), Status: mediaReady}
			if f.Type == "document" {
				f.Pages = len(manifest.Pages(dir, name))
			}
			out = append(out, f)
		}
	}
	for _, j := range s.jobs.snapshot(deviceID) {
		var size int64
		if info, err := os.Stat(j.src); err == nil {
			size = info.Size()
		}
		out = append(out, MediaFile{Name: j.name, Type: j.kind, Size: size,
			Status: j.status, Progress: j.progress, Pages: j.pages, Error: j.err})
	}
	return out, nil
}

// writeMediaList 回复设备当前的播放列表（各个修改播放列表的接口都以它结尾）。
func (s *Server) writeMediaList(w http.ResponseWriter, deviceID string) {
	files, err := s.mediaList(deviceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, files)
}

// editPlaylist 在一次写锁内修改设备的播放顺序：fn 拿到当前有效顺序（见 orderPlaylist），返回新顺序。
// 读和写在同一把锁里，上传完成与转码完成同时追加也不会互相覆盖。
func (s *Server) editPlaylist(deviceID string, fn func(current []string) []string) error {
	dir := s.deviceMediaDir(deviceID)
	var err error
	if uerr := s.store.Update(func(st *store.State) {
		d := st.Displays[deviceID]
		var current []string
		if current, err = orderPlaylist(dir, d.Playlist); err == nil {
			d.Playlist = fn(current)
			st.Displays[deviceID] = d
		}
	}); uerr != nil {
		return uerr
	}
	return err
}

// appendPlaylist 把文件按给定顺序追加到设备播放顺序末尾（上传完成、转码完成时用）。
// 这些文件已经落盘，orderPlaylist 会把它们当成"目录里有、记录里没有"的文件按名字排进去，
// 所以先剔掉再追加，才能保持上传顺序。
func (s *Server) appendPlaylist(deviceID string, names ...string) error {
	return s.editPlaylist(deviceID, func(current []string) []string {
		return append(slices.DeleteFunc(current, func(n string) bool { return slices.Contains(names, n) }), names...)
	})
}

// unlinkMedia 从设备目录删掉这些文件（PDF 连同逐页图片）。内容本身留在缓存区里，先记下它们"刚刚还在用"，
// 淘汰顺序才准；之后同一文件再上传可直接复用。
func (s *Server) unlinkMedia(deviceID string, names ...string) {
	dir := s.deviceMediaDir(deviceID)
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(dir, n)
	}
	s.cacheTouch(paths...)
	for i, n := range names {
		os.Remove(paths[i])
		os.RemoveAll(manifest.PagesPath(dir, n))
	}
	s.kickCache()
}

func (s *Server) handleListDeviceMedia(w http.ResponseWriter, r *http.Request) {
	if dev, ok := s.pathDevice(w, r); ok {
		s.writeMediaList(w, dev.ID)
	}
}

// handleUploadDeviceMedia 接收多文件上传（字段名 files），逐个校验后追加到播放列表末尾。
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
	if err := os.MkdirAll(s.deviceMediaDir(dev.ID), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type rejection struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	accepted := []string{}    // 已就绪，直接进播放列表
	reused := []string{}      // 其中复用了缓存区里已处理好的结果的
	transcoding := []string{} // 已进入转码队列，完成后自动追加到播放列表末尾
	rejected := []rejection{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad multipart body", http.StatusBadRequest)
			return
		}
		if part.FormName() != "files" {
			part.Close()
			continue
		}
		orig := filepath.Base(part.FileName())
		log.Printf("upload started: device %s, %s (%s)", dev.ID, orig, uploadSizeHint(r))
		start := time.Now()
		cr := &countingReader{r: part}
		name, outcome, reason := s.saveUploadedMedia(dev.ID, cr, orig)
		part.Close()
		took := time.Since(start).Round(100 * time.Millisecond)
		if reason != nil {
			log.Printf("upload rejected: device %s, %s (%s received in %s): %s", dev.ID, orig, humanBytes(cr.n), took, reason.log)
			rejected = append(rejected, rejection{Name: orig, Reason: reason.ui})
			continue
		}
		switch outcome {
		case uploadQueued:
			log.Printf("upload done: device %s, %s (%s in %s), queued for processing as %s", dev.ID, orig, humanBytes(cr.n), took, name)
			transcoding = append(transcoding, name)
		case uploadReused:
			log.Printf("upload done: device %s, %s (%s in %s), reused cached result, added to playlist", dev.ID, name, humanBytes(cr.n), took)
			accepted, reused = append(accepted, name), append(reused, name)
		default:
			log.Printf("upload done: device %s, %s (%s in %s), added to playlist", dev.ID, name, humanBytes(cr.n), took)
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
	s.kickCache()
	writeJSON(w, map[string]any{"accepted": accepted, "reused": reused, "transcoding": transcoding, "rejected": rejected, "playlist": files})
}

// rejectReason 是拒收原因：ui 显示在后台（中文，告诉运营方怎么办），log 写进控制台（英文）。
type rejectReason struct{ ui, log string }

func reject(ui, log string) *rejectReason { return &rejectReason{ui: ui, log: log} }

// rejectErr 是没有专门中文说明的拒收原因（如磁盘写入失败）：原样显示。
func rejectErr(err error) *rejectReason { return reject(err.Error(), err.Error()) }

// uploadOutcome 是一个上传文件的去向。
type uploadOutcome int

const (
	uploadReady  uploadOutcome = iota // 已就绪（图片）
	uploadQueued                      // 进入后台处理（视频转码、PDF 渲染）
	uploadReused                      // 同一原片处理过：直接复用缓存区里的结果，立即就绪
)

// saveUploadedMedia 把一个上传分片落盘并归一化，返回最终文件名与去向，或拒收原因。
//
//   - 图片：校验能解码，过大的缩到画布尺寸以内，立即就绪；
//   - 视频：原片放进暂存区排队转码，产物名统一为 .mp4。没有 ffmpeg 就不收视频——
//     未转码的原片码率过高，会让设备过热关机，宁可当场拒绝。
//   - PDF：放进暂存区排队逐页渲染成图片（见 renderPDF）。没有 poppler-utils 就不收。
//
// 落盘时顺带算原片的 sha256：同一原片按同样参数处理过、结果还在缓存区里，就直接复用（见 cache.go）。
// 设备在用的文件已占满缓存区配额时拒收。
func (s *Server) saveUploadedMedia(deviceID string, part io.Reader, name string) (string, uploadOutcome, *rejectReason) {
	name = cleanMediaName(name)
	kind := manifest.TypeOf(name)
	switch kind {
	case "image":
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			return "", 0, reject("图片仅支持 png/jpg", "unsupported image format")
		}
	case "video":
		if s.tools.enc == nil {
			return "", 0, reject("服务端 ffmpeg 不可用，暂不能上传视频（未转码的视频会让设备过热）；原因见后台顶部提示",
				"ffmpeg unavailable, videos are not accepted")
		}
		name = transcode.OutputName(name)
	case "document":
		if s.tools.pdf == nil {
			return "", 0, reject("服务端没装 poppler-utils，暂不能上传 PDF；原因见后台顶部提示",
				"poppler-utils unavailable, PDFs are not accepted")
		}
	default:
		return "", 0, reject("不支持的文件类型（图片 png/jpg，视频 mp4/mov/mkv/webm 等，PDF）", "unsupported file type")
	}
	if st := s.cacheStats(); st.InUseBytes >= s.quotaBytes() {
		return "", 0, reject(fmt.Sprintf("缓存区已被设备在用的文件占满（%s / %dGB），请在「存储」页调大缓存区，或先删除不再使用的内容",
			humanBytes(st.InUseBytes), st.QuotaGB), "cache quota is full of in-use files")
	}
	name, release, err := s.claimMediaName(deviceID, name)
	if err != nil {
		return "", 0, rejectErr(err)
	}
	defer release()

	// 图片直接落在设备目录（处理很快）；视频与 PDF 放进暂存区排队处理
	dir, stageDir := s.deviceMediaDir(deviceID), s.deviceMediaDir(deviceID)
	limit := map[string]int64{"image": maxImageUploadBytes, "video": maxVideoUploadBytes, "document": maxPDFUploadBytes}[kind]
	if kind != "image" {
		stageDir = filepath.Join(s.incomingDir(), deviceID)
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return "", 0, rejectErr(err)
		}
	}
	tmp := filepath.Join(stageDir, "."+name+".part")
	out, err := os.Create(tmp)
	if err != nil {
		return "", 0, rejectErr(err)
	}
	// 落盘时顺带算原片的 sha256（缓存区复用的依据）；多读 1 字节：读得出来说明超限了。
	h := sha256.New()
	n, err := io.Copy(out, io.TeeReader(io.LimitReader(part, limit+1), h))
	out.Close()
	fail := func(reason *rejectReason) (string, uploadOutcome, *rejectReason) {
		os.Remove(tmp)
		return "", 0, reason
	}
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		// 浏览器没把文件传完就断了：多半是浏览器读不到文件了（iOS 相册临时文件被删、网盘文件没下载），
		// 或者网络断了。后台会自动重传一次。
		return fail(reject("上传中断：浏览器没把文件传完", "client stopped sending mid-upload (unexpected EOF)"))
	case err != nil:
		return fail(reject("写入失败："+err.Error(), "write failed: "+err.Error()))
	case n > limit:
		return fail(reject(fmt.Sprintf("文件超过上限 %dMB", limit>>20), fmt.Sprintf("exceeds the %dMB limit", limit>>20)))
	}

	key := processKey(kind, hex.EncodeToString(h.Sum(nil)))
	if reused, err := s.cacheReuse(dir, name, key); err != nil {
		log.Printf("cache: reuse failed, processing again: %v", err)
	} else if reused {
		os.Remove(tmp)
		return name, uploadReused, nil
	}
	if kind != "image" {
		// 视频：编码不用查，转码会统一成 H.264；是不是真视频交给 ffmpeg 判断。
		// PDF：页数、加密与否交给 pdfinfo 判断。失败都会显示在列表里。
		s.jobs.add(&mediaJob{kind: kind, deviceID: deviceID, name: name, src: tmp, key: key, status: jobQueued})
		return name, uploadQueued, nil
	}
	if reason := checkImage(tmp); reason != nil {
		return fail(reason)
	}
	// 文件头能读不代表整张图完好（截断的文件过得了 DecodeConfig），完整解码在这一步才发生
	if err := transcode.ShrinkImage(tmp, manifest.CanvasW, manifest.CanvasH); err != nil {
		return fail(reject("图片无法解码（文件损坏或不完整）", "image cannot be decoded"))
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fail(rejectErr(err))
	}
	if _, err := s.cacheAdopt(dir, name, key); err != nil {
		log.Printf("cache: cannot add %s to the cache: %v", name, err)
	}
	return name, uploadReady, nil
}

// cleanMediaName 把浏览器给的原文件名整理成能安全落盘、放进 URL 的名字——整理而不是拒收：
// 截图、网页下载的文件名里常有 | : / 和各种特殊空格（macOS 截图在 "PM" 前用的是窄不换行空格）。
// 字母（含中文）、数字、组合附标和 _ . - 原样保留；各种空白变成普通空格并合并；其余字符换成 _；
// 去掉开头的点（否则成了隐藏文件）与首尾空格；主名截到 maxMediaStem 个字符，后缀保留。
func cleanMediaName(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	clean := func(s string) string {
		var b strings.Builder
		space := false
		for _, r := range s {
			switch {
			case unicode.IsSpace(r):
				if !space {
					b.WriteByte(' ')
				}
				space = true
				continue
			case unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || strings.ContainsRune("_.-", r):
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
			space = false
		}
		return b.String()
	}
	ext := filepath.Ext(name)
	stem := strings.Trim(clean(strings.TrimSuffix(name, ext)), " .")
	if r := []rune(stem); len(r) > maxMediaStem {
		stem = strings.TrimRight(string(r[:maxMediaStem]), " .")
	}
	if stem == "" {
		stem = "file"
	}
	return stem + clean(ext)
}

// claimMediaName 为一次上传选定不重名的文件名，并在上传期间占住它。
//
// 后台逐个文件上传，几个浏览器标签页还可能同时往同一台设备传：只按请求开始时的列表查重，
// 两个同名文件会选中同一个名字、互相覆盖。占用在文件落盘（或进入转码队列）后释放，
// 那时它已经出现在 mediaList 里了。
func (s *Server) claimMediaName(deviceID, name string) (string, func(), error) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	existing, err := manifest.ListMedia(s.deviceMediaDir(deviceID))
	if err != nil {
		return "", nil, err
	}
	taken := map[string]bool{}
	for _, n := range existing {
		taken[n] = true
	}
	for _, j := range s.jobs.snapshot(deviceID) {
		taken[j.name] = true
	}
	prefix := deviceID + "/"
	for k := range s.uploading {
		if strings.HasPrefix(k, prefix) {
			taken[strings.TrimPrefix(k, prefix)] = true
		}
	}
	name = uniqueName(name, taken)
	s.uploading[prefix+name] = true
	return name, func() {
		s.uploadMu.Lock()
		delete(s.uploading, prefix+name)
		s.uploadMu.Unlock()
	}, nil
}

// countingReader 统计读到的字节数（日志用）。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

// uploadSizeHint 给出本次上传请求的大致大小（单文件上传时就是文件大小）。
func uploadSizeHint(r *http.Request) string {
	if r.ContentLength > 0 {
		return "about " + humanBytes(r.ContentLength)
	}
	return "size unknown"
}

// checkImage 在完整解码之前先读文件头：挡住伪装成图片的文件与超大图（解压炸弹）。
func checkImage(path string) *rejectReason {
	f, err := os.Open(path)
	if err != nil {
		return rejectErr(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return reject("图片无法解码（文件损坏或不是真正的 png/jpg）", "not a decodable png/jpg")
	}
	if cfg.Width*cfg.Height > maxImagePixels {
		return reject(fmt.Sprintf("图片 %d×%d 像素过大（上限约 %d 万像素），请先缩小再上传",
			cfg.Width, cfg.Height, maxImagePixels/10000),
			fmt.Sprintf("%dx%d exceeds the pixel limit", cfg.Width, cfg.Height))
	}
	return nil
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
	if !decodeJSON(w, r, 64<<10, &names) {
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
	if err := s.editPlaylist(dev.ID, func([]string) []string { return order }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeMediaList(w, dev.ID)
}

// handleDeviceMediaThumb 返回播放列表里一张图片的缩略图（后台列表用）；PDF 返回某一页
// （?page=n，默认第 1 页）。视频没有缩略图，回 404。
func (s *Server) handleDeviceMediaThumb(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	name := r.PathValue("file")
	if !manifest.SafeFileName(name) {
		http.NotFound(w, r)
		return
	}
	dir := s.deviceMediaDir(dev.ID)
	path := filepath.Join(dir, name)
	switch manifest.TypeOf(name) {
	case "image":
	case "document":
		pages := manifest.Pages(dir, name)
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n = max(n, 1)
		if n > len(pages) {
			http.NotFound(w, r)
			return
		}
		path = filepath.Join(manifest.PagesPath(dir, name), pages[n-1])
	default:
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 同名文件可能删了又重新上传，所以用修改时间+大小做 ETag，每次让浏览器来问一下。
	etag := fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size())
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var buf bytes.Buffer
	if err := transcode.Thumbnail(&buf, path, 240, 160); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(buf.Bytes())
}

// handleDeleteDeviceMedia 删除一个媒体文件及其播放列表项。
func (s *Server) handleDeleteDeviceMedia(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	name := r.PathValue("file")
	if !manifest.SafeFileName(name) {
		http.Error(w, "bad file name", http.StatusBadRequest)
		return
	}
	if s.jobs.remove(dev.ID, name) { // 处理中的会被取消，失败的直接移除
		log.Printf("processing cancelled: device %s, %s (deleted by operator)", dev.ID, name)
	}
	s.unlinkMedia(dev.ID, name)
	// 播放顺序里不在磁盘上的文件本来就会被跳过，这里顺手把记录也整理干净
	if err := s.editPlaylist(dev.ID, func(current []string) []string { return current }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeMediaList(w, dev.ID)
}
