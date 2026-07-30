// Package manifest 定义播放清单的数据结构，并支持从设备媒体目录构建清单。
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Item 是清单中的一个播放条目。
type Item struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "image" | "video"
	Name     string `json:"name"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Duration int    `json:"duration"` // 秒；仅图片有效，视频为 0 表示播放至结束
	Order    int    `json:"order"`
}

// Manifest 是设备的播放清单。
type Manifest struct {
	Version  string   `json:"version"`
	Items    []Item   `json:"items"`
	Commands []string `json:"commands"`
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".bmp": true, ".webp": true,
}

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".avi": true, ".ts": true, ".webm": true,
}

// TypeOf 根据扩展名返回条目类型，不支持的类型返回空串。
func TypeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case imageExts[ext]:
		return "image"
	case videoExts[ext]:
		return "video"
	default:
		return ""
	}
}

// HashCache 按 (path,size,mtime) 缓存文件 sha256，避免每次轮询重算。
type HashCache struct {
	mu sync.Mutex
	m  map[string]hashEntry
}

type hashEntry struct {
	size  int64
	mtime int64
	sum   string
}

func NewHashCache() *HashCache {
	return &HashCache{m: make(map[string]hashEntry)}
}

// FileSHA256 返回文件内容的 sha256（hex），带缓存。
func (c *HashCache) FileSHA256(path string, size, mtime int64) (string, error) {
	c.mu.Lock()
	e, ok := c.m[path]
	c.mu.Unlock()
	if ok && e.size == size && e.mtime == mtime {
		return e.sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	c.mu.Lock()
	c.m[path] = hashEntry{size: size, mtime: mtime, sum: sum}
	c.mu.Unlock()
	return sum, nil
}

// BuildFromDir 扫描 dir 下的媒体文件（按文件名排序）构建清单。
// 目录不存在视为空清单。隐藏文件、下载临时文件与不支持的类型会被跳过。
func BuildFromDir(dir, deviceID string, imageDuration int, cache *HashCache) (*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			entries = nil
		} else {
			return nil, err
		}
	}

	type fileInfo struct {
		name  string
		size  int64
		mtime int64
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".part") {
			continue
		}
		if TypeOf(name) == "" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		files = append(files, fileInfo{name: name, size: info.Size(), mtime: info.ModTime().Unix()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	// 版本号: 文件名/大小/修改时间列表的摘要，目录内容不变则版本稳定。
	vh := sha256.New()
	for _, f := range files {
		fmt.Fprintf(vh, "%s|%d|%d\n", f.name, f.size, f.mtime)
	}
	version := hex.EncodeToString(vh.Sum(nil))[:12]

	m := &Manifest{Version: version, Items: []Item{}, Commands: []string{}}
	for i, f := range files {
		sum, err := cache.FileSHA256(filepath.Join(dir, f.name), f.size, f.mtime)
		if err != nil {
			return nil, err
		}
		item := Item{
			ID:     sum[:12],
			Type:   TypeOf(f.name),
			Name:   f.name,
			URL:    "/media/" + url.PathEscape(deviceID) + "/" + url.PathEscape(f.name),
			SHA256: sum,
			Size:   f.size,
			Order:  i + 1,
		}
		if item.Type == "image" {
			item.Duration = imageDuration
		}
		m.Items = append(m.Items, item)
	}
	return m, nil
}
