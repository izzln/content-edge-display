package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteCopyMoveLink(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := WriteFile(a, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(a, []byte("two"), 0o600); err != nil || read(t, a) != "two" {
		t.Fatalf("覆盖写入：%v", err)
	}
	if fi, _ := os.Stat(a); fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600，得到 %v", fi.Mode().Perm())
	}

	b := filepath.Join(dir, "b")
	if err := CopyFile(a, b); err != nil || read(t, b) != "two" {
		t.Fatalf("复制：%v", err)
	}
	c := filepath.Join(dir, "c")
	if err := MoveFile(b, c); err != nil || read(t, c) != "two" {
		t.Fatalf("移动：%v", err)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatal("移动后源文件应不存在")
	}

	l := filepath.Join(dir, "l")
	os.WriteFile(l, []byte("old"), 0o644)
	if err := LinkFile(a, l); err != nil {
		t.Fatal(err)
	}
	fa, _ := os.Stat(a)
	fl, _ := os.Stat(l)
	if !os.SameFile(fa, fl) {
		t.Fatal("LinkFile 应替换成硬链接")
	}
	if err := LinkFile(a, l); err != nil {
		t.Fatalf("已是同一文件时应直接成功：%v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Fatalf("不应残留临时文件 %s", e.Name())
		}
	}
}

// 文件系统不支持硬链接（如某些网络盘、FAT）时退回复制。
func TestLinkFileFallsBackToCopy(t *testing.T) {
	old := link
	link = func(string, string) error { return os.ErrPermission }
	defer func() { link = old }()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte("x"), 0o644)
	if err := LinkFile(a, b); err != nil || read(t, b) != "x" {
		t.Fatalf("应退回复制：%v", err)
	}
}
