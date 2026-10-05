//go:build !linux && !darwin

package fsutil

// DiskFree 在不支持的平台上读不到，返回 -1。
func DiskFree(string) int64 { return -1 }
