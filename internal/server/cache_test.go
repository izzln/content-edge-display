package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/testpdf"
)

// smallGB 把配额单位调成 n 字节，好用几百字节的文件触发淘汰。
func smallGB(s *Server, n int64) {
	s.cache.mu.Lock()
	s.cache.gb = n
	s.cache.mu.Unlock()
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		t.Fatalf("stat: %v %v", err1, err2)
	}
	return os.SameFile(fa, fb)
}

func devFile(s *Server, dev, name string) string { return filepath.Join(s.deviceMediaDir(dev), name) }

// 同一原片再次上传（同一台、另一台设备）：直接复用缓存区里的结果，不再转码，立即就绪；
// 设备文件是缓存区里同一份内容的硬链接，不占额外空间。
func TestReuploadReusesCachedResult(t *testing.T) {
	s, h := newAdminTestServer(t)
	enc := &fakeEncoder{}
	s.setEncoder(enc)

	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"clip.mov", fakeVideo}, upload{"a.png", tinyPNG(t)}))
	waitMedia(t, h, "转码完成", func(fs []MediaFile) bool { f, ok := byName(fs, "clip.mp4"); return ok && f.Status == mediaReady })

	res := parseUpload(t, uploadMedia(t, h, "dev-002", upload{"clip.mov", fakeVideo}, upload{"a.png", tinyPNG(t)}))
	if strings.Join(res.Reused, ",") != "clip.mp4,a.png" || strings.Join(res.Accepted, ",") != "clip.mp4,a.png" || len(res.Queued) != 0 {
		t.Fatalf("同一原片应直接复用、立即就绪：%+v", res)
	}
	if enc.calls != 1 {
		t.Fatalf("同一原片只应转码一次，实际 %d 次", enc.calls)
	}
	for _, n := range []string{"clip.mp4", "a.png"} {
		if !sameFile(t, devFile(s, testDeviceID, n), devFile(s, "dev-002", n)) {
			t.Errorf("%s：两台设备的文件应是同一份内容的硬链接", n)
		}
	}
	// 另一台设备的清单里也有了
	m := manifestFor(t, h, "dev-002", "other-secret")
	if len(m.Items) != 2 || m.Items[0].Name != "clip.mp4" {
		t.Fatalf("dev-002 的清单：%+v", m.Items)
	}
	// 内容不同的原片不复用
	if res := parseUpload(t, uploadMedia(t, h, "dev-002", upload{"other.mov", []byte("another video")})); len(res.Reused) != 0 {
		t.Fatalf("不同原片不应复用：%+v", res)
	}
}

// 删除文件、删除设备都不真正删内容：留在缓存区里，再上传同一原片立即就绪。
func TestDeletedContentStaysCached(t *testing.T) {
	s, h := newAdminTestServer(t)
	enc := &fakeEncoder{}
	s.setEncoder(enc)
	parseUpload(t, uploadMedia(t, h, "dev-002", upload{"clip.mov", fakeVideo}))
	waitForCond(t, "转码完成", func() bool { _, err := os.Stat(devFile(s, "dev-002", "clip.mp4")); return err == nil })
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/dev-002", nil), http.StatusNoContent)

	s.reconcileCache()
	st := s.cacheStats()
	if st.Files != 1 || st.InUseBytes != 0 || st.CachedBytes != int64(len(fakeVideo)) {
		t.Fatalf("删除设备后内容应留在缓存区、不再算在用：%+v", st)
	}
	res := parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"clip.mov", fakeVideo}))
	if len(res.Reused) != 1 || enc.calls != 1 {
		t.Fatalf("已删除设备上传过的原片应直接复用：%+v，转码 %d 次", res, enc.calls)
	}
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/clip.mp4", nil), http.StatusOK)
	if res := parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"again.mov", fakeVideo})); strings.Join(res.Reused, ",") != "again.mp4" {
		t.Fatalf("删除后再传同一原片应复用：%+v", res)
	}
}

// 超出配额时从最久没用的开始清，设备在用的永远不清。
func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	s, h := newAdminTestServer(t)
	smallGB(s, 1200)
	s.store.Update(func(st *store.State) error { st.CacheQuotaGB = 2; return nil }) // 2400 字节
	file := func(c byte) []byte { return bytes.Repeat([]byte{c}, 600) }
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		putMediaFile(t, s, n, file(n[0]))
	}
	s.reconcileCache() // 运营方直接拷进来的文件也纳入缓存区
	if st := s.cacheStats(); st.Files != 3 || st.InUseFiles != 3 {
		t.Fatalf("纳入后：%+v", s.cacheStats())
	}
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/a.jpg", nil), http.StatusOK)
	time.Sleep(20 * time.Millisecond)
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/b.jpg", nil), http.StatusOK)
	putMediaFile(t, s, "d.jpg", file('d'))
	putMediaFile(t, s, "e.jpg", file('e'))
	time.Sleep(20 * time.Millisecond)
	s.reconcileCache() // 在用 c d e = 1800，缓存 a b = 1200，共 3000 > 2400：清掉最久没用的 a

	st := s.cacheStats()
	if st.UsedBytes != 2400 || st.InUseBytes != 1800 || st.Files != 4 {
		t.Fatalf("应只清掉一个最久没用的：%+v", st)
	}
	blobs := storeFiles(t, s)
	if blobs[string(file('a'))] || !blobs[string(file('b'))] {
		t.Fatalf("应清掉 a（最久没用）而保留 b")
	}

	// 在用的超过配额也不清
	s.store.Update(func(st *store.State) error { st.CacheQuotaGB = 1; return nil })
	s.reconcileCache()
	for _, n := range []string{"c.jpg", "d.jpg", "e.jpg"} {
		if _, err := os.Stat(devFile(s, testDeviceID, n)); err != nil {
			t.Fatalf("在用的 %s 不能被清掉", n)
		}
	}
	if st := s.cacheStats(); st.CachedBytes != 0 {
		t.Fatalf("超额时未在用的应全部清掉：%+v", st)
	}
}

