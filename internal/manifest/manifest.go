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
	"time"
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

// Update 是随清单下发的程序更新：设备下载这个程序包、执行包内 update.sh 后切换到 Version。
type Update struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// CanvasW/H 是模板画布与内容处理的基准尺寸：模板按它排版，上传的图片、视频、PDF 页面都缩到它以内。
const CanvasW, CanvasH = 1440, 900

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
	Version string  `json:"version"`
	Items   []Item  `json:"items"`
	Layout  *Layout `json:"layout,omitempty"`
	Update  *Update `json:"update,omitempty"`
	Access  *Access `json:"access,omitempty"`
}

// Downloads 返回本份清单需要设备端下载校验的全部文件（播放条目 + 叠加图）。
func (m *Manifest) Downloads() []Item {
	out := append([]Item(nil), m.Items...)
	if m.Layout != nil {
		out = append(out, m.Layout.Overlay)
	}
	return out
}

var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true}

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".avi": true, ".ts": true, ".webm": true, ".m4v": true,
}

// PagesDir 是媒体目录下存放文档逐页渲染图的隐藏目录：<媒体目录>/.pages/<文档名>/p001.jpg …
// 隐藏目录不会被 ListMedia 当成媒体；清单生成时文档展开成这些页面。
const PagesDir = ".pages"

// PagesPath 是文档 doc 的逐页图片目录。
func PagesPath(dir, doc string) string { return filepath.Join(dir, PagesDir, doc) }

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

// Prune 丢掉文件已经不在的条目（删掉的媒体、换掉的渲染图），每小时维护时调用。
func (c *HashCache) Prune() {
	c.mu.Lock()
	paths := make([]string, 0, len(c.m))
	for p := range c.m {
		paths = append(paths, p)
	}
	c.mu.Unlock()
	for _, p := range paths {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			c.mu.Lock()
			delete(c.m, p)
			c.mu.Unlock()
		}
	}
}

// Sum 返回文件的 sha256（hex）与大小；大小与修改时间没变时用缓存，不重读文件。
func (c *HashCache) Sum(path string) (sum string, size int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	size, mtime := info.Size(), info.ModTime().UnixNano()
	c.mu.Lock()
	e, ok := c.m[path]
	c.mu.Unlock()
	if ok && e.size == size && e.mtime == mtime {
		return e.sum, size, nil
	}
	if sum, err = FileSHA256(path); err != nil {
		return "", 0, err
	}
	c.mu.Lock()
	c.m[path] = hashEntry{size: size, mtime: mtime, sum: sum}
	c.mu.Unlock()
	return sum, size, nil
}

// Version 由条目、程序更新、叠加布局与访问凭据计算清单版本号：内容不变则版本稳定（设备收到 304），
// 任何一项出现/消失/变化都会让版本号变，触发设备刷新。
//
// 参与计算的不只是文件本身，还有影响播放行为的字段（类型、停留时长、媒体区位置）：
// 只改停留时长或只改模板属性文字而媒体文件不变时版本号也必须变，
// 否则设备一直收到 304，新设置永远到不了现场。
func Version(m Manifest) string {
	h := sha256.New()
	for _, it := range m.Items {
		fmt.Fprintf(h, "%s|%s|%s|%d\n", it.Name, it.SHA256, it.Type, it.Duration)
	}
	if u := m.Update; u != nil {
		fmt.Fprintf(h, "update|%s|%s\n", u.Version, u.SHA256)
	}
	if l := m.Layout; l != nil {
		fmt.Fprintf(h, "layout|%d|%d|%d|%d|%d|%d|%s\n",
			l.CanvasW, l.CanvasH, l.Media.X, l.Media.Y, l.Media.W, l.Media.H, l.Overlay.SHA256)
	}
	if a := m.Access; a != nil {
		fmt.Fprintf(h, "access|%s|%s\n", a.RootHash, strings.Join(a.SSHKeys, "|"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// File 是媒体目录里的一个文件。带着大小与修改时间：同名文件被替换也看得出来。
type File struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// ListMedia 返回 dir 下受支持的媒体文件，按文件名排序（os.ReadDir 已排好）。
// 目录不存在视为空目录。隐藏文件（含各种临时文件）与不支持的类型会被跳过。
func ListMedia(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []File{}, nil
		}
		return nil, err
	}
	out := []File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || TypeOf(name) == "" {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, File{Name: name, Size: info.Size(), ModTime: info.ModTime()})
		}
	}
	return out, nil
}

// Names 返回 files 的文件名。
func Names(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}

// BuildItems 按给定顺序为 names 构建播放条目。已不存在的文件会被跳过
// （管理后台的播放列表可能残留刚被删掉的文件名，不该因此让整份清单构建失败）。
// 文档展开成逐页的图片条目，与图片一样按 imageDuration 停留。
func BuildItems(dir, deviceID string, names []string, imageDuration int, cache *HashCache) ([]Item, error) {
	items := []Item{}
	add := func(path, name, rawURL, typ string) error {
		sum, size, err := cache.Sum(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		item := Item{Type: typ, Name: name, URL: rawURL, SHA256: sum, Size: size}
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
				err = add(filepath.Join(PagesPath(dir, name), page), stem+"-"+page,
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
	entries, err := os.ReadDir(PagesPath(dir, doc))
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
