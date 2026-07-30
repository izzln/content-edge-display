package manifest

import (
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

func TestBuildFromDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b_video.mp4", "video-bytes")
	writeFile(t, dir, "a_image.jpg", "image-bytes")
	writeFile(t, dir, "notes.txt", "ignored")
	writeFile(t, dir, ".hidden.jpg", "ignored")
	writeFile(t, dir, "c.mp4.part", "ignored")

	m, err := BuildFromDir(dir, "dev-001", 10, NewHashCache())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Items) != 2 {
		t.Fatalf("expected 2 items, got %d: %+v", len(m.Items), m.Items)
	}
	if m.Items[0].Name != "a_image.jpg" || m.Items[1].Name != "b_video.mp4" {
		t.Fatalf("wrong order: %+v", m.Items)
	}
	img, vid := m.Items[0], m.Items[1]
	if img.Type != "image" || img.Duration != 10 {
		t.Fatalf("image item wrong: %+v", img)
	}
	if vid.Type != "video" || vid.Duration != 0 {
		t.Fatalf("video item wrong: %+v", vid)
	}
	if img.URL != "/media/dev-001/a_image.jpg" {
		t.Fatalf("wrong url: %s", img.URL)
	}
	if len(img.SHA256) != 64 || img.ID != img.SHA256[:12] {
		t.Fatalf("bad sha/id: %+v", img)
	}
	if img.Size != int64(len("image-bytes")) {
		t.Fatalf("bad size: %d", img.Size)
	}
	if img.Order != 1 || vid.Order != 2 {
		t.Fatalf("bad order fields: %+v", m.Items)
	}
}

func TestVersionStableAndChanges(t *testing.T) {
	dir := t.TempDir()
	cache := NewHashCache()
	writeFile(t, dir, "a.jpg", "aaa")

	m1, err := BuildFromDir(dir, "d", 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := BuildFromDir(dir, "d", 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Version != m2.Version {
		t.Fatalf("version not stable: %s vs %s", m1.Version, m2.Version)
	}

	// 新增文件 → 版本变化
	writeFile(t, dir, "b.mp4", "bbb")
	m3, err := BuildFromDir(dir, "d", 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	if m3.Version == m1.Version {
		t.Fatal("version unchanged after adding a file")
	}

	// 内容修改（强制改 mtime 保证可感知）→ 版本变化
	writeFile(t, dir, "a.jpg", "AAA!")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "a.jpg"), future, future); err != nil {
		t.Fatal(err)
	}
	m4, err := BuildFromDir(dir, "d", 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	if m4.Version == m3.Version {
		t.Fatal("version unchanged after modifying a file")
	}
}

func TestEmptyOrMissingDir(t *testing.T) {
	m, err := BuildFromDir(filepath.Join(t.TempDir(), "no-such"), "d", 10, NewHashCache())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Items) != 0 || m.Version == "" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}
