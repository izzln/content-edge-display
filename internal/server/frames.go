package server

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

// 播放内容的静止画面（后台缩略图、效果预览）。视频要用 ffmpeg 抽帧，按内容 sha256 缓存在 data_dir/thumbs/：
// 同一个视频只抽一次，30 天没用过的在每小时维护时删掉。

const thumbMaxAge = 30 * 24 * time.Hour

func (s *Server) thumbsDir() string { return filepath.Join(s.cfg.DataDir, "thumbs") }

// videoFrame 返回视频 path 的一帧 JPEG 的路径；没有 ffmpeg 时报错。
func (s *Server) videoFrame(ctx context.Context, path string) (string, error) {
	if s.tools.enc == nil {
		return "", errors.New("ffmpeg not available")
	}
	sum, _, err := s.hashes.Sum(path)
	if err != nil {
		return "", err
	}
	out := filepath.Join(s.thumbsDir(), sum[:16]+".jpg")
	if _, err := os.Stat(out); err == nil {
		now := time.Now()
		os.Chtimes(out, now, now) // 记下最近用过：回收时保留
		return out, nil
	}
	f, err := os.CreateTemp(s.thumbsDir(), ".frame-*.jpg") // ffmpeg 按扩展名选输出格式
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	f.Close()
	if err := s.tools.enc.Frame(ctx, path, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return out, os.Rename(tmp, out)
}

// gcThumbs 删掉很久没用过的视频帧。
func (s *Server) gcThumbs() {
	entries, _ := os.ReadDir(s.thumbsDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > thumbMaxAge {
			os.Remove(filepath.Join(s.thumbsDir(), e.Name()))
		}
	}
}

// stillImage 返回设备播放列表里 item（空 = 第一项）的静止画面：图片本身、PDF 第 1 页、视频抽帧。
// 播放列表为空或取不到画面时返回 nil。
func (s *Server) stillImage(ctx context.Context, deviceID string, disp store.DisplayConfig, item string) image.Image {
	dir := s.deviceMediaDir(deviceID)
	names, err := orderPlaylist(dir, disp.Playlist)
	if err != nil || len(names) == 0 {
		return nil
	}
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
