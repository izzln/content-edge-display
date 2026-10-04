//go:build linux || darwin

package server

import "syscall"

// diskFree 返回 path 所在文件系统对普通用户可用的字节数。
func diskFree(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
