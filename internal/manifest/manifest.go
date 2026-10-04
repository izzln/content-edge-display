// Package manifest 定义设备与服务端之间的协议：播放清单、注册与心跳的报文、响应头约定，
// 并负责从媒体目录构建播放条目。两端共用这里的类型，字段含义不会各说各话。
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
	Type     string `json:"type"` // "image" | "video"
	Name     string `json:"name"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Duration int    `json:"duration"` // 秒；仅图片有效，视频为 0 表示播放至结束
}

// Command 是随清单下发的运维指令。
type Command struct {
	Type    string `json:"type"` // "update"
	Version string `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// Rect 是画布上的一个矩形（像素）。
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Layout 描述“模板承载媒体”的合成方式：Overlay 是一张整屏 PNG，媒体区被挖成全透明；
// 设备端把它贴在画面之上，并把播放内容限制在 Media 矩形里、按 cover 撑满。
// 这样属性/文字变化只需重发这张小 PNG，视频完全不用重新编码。
//
// 为 nil 表示整屏播放（测试卡，或模板已渲染成一张整屏图）。
type Layout struct {
	CanvasW int  `json:"canvas_w"`
	CanvasH int  `json:"canvas_h"`
	Media   Rect `json:"media"`
	Overlay Item `json:"overlay"`
}

// Manifest 是设备的播放清单。
type Manifest struct {
	Version  string    `json:"version"`
	Items    []Item    `json:"items"`
	Layout   *Layout   `json:"layout,omitempty"`
	Commands []Command `json:"commands"`
}

// Downloads 返回本份清单需要设备端下载校验的全部文件（播放条目 + 叠加图）。
func (m *Manifest) Downloads() []Item {
	out := append([]Item(nil), m.Items...)
	if m.Layout != nil {
		out = append(out, m.Layout.Overlay)
	}
	return out
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".bmp": true, ".webp": true,
}

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".avi": true, ".ts": true, ".webm": true, ".m4v": true,
}

// PagesDir 是媒体目录下存放文档逐页渲染图的隐藏目录：<媒体目录>/.pages/<文档名>/p001.jpg …
// 隐藏目录不会被 ListMedia 当成媒体；清单生成时文档展开成这些页面。
const PagesDir = ".pages"

// SafeFileName 判断 name 是不是可以直接拼进目录的单个文件名：不含路径成分、不是隐藏文件。
// 设备下载、后台删除/缩略图、构建清单都要过这一关。
func SafeFileName(name string) bool {
	return name != "" && name == filepath.Base(name) && !strings.HasPrefix(name, ".") && !strings.ContainsRune(name, '\\')
}

// TypeOf 根据扩展名返回条目类型（image | video | document），不支持的类型返回空串。
// document 只出现在服务端的播放列表里，下发给设备时展开成逐页的图片条目。
func TypeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case imageExts[ext]:
		return "image"
	case videoExts[ext]:
		return "video"
	case ext == ".pdf":
		return "document"
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

// FileSHA256 返回文件内容的 sha256（hex）。
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileSHA256 返回文件内容的 sha256（hex），带缓存。
func (c *HashCache) FileSHA256(path string, size, mtime int64) (string, error) {
	c.mu.Lock()
	e, ok := c.m[path]
	c.mu.Unlock()
	if ok && e.size == size && e.mtime == mtime {
		return e.sum, nil
	}
	sum, err := FileSHA256(path)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.m[path] = hashEntry{size: size, mtime: mtime, sum: sum}
	c.mu.Unlock()
	return sum, nil
}

// Version 由条目、指令与叠加布局计算清单版本号：内容不变则版本稳定（设备收到 304），
// 任何一项出现/消失/变化都会让版本号变，触发设备刷新。
//
// 参与计算的不只是文件本身，还有影响播放行为的字段（类型、停留时长、媒体区位置）：
// 只改停留时长或只改模板属性文字而媒体文件不变时版本号也必须变，
// 否则设备一直收到 304，新设置永远到不了现场。
func Version(items []Item, cmds []Command, layout *Layout) string {
	h := sha256.New()
	for _, it := range items {
		fmt.Fprintf(h, "%s|%s|%s|%d\n", it.Name, it.SHA256, it.Type, it.Duration)
	}
	for _, c := range cmds {
		fmt.Fprintf(h, "cmd|%s|%s|%s\n", c.Type, c.Version, c.SHA256)
	}
	if layout != nil {
		fmt.Fprintf(h, "layout|%d|%d|%d|%d|%d|%d|%s\n",
			layout.CanvasW, layout.CanvasH,
			layout.Media.X, layout.Media.Y, layout.Media.W, layout.Media.H,
			layout.Overlay.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ListMedia 返回 dir 下受支持的媒体文件名，按文件名排序。
// 目录不存在视为空目录。隐藏文件、下载临时文件与不支持的类型会被跳过。
func ListMedia(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
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
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// BuildItems 按给定顺序为 names 构建播放条目。已不存在的文件会被跳过
// （管理后台的播放列表可能残留刚被删掉的文件名，不该因此让整份清单构建失败）。
// 文档展开成逐页的图片条目，与图片一样按 imageDuration 停留。
func BuildItems(dir, deviceID string, names []string, imageDuration int, cache *HashCache) ([]Item, error) {
	items := []Item{}
	add := func(path, name, rawURL, typ string) error {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		sum, err := cache.FileSHA256(path, info.Size(), info.ModTime().Unix())
		if err != nil {
			return err
		}
		item := Item{Type: typ, Name: name, URL: rawURL, SHA256: sum, Size: info.Size()}
		if typ == "image" {
			item.Duration = imageDuration
		}
		items = append(items, item)
		return nil
	}
	base := "/media/" + url.PathEscape(deviceID) + "/"
	for _, name := range names {
		if !SafeFileName(name) {
			continue
		}
		var err error
		switch typ := TypeOf(name); typ {
		case "image", "video":
			err = add(filepath.Join(dir, name), name, base+url.PathEscape(name), typ)
		case "document":
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			for _, page := range Pages(dir, name) {
				err = add(filepath.Join(dir, PagesDir, name, page), stem+"-"+page,
					base+url.PathEscape(name)+"/"+url.PathEscape(page), "image")
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

// Pages 返回文档已渲染好的页面文件名（按页序）；还没渲染或目录不存在时为空。
func Pages(dir, doc string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, PagesDir, doc))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && SafeFileName(e.Name()) && TypeOf(e.Name()) == "image" {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
