package agentpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	mode       int64
	typ        byte
}

func makeTarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: typ, Linkname: "/etc/passwd"}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractStripsTopDirAndKeepsExecBit(t *testing.T) {
	pkg := makeTarGz(t,
		entry{name: "display-agent-1.2.0/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "display-agent-1.2.0/VERSION", body: "1.2.0\n", mode: 0o644},
		entry{name: "display-agent-1.2.0/update.sh", body: "#!/bin/sh\n", mode: 0o755},
		entry{name: "display-agent-1.2.0/install-agent.sh", body: "#!/bin/sh\n", mode: 0o755},
		entry{name: "display-agent-1.2.0/display-agent", body: "bin", mode: 0o755},
		entry{name: "display-agent-1.2.0/sub/x.txt", body: "x", mode: 0o644},
		entry{name: "display-agent-1.2.0/../../escape.txt", body: "nope", mode: 0o644}, // 想跳出解包目录
	)
	dir := t.TempDir()
	if err := Extract(bytes.NewReader(pkg), dir); err != nil {
		t.Fatal(err)
	}
	if v, err := Check(dir); err != nil || v != "1.2.0" {
		t.Fatalf("版本：%q %v", v, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "update.sh")); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("update.sh 应保留可执行权限：%v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "x.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("包里的 .. 路径不能写到解包目录之外")
	}
}

func TestExtractRejectsLinksAndGarbage(t *testing.T) {
	pkg := makeTarGz(t, entry{name: "p/evil", typ: tar.TypeSymlink})
	if err := Extract(bytes.NewReader(pkg), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsupported entry") {
		t.Fatalf("包里的符号链接应被拒绝：%v", err)
	}
	if err := Extract(strings.NewReader("not a package"), t.TempDir()); err == nil {
		t.Fatal("不是 gzip 应报错")
	}
}

func TestCheckRequiresFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Check(dir); err == nil || !strings.Contains(err.Error(), VersionFile) {
		t.Fatalf("缺 VERSION 应报错：%v", err)
	}
	os.WriteFile(filepath.Join(dir, VersionFile), []byte("../x\n"), 0o644)
	if _, err := Check(dir); err == nil {
		t.Fatal("非法版本号应报错")
	}
	os.WriteFile(filepath.Join(dir, VersionFile), []byte("2.0.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, Binary), nil, 0o755)
	if _, err := Check(dir); err == nil || !strings.Contains(err.Error(), UpdateScript) {
		t.Fatalf("缺 update.sh 应报错：%v", err)
	}
	os.WriteFile(filepath.Join(dir, UpdateScript), nil, 0o755)
	if _, err := Check(dir); err == nil || !strings.Contains(err.Error(), InstallScript) {
		t.Fatalf("缺 install-agent.sh 应报错（最新的包也是装机包）：%v", err)
	}
}

// 压缩炸弹：声明的解包总大小超过上限，读到头部就拒绝，不往磁盘写。
func TestExtractRejectsHugePackage(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "p/huge", Mode: 0o644, Size: MaxExtracted + 1, Typeflag: tar.TypeReg})
	tw.Flush()
	gz.Close()
	if err := Extract(&buf, t.TempDir()); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("超过解包上限应拒绝：%v", err)
	}
}
