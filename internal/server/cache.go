package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
)

// 文件缓存区（内容仓库）。
//
// 处理好的媒体文件（缩过的图片、转好的视频、PDF 及其页面）都存一份在 media_root/.store/，
// 以内容的 sha256 命名；设备目录里的文件是指向它的硬链接（同一文件系统，不占额外空间），
// 文件系统不支持硬链接时退回复制。于是：
//   - 后台删除文件、删除设备只是删掉设备目录里的链接，仓库里那份留着，变成"缓存中"；
//   - 同一原片再次上传（不论哪台设备）直接链接已有结果，立即就绪，不再转码/渲染；
//   - 仓库总大小超过配额时，从最久没被使用的开始清，设备在用的永远不清。
//
// 设备目录、清单、下载地址都不变，设备端对此一无所知。

// cacheBlob 是仓库里的一份内容。
type cacheBlob struct {
	Ext      string    `json:"ext"`
	Size     int64     `json:"size"` // 含 PDF 页面
	LastUsed time.Time `json:"last_used"`
}

// cacheIndex 持久化在 data_dir/cache.json。
type cacheIndex struct {
	// Sources：处理键（原片 sha256 + 处理参数）→ 结果的 sha256。参数一变，旧结果自然不再复用。
	Sources map[string]string     `json:"sources"`
	Blobs   map[string]*cacheBlob `json:"blobs"`
}

// CacheStats 是缓存区的现状（后台"存储"页用）。
type CacheStats struct {
	QuotaGB     int   `json:"quota_gb"`
	MinQuotaGB  int   `json:"min_quota_gb"` // 不能低于在用文件的总大小
	UsedBytes   int64 `json:"used_bytes"`
	InUseBytes  int64 `json:"in_use_bytes"`
	CachedBytes int64 `json:"cached_bytes"` // 未在用、可被清理的
	Files       int   `json:"files"`
	InUseFiles  int   `json:"in_use_files"`
	DiskFree    int64 `json:"disk_free_bytes"` // -1 表示读不到
}

type contentCache struct {
	root  string // media_root/.store
	path  string // data_dir/cache.json
	runMu sync.Mutex
	mu    sync.Mutex
	idx   cacheIndex
	stats CacheStats
	kick  chan struct{}

	gb int64 // 配额 1GB 对应的字节数（在 mu 下读写；测试调小它，好用小文件触发淘汰）
}

func openCache(root, path string) (*contentCache, error) {
	c := &contentCache{root: root, path: path, kick: make(chan struct{}, 1), gb: 1 << 30,
		idx: cacheIndex{Sources: map[string]string{}, Blobs: map[string]*cacheBlob{}}}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &c.idx); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if c.idx.Sources == nil {
			c.idx.Sources = map[string]string{}
		}
		if c.idx.Blobs == nil {
			c.idx.Blobs = map[string]*cacheBlob{}
		}
	}
	return c, nil
}

func (c *contentCache) saveLocked() {
	data, err := json.MarshalIndent(&c.idx, "", " ")
	if err == nil {
		err = fsutil.WriteFile(c.path, data, 0o644)
	}
	if err != nil {
		log.Printf("cache: cannot save index: %v", err)
	}
}

func (c *contentCache) blobPath(sha, ext string) string { return filepath.Join(c.root, sha+ext) }
func (c *contentCache) pagesPath(sha string) string     { return filepath.Join(c.root, sha+".pages") }

// processKey 是"这份原片按当前参数处理"的标识。
func processKey(kind, srcSHA string) string {
	var params string
	switch kind {
	case "image":
		params = fmt.Sprintf("shrink %dx%d", manifest.CanvasW, manifest.CanvasH)
	case "video":
		params = fmt.Sprintf("%+v", transcode.DefaultSpec())
	case "document":
		params = fmt.Sprintf("pages w%d", transcode.PageWidth)
	}
	return kind + "|" + params + "|" + srcSHA
}

// cacheReuse 查这份原片有没有处理好的结果；有就链接成设备文件 dir/name（PDF 连页面），返回 true。
func (s *Server) cacheReuse(dir, name, key string) (bool, error) {
	c := s.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	sha, ok := c.idx.Sources[key]
	if !ok {
		return false, nil
	}
	b := c.idx.Blobs[sha]
	if b == nil {
		return false, nil
	}
	src := c.blobPath(sha, b.Ext)
	if _, err := os.Stat(src); err != nil {
		return false, nil // 仓库里的文件不见了：当作没有，重新处理
	}
	if err := linkContent(src, filepath.Join(dir, name), c.pagesPath(sha), manifest.PagesPath(dir, name)); err != nil {
		return false, err
	}
	b.LastUsed = time.Now()
	c.saveLocked()
	return true, nil
}