// storeFiles 返回仓库里现有文件的内容集合。
func storeFiles(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(s.cfg.MediaRoot, ".store"))
	for _, e := range entries {
		if !e.IsDir() {
			data, _ := os.ReadFile(filepath.Join(s.cfg.MediaRoot, ".store", e.Name()))
			out[string(data)] = true
		}
	}
	return out
}

// 配额接口：报告用量与最小必要容量；不能设得比在用总量小；调小立即清理。
// 设备在用的已占满配额时，新上传当场拒收。
func TestCacheQuotaAPI(t *testing.T) {
	s, h := newAdminTestServer(t)
	smallGB(s, 1000)
	putMediaFile(t, s, "a.jpg", bytes.Repeat([]byte{1}, 1500))

	var st CacheStats
	json.Unmarshal(do(t, h, adminReq("GET", "/api/v1/admin/cache", nil), http.StatusOK).Body.Bytes(), &st)
	if st.QuotaGB != 16 || st.InUseBytes != 1500 || st.MinQuotaGB != 2 {
		t.Fatalf("默认配额 16、在用 1500 字节、最小 2：%+v", st)
	}
	w := do(t, h, adminReq("PUT", "/api/v1/admin/cache", map[string]int{"quota_gb": 1}), http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "至少 2GB") {
		t.Fatalf("应说明最小值：%s", w.Body.String())
	}
	do(t, h, adminReq("PUT", "/api/v1/admin/cache", map[string]int{"quota_gb": 2}), http.StatusNoContent)
	json.Unmarshal(do(t, h, adminReq("GET", "/api/v1/admin/cache", nil), http.StatusOK).Body.Bytes(), &st)
	if st.QuotaGB != 2 || state(s).CacheQuotaGB != 2 {
		t.Fatalf("配额应已保存：%+v", st)
	}

	putMediaFile(t, s, "b.jpg", bytes.Repeat([]byte{2}, 600)) // 在用 2100 ≥ 2000
	s.reconcileCache()
	res := parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"c.png", tinyPNG(t)}))
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Reason, "缓存区已被设备在用的文件占满") {
		t.Fatalf("在用已占满配额时应拒收：%+v", res)
	}
}

// PDF 复用时页面一起复用；另一台设备的清单展开出同样的页面。
func TestPDFReuseIncludesPages(t *testing.T) {
	s, h := newAdminTestServer(t)
	realPDF(t, s)
	doc := testpdf.Make(testpdf.A4Portrait, testpdf.A4Landscape)
	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"doc.pdf", doc}))
	waitMedia(t, h, "渲染完成", func(fs []MediaFile) bool { f, ok := byName(fs, "doc.pdf"); return ok && f.Status == mediaReady })

	res := parseUpload(t, uploadMedia(t, h, "dev-002", upload{"同一份.pdf", doc}))
	if strings.Join(res.Reused, ",") != "同一份.pdf" {
		t.Fatalf("同一 PDF 应直接复用：%+v", res)
	}
	m := manifestFor(t, h, "dev-002", "other-secret")
	if len(m.Items) != 2 || m.Items[1].Name != "同一份-p002.jpg" {
		t.Fatalf("复用的 PDF 应展开出页面：%+v", m.Items)
	}
	pages := manifest.Pages(s.deviceMediaDir("dev-002"), "同一份.pdf")
	if !sameFile(t, filepath.Join(s.deviceMediaDir(testDeviceID), manifest.PagesDir, "doc.pdf", pages[0]),
		filepath.Join(s.deviceMediaDir("dev-002"), manifest.PagesDir, "同一份.pdf", pages[0])) {
		t.Fatal("页面也应是同一份内容的硬链接")
	}
}

// 首次启动纳入存量文件：设备文件换成指向仓库的链接，内容与清单版本不变。
func TestCacheAdoptsExistingFilesWithoutChangingManifest(t *testing.T) {
	s, h := newAdminTestServer(t)
	putMediaFile(t, s, "a.jpg", []byte("existing image"))
	before := deviceManifest(t, h).Version
	s.reconcileCache()
	if s.cacheStats().Files != 1 {
		t.Fatalf("存量文件应纳入缓存区：%+v", s.cacheStats())
	}
	if after := deviceManifest(t, h).Version; after != before {
		t.Fatalf("纳入缓存区不应改变清单：%s → %s", before, after)
	}
}
