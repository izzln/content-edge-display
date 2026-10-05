package server

import (
	"context"
	"errors"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
)

// 播放内容的静止画面：后台缩略图（240×160）与效果预览。视频要用 ffmpeg 抽一帧。抽出的帧与缩略图都按源文件
// 内容的 sha256 缓存在 data_dir/thumbs/：同一内容只生成一次，30 天没用过的在每小时维护时删掉。

const thumbMaxAge = 30 * 24 * time.Hour

// handleDeviceMediaThumb 返回播放列表里一项的缩略图：图片本身、PDF 某一页（?page=n，默认第 1 页）、
// 视频的一帧（没有 ffmpeg 时 404，后台显示 ▶）。
func (s *Server) handleDeviceMediaThumb(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	name, dir := r.PathValue("file"), s.deviceMediaDir(dev.ID)
	if !manifest.SafeFileName(name) || manifest.TypeOf(name) == "" {
		http.NotFound(w, r)
		return
	}
	src := filepath.Join(dir, name)
	if manifest.TypeOf(name) == "document" {
		pages := manifest.Pages(dir, name)
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if n = max(n, 1); n > len(pages) {
			http.NotFound(w, r)
			return
		}
		src = filepath.Join(manifest.PagesPath(dir, name), pages[n-1])
	}
	info, err := os.Stat(src)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 同名文件可能删了又重新上传，所以按源文件的修改时间+大小做 ETag，每次让浏览器来问一下；没变就不必生成
	etag := fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size())
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	thumb, err := s.thumbnail(r.Context(), src, manifest.TypeOf(name) == "video")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, thumb)
}

// thumbnail 返回 src（图片；video 时是视频）的缩略图路径。
func (s *Server) thumbnail(ctx context.Context, src string, video bool) (string, error) {
	if video {
		var err error
		if src, err = s.videoFrame(ctx, src); err != nil {
			return "", err
		}
	}
	return s.derived(src, "-thumb.jpg", func(tmp string) error {
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		err = transcode.Thumbnail(f, src, 240, 160)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
}

// videoFrame 返回视频 path 的一帧 JPEG 的路径；没有 ffmpeg 时报错。
func (s *Server) videoFrame(ctx context.Context, path string) (string, error) {
	if s.tools.enc == nil {
		return "", errors.New("ffmpeg not available")
	}
	return s.derived(path, ".jpg", func(tmp string) error { return s.tools.enc.Frame(ctx, path, tmp) })
}

// derived 返回由 src 生成的文件 thumbs/<src 内容 sha256 前 16 位><suffix>：已有就刷新修改时间（回收时保留），
// 没有就用 make 生成（写到临时文件，后缀与 suffix 相同：ffmpeg 按扩展名选输出格式）。一次只生成一个，
// 同一内容的并发请求等第一个生成完直接用。
func (s *Server) derived(src, suffix string, make func(tmp string) error) (string, error) {
	sum, _, err := s.hashes.Sum(src)
	if err != nil {
		return "", err
	}
	out := filepath.Join(s.thumbsDir(), sum[:16]+suffix)
	touch := func() bool { now := time.Now(); return os.Chtimes(out, now, now) == nil }
	if touch() {
		return out, nil
	}
	s.thumbMu.Lock()
	defer s.thumbMu.Unlock()
	if touch() {
		return out, nil
	}
	tmp := filepath.Join(s.incomingDir(), "derived-"+sum[:16]+suffix)
	defer os.Remove(tmp)
	if err := make(tmp); err != nil {
		return "", err
	}
	return out, os.Rename(tmp, out)
}

// gcThumbs 删掉很久没用过的缩略图与视频帧。
func (s *Server) gcThumbs() {
	pruneDir(s.thumbsDir(), func(_ string, info os.FileInfo) bool { return time.Since(info.ModTime()) < thumbMaxAge })
}

// stillImage 返回设备播放列表里 item（空 = 第一项）的静止画面：图片本身、PDF 第 1 页、视频抽帧。
// 播放列表为空或取不到画面时返回 nil。
func (s *Server) stillImage(ctx context.Context, deviceID string, disp store.DisplayConfig, item string) image.Image {
	dir := s.deviceMediaDir(deviceID)
	files, err := orderPlaylist(dir, disp.Playlist)
	if err != nil || len(files) == 0 {
		return nil
	}
	names := manifest.Names(files)
	name := names[0]
	if slices.Contains(names, item) {
		name = item
	}
	path := filepath.Join(dir, name)
	switch manifest.TypeOf(name) {
	case "document":
		pages := manifest.Pages(dir, name)
		if len(pages) == 0 {
			return nil
		}
		path = filepath.Join(manifest.PagesPath(dir, name), pages[0])
	case "video":
		if path, err = s.videoFrame(ctx, path); err != nil {
			return nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil
	}
	return img
}
