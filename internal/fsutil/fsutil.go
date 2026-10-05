// Package fsutil 是服务端与设备端共用的文件操作：原子写入、复制、跨文件系统移动、硬链接。
//
// 一律"先写到同目录的临时文件、再改名"：读的一方要么看到旧文件要么看到完整的新文件，
// 中途断电或出错不会留下半截文件。
package fsutil

import (
	"io"
	"os"
	"path/filepath"
)

// tempName 是 dst 同目录下的隐藏临时文件名（隐藏文件不会被当成媒体或内容）。
func tempName(dst string) string {
	return filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp")
}

// WriteFile 原子且持久地写文件：临时文件 → fsync → 改名 → fsync 目录。
// 只改名不 fsync 的话，写完不久断电，文件可能是空的或根本不存在（设备没有 UPS）。
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return writeWith(path, perm, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// CopyFile 把 src 原子地复制到 dst。
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeWith(dst, 0o644, func(f *os.File) error {
		_, err := io.Copy(f, in)
		return err
	})
}

// MoveFile 把 src 移到 dst；不在同一个文件系统、改名不行时退回复制再删除。
func MoveFile(src, dst string) error {
	if os.Rename(src, dst) == nil {
		return nil
	}
	if err := CopyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// LinkFile 让 dst 成为 src 的硬链接（原子替换已有的 dst）；已经是同一个文件时什么都不做，
// 文件系统不支持硬链接时退回复制。
func LinkFile(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	tmp := tempName(dst)
	os.Remove(tmp)
	if err := link(src, tmp); err != nil {
		return CopyFile(src, dst)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

var link = os.Link // 测试替换成总失败的实现，验证退回复制

func writeWith(path string, perm os.FileMode, fill func(*os.File) error) error {
	tmp := tempName(path)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	err = fill(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	SyncDir(filepath.Dir(path))
	return nil
}

// SyncDir 把目录项（新建、改名）落盘。
func SyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