// cacheAdopt 把设备文件 dir/name（PDF 连页面）纳入仓库，返回内容的 sha256：仓库里已有同样内容就让设备文件
// 改为链接它（省空间），没有就把它链接进仓库。key 非空时登记"这份原片 → 这个结果"，供以后复用。
func (s *Server) cacheAdopt(dir, name, key string) (string, error) {
	path := filepath.Join(dir, name)
	sha, size, err := s.hashes.Sum(path)
	if err != nil {
		return "", err
	}
	c := s.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.idx.Blobs[sha]
	if b != nil {
		if _, err := os.Stat(c.blobPath(sha, b.Ext)); err != nil {
			b = nil // 索引里有、文件没了：重新纳入
		}
	}
	pages := manifest.PagesPath(dir, name)
	if b == nil {
		ext := strings.ToLower(filepath.Ext(name))
		if err := linkContent(path, c.blobPath(sha, ext), pages, c.pagesPath(sha)); err != nil {
			return "", err
		}
		b = &cacheBlob{Ext: ext, Size: size + dirSize(c.pagesPath(sha))}
		c.idx.Blobs[sha] = b
	} else if err := linkContent(c.blobPath(sha, b.Ext), path, c.pagesPath(sha), pages); err != nil {
		return "", err
	}
	b.LastUsed = time.Now()
	if key != "" {
		c.idx.Sources[key] = sha
	}
	c.saveLocked()
	return sha, nil
}

// linkContent 让 dst 成为 src 的硬链接；src 是文档（PDF）时把它的页面目录 srcPages 也链接到 dstPages。
// 仓库里的页面目录是 <sha>.pages，设备目录里是 .pages/<文件名>/。
func linkContent(src, dst, srcPages, dstPages string) error {
	if manifest.TypeOf(dst) == "document" || manifest.TypeOf(src) == "document" {
		if err := linkPages(srcPages, dstPages); err != nil {
			return err
		}
	}
	return fsutil.LinkFile(src, dst)
}

// cacheTouch 在设备文件被删除前记下它的仓库内容"刚刚还在用"，淘汰顺序才准。
func (s *Server) cacheTouch(paths ...string) {
	var shas []string
	for _, p := range paths {
		if sha, _, err := s.hashes.Sum(p); err == nil {
			shas = append(shas, sha)
		}
	}
	c := s.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, sha := range shas {
		if b := c.idx.Blobs[sha]; b != nil {
			b.LastUsed = now
		}
	}
	c.saveLocked()
}

// quotaBytes 是当前配额的字节数。
func (s *Server) quotaBytes() int64 {
	var gb int
	s.store.View(func(st *store.State) { gb = st.CacheQuotaGB })
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	return int64(gb) * s.cache.gb
}

// kickCache 让缓存协程尽快整理一次（不阻塞）。
func (s *Server) kickCache() {
	select {
	case s.cache.kick <- struct{}{}:
	default:
	}
}

// cacheStats 返回最近一次整理时的统计。
func (s *Server) cacheStats() CacheStats {
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	return s.cache.stats
}

