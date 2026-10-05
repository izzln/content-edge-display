//go:build linux || darwin

package fsutil

import "syscall"

// DiskFree 返回 path 所在文件系统对普通用户可用的字节数，读不到时返回 -1。
func DiskFree(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
