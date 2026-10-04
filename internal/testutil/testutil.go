// Package testutil 是各包测试共用的小工具。
package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
	"time"
)

// WaitFor 每 10ms 检查一次 cond，timeout 内不成立就失败。
func WaitFor(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// File 是程序包里的一个文件；Exec 为真时带可执行权限。
type File struct {
	Name, Body string
	Exec       bool
}

// Package 按 make package 的结构（顶层目录 display-agent-<版本>/）打一个 tar.gz 程序包。
func Package(version string, files ...File) []byte { return TarGz("display-agent-"+version, files...) }

// TarGz 把 files 放在顶层目录 top/ 下打成 tar.gz。
func TarGz(top string, files ...File) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		mode := int64(0o644)
		if f.Exec {
			mode = 0o755
		}
		tw.WriteHeader(&tar.Header{Name: top + "/" + f.Name, Mode: mode,
			Size: int64(len(f.Body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.Body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}