// reconcileCache 整理缓存区：把还没纳入仓库的设备文件纳入（首次启动的存量、运营方直接拷进目录的），
// 刷新在用内容的最近使用时间，超出配额时按最近使用时间从旧到新清掉未在用的内容，最后更新统计。
func (s *Server) reconcileCache() {
	c := s.cache
	c.runMu.Lock()
	defer c.runMu.Unlock()
	start := time.Now()

	// 1. 设备在用的内容（算 sha256 走 HashCache，大多命中缓存）；没纳入的纳入
	inUse := map[string]bool{}
	devDirs, _ := os.ReadDir(s.cfg.MediaRoot)
	for _, d := range devDirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		dir := filepath.Join(s.cfg.MediaRoot, d.Name())
		names, err := manifest.ListMedia(dir)
		if err != nil {
			continue
		}
		for _, name := range names {
			if manifest.TypeOf(name) == "document" && len(manifest.Pages(dir, name)) == 0 {
				continue // 没渲染过的 PDF（直接拷进目录的）：不下发，也不纳入
			}
			path := filepath.Join(dir, name)
			sha, _, err := s.hashes.Sum(path)
			if err != nil {
				continue
			}
			c.mu.Lock()
			known := c.idx.Blobs[sha] != nil
			c.mu.Unlock()
			if !known {
				if sha, err = s.cacheAdopt(dir, name, ""); err != nil {
					log.Printf("cache: cannot adopt %s: %v", path, err)
					continue
				}
			}
			inUse[sha] = true
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var used, inUseBytes int64
	inUseFiles := 0
	for sha, b := range c.idx.Blobs {
		used += b.Size
		if inUse[sha] {
			b.LastUsed = now
			inUseBytes += b.Size
			inUseFiles++
		}
	}

	// 2. 超出配额：清最久没用的（整理开始后才被用到的不动——可能刚被复用、链接进了设备目录）
	var quotaGB int
	s.store.View(func(st *store.State) { quotaGB = st.CacheQuotaGB })
	quota := int64(quotaGB) * c.gb
	if used > quota {
		var idle []string
		for sha, b := range c.idx.Blobs {
			if !inUse[sha] && b.LastUsed.Before(start) {
				idle = append(idle, sha)
			}
		}
		sort.Slice(idle, func(i, j int) bool { return c.idx.Blobs[idle[i]].LastUsed.Before(c.idx.Blobs[idle[j]].LastUsed) })
		for _, sha := range idle {
			if used <= quota {
				break
			}
			b := c.idx.Blobs[sha]
			os.Remove(c.blobPath(sha, b.Ext))
			os.RemoveAll(c.pagesPath(sha))
			delete(c.idx.Blobs, sha)
			for k, v := range c.idx.Sources {
				if v == sha {
					delete(c.idx.Sources, k)
				}
			}
			used -= b.Size
			log.Printf("cache: evicted %s%s (%s, last used %s) to stay within %dGB",
				sha[:12], b.Ext, humanBytes(b.Size), b.LastUsed.Format(time.DateTime), quota/c.gb)
		}
	}
	c.saveLocked()

	c.stats = CacheStats{
		QuotaGB:     int(quota / c.gb),
		MinQuotaGB:  int(max(1, (inUseBytes+c.gb-1)/c.gb)),
		UsedBytes:   used,
		InUseBytes:  inUseBytes,
		CachedBytes: used - inUseBytes,
		Files:       len(c.idx.Blobs),
		InUseFiles:  inUseFiles,
		DiskFree:    diskFree(c.root),
	}
}

// runCache 是缓存整理协程：启动时整理一次（纳入存量文件），之后每小时一次、有变动时立即一次。
func (s *Server) runCache(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.reconcileCache()
		s.gcBackgrounds()
		select {
		case <-stop:
			return
		case <-t.C:
		case <-s.cache.kick:
		}
	}
}

// linkPages 把页面目录 srcDir 里的文件逐个链接到 dstDir（dstDir 原有内容整个换掉）。
func linkPages(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dstDir), "."+filepath.Base(dstDir)+".link")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := fsutil.LinkFile(filepath.Join(srcDir, e.Name()), filepath.Join(tmp, e.Name())); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	os.RemoveAll(dstDir)
	return os.Rename(tmp, dstDir)
}

func dirSize(dir string) int64 {
	var n int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			n += info.Size()
		}
	}
	return n
}

// handleGetCache 当场整理一次并返回缓存区现状（文件 sha256 有缓存，整理很快）。
func (s *Server) handleGetCache(w http.ResponseWriter, r *http.Request) {
	s.reconcileCache()
	writeJSON(w, s.cacheStats())
}

// handlePutCache 修改缓存区配额：不能小于设备在用文件的总大小；调小后缓存协程随即按新配额清理。
func (s *Server) handlePutCache(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QuotaGB int `json:"quota_gb"`
	}
	if !decodeJSON(w, r, 1<<10, &req) {
		return
	}
	s.reconcileCache() // 用最新的在用总量校验
	if st := s.cacheStats(); req.QuotaGB < st.MinQuotaGB {
		http.Error(w, fmt.Sprintf("缓存区不能小于设备在用文件的总大小：至少 %dGB（在用 %s）", st.MinQuotaGB, humanBytes(st.InUseBytes)),
			http.StatusBadRequest)
		return
	}
	if s.update(w, func(st *store.State) { st.CacheQuotaGB = req.QuotaGB }) {
		log.Printf("cache quota set to %dGB", req.QuotaGB)
		s.kickCache()
		w.WriteHeader(http.StatusNoContent)
	}
}
