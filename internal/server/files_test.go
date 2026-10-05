package server

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// 上传落进暂存区：边写边算 sha256；超限、中断都不留临时文件。
func TestReceive(t *testing.T) {
	s, _ := newTestServer(t)
	left := func() int { e, _ := os.ReadDir(s.incomingDir()); return len(e) }

	path, sha, n, err := s.receive(strings.NewReader("hello"), 5)
	if err != nil || n != 5 || sha != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("%v %d %s", err, n, sha)
	}
	os.Remove(path)
	if _, _, _, err := s.receive(strings.NewReader("hello!"), 5); !errors.Is(err, errTooLarge) {
		t.Fatalf("超限应报 errTooLarge：%v", err)
	}
	broken := io.MultiReader(strings.NewReader("hel"), iotest{io.ErrUnexpectedEOF})
	if _, _, _, err := s.receive(broken, 5); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("中断应原样报错：%v", err)
	}
	if n := left(); n != 0 {
		t.Fatalf("出错时不应留下临时文件：%d", n)
	}
}

type iotest struct{ err error }

func (r iotest) Read([]byte) (int, error) { return 0, r.err }

// pruneDir 只删 keep 不保留的，返回删掉的名字。
func TestPruneDir(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", ".c"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "b"), old, old)
	removed := pruneDir(dir, func(name string, info os.FileInfo) bool {
		return strings.HasPrefix(name, ".") || time.Since(info.ModTime()) < time.Minute && name != "d"
	})
	slices.Sort(removed)
	if strings.Join(removed, ",") != "b,d" {
		t.Fatalf("删掉的：%v", removed)
	}
	if e, _ := os.ReadDir(dir); len(e) != 2 {
		t.Fatalf("应剩 a 与 .c：%v", e)
	}
}
