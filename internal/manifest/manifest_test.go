package manifest

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildDir 按目录里的文件（文件名顺序）构建条目。
func buildDir(t *testing.T, dir string, cache *HashCache) []Item {
	t.Helper()
	files, err := ListMedia(dir)
	if err != nil {
		t.Fatal(err)
	}
	items, err := BuildItems(dir, "d", Names(files), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestListMediaSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b_video.mp4", "video-bytes")
	writeFile(t, dir, "a_image.jpg", "image-bytes")
	writeFile(t, dir, "notes.txt", "ignored")
	writeFile(t, dir, ".hidden.jpg", "ignored") // 转码半成品等隐藏文件
	writeFile(t, dir, "c.mp4.part", "ignored")  // 上传中
	files, err := ListMedia(dir)
	if err != nil {
		t.Fatal(err)
	}
	if names := Names(files); len(names) != 2 || names[0] != "a_image.jpg" || names[1] != "b_video.mp4" || files[0].Size != 11 {
		t.Fatalf("got %+v", files)
	}
	if names, err := ListMedia(filepath.Join(dir, "no-such")); err != nil || len(names) != 0 {
		t.Fatalf("目录不存在应视为空：%v %v", names, err)
	}
}

func TestBuildItemsFollowsGivenOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.jpg", "image-bytes")
	writeFile(t, dir, "b.mp4", "video-bytes")

	// 按给定顺序；已不存在的、非法的名字跳过而不是让整份清单失败
	items, err := BuildItems(dir, "dev-001", []string{"b.mp4", "gone.jpg", "../x.jpg", "a.jpg"}, 7, NewHashCache())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name != "b.mp4" || items[1].Name != "a.jpg" {
		t.Fatalf("got %+v", items)
	}
	vid, img := items[0], items[1]
	if vid.Type != "video" || vid.Duration != 0 {
		t.Fatalf("视频不带停留时长：%+v", vid)
	}
	if img.Type != "image" || img.Duration != 7 {
		t.Fatalf("图片带上给定的停留时长：%+v", img)
	}
	if img.URL != "/media/dev-001/a.jpg" || len(img.SHA256) != 64 || img.Size != int64(len("image-bytes")) {
		t.Fatalf("条目字段错误：%+v", img)
	}
}

// 文档在播放列表里是一项，下发给设备时按页序展开成图片条目（与图片同样的停留时长）；
// 还没渲染完（页面目录不存在）的文档不下发。
func TestBuildItemsExpandsDocuments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.jpg", "image-bytes")
	writeFile(t, dir, "季度报告.pdf", "pdf-bytes")
	writeFile(t, dir, "rendering.pdf", "pdf-bytes")
	pages := filepath.Join(dir, PagesDir, "季度报告.pdf")
	os.MkdirAll(pages, 0o755)
	for _, p := range []string{"p002.jpg", "p001.jpg", "p010.jpg"} {
		writeFile(t, pages, p, "page-"+p)
	}

	if TypeOf("x.PDF") != "document" {
		t.Fatal(".pdf 应识别为文档")
	}
	if names, _ := ListMedia(dir); len(names) != 3 {
		t.Fatalf("页面目录不能被当成媒体：%v", names)
	}
	items, err := BuildItems(dir, "dev 1", []string{"季度报告.pdf", "rendering.pdf", "a.jpg"}, 7, NewHashCache())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Name)
		if it.Type != "image" || it.Duration != 7 {
			t.Fatalf("文档页面应是带停留时长的图片：%+v", it)
		}
	}
	want := []string{"季度报告-p001.jpg", "季度报告-p002.jpg", "季度报告-p010.jpg", "a.jpg"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("展开顺序 %v，期望 %v", got, want)
	}
	if items[0].URL != "/media/dev%201/"+url.PathEscape("季度报告.pdf")+"/p001.jpg" {
		t.Fatalf("页面 URL：%s", items[0].URL)
	}
}

func TestVersionStableAndChanges(t *testing.T) {
	dir := t.TempDir()
	cache := NewHashCache()
	writeFile(t, dir, "a.jpg", "aaa")

	v1 := Version(Manifest{Items: buildDir(t, dir, cache)})
	if v2 := Version(Manifest{Items: buildDir(t, dir, cache)}); v1 != v2 {
		t.Fatalf("version not stable: %s vs %s", v1, v2)
	}

	// 新增文件 → 版本变化
	writeFile(t, dir, "b.mp4", "bbb")
	v3 := Version(Manifest{Items: buildDir(t, dir, cache)})
	if v3 == v1 {
		t.Fatal("version unchanged after adding a file")
	}

	// 内容修改（强制改 mtime 保证可感知）→ 版本变化
	writeFile(t, dir, "a.jpg", "AAA!")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "a.jpg"), future, future); err != nil {
		t.Fatal(err)
	}
	items := buildDir(t, dir, cache)
	v4 := Version(Manifest{Items: items})
	if v4 == v3 {
		t.Fatal("version unchanged after modifying a file")
	}

	// 只改停留时长、只加指令、只改叠加布局 —— 都必须改变版本号，否则设备一直收到 304
	items[0].Duration++
	if Version(Manifest{Items: items}) == v4 {
		t.Fatal("停留时长变化必须改变版本号")
	}
	items[0].Duration--
	if Version(Manifest{Items: items, Update: &Update{Version: "2"}}) == v4 {
		t.Fatal("指令出现必须改变版本号")
	}
	l := &Layout{CanvasW: 1440, CanvasH: 900, Media: Rect{720, 0, 720, 900}, Overlay: Item{SHA256: "x"}}
	withLayout := Version(Manifest{Items: items, Layout: l})
	if withLayout == v4 {
		t.Fatal("叠加布局出现必须改变版本号")
	}
	l.Media.X = 0
	if Version(Manifest{Items: items, Layout: l}) == withLayout {
		t.Fatal("媒体区位置变化必须改变版本号")
	}
	withAccess := Version(Manifest{Items: items, Access: &Access{SSHKeys: []string{"ssh-ed25519 AAAA a"}}})
	if withAccess == v4 || Version(Manifest{Items: items, Access: &Access{SSHKeys: []string{"ssh-ed25519 AAAA b"}}}) == withAccess {
		t.Fatal("访问凭据出现或变化必须改变版本号")
	}
}

func TestDownloadsIncludeOverlay(t *testing.T) {
	m := &Manifest{Items: []Item{{Name: "a.mp4"}}}
	if len(m.Downloads()) != 1 {
		t.Fatal("没有叠加布局时只下载播放条目")
	}
	m.Layout = &Layout{Overlay: Item{Name: "ovl.png"}}
	if d := m.Downloads(); len(d) != 2 || d[1].Name != "ovl.png" {
		t.Fatalf("有叠加布局时还要下载叠加图：%+v", d)
	}
}

// 维护时丢掉已经不在的文件的哈希缓存。
func TestHashCachePrune(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.jpg", "a")
	writeFile(t, dir, "b.jpg", "b")
	c := NewHashCache()
	c.Sum(filepath.Join(dir, "a.jpg"))
	c.Sum(filepath.Join(dir, "b.jpg"))
	os.Remove(filepath.Join(dir, "a.jpg"))
	c.Prune()
	if _, ok := c.m[filepath.Join(dir, "a.jpg")]; ok || len(c.m) != 1 {
		t.Fatalf("应只剩还在的文件：%v", c.m)
	}
}
