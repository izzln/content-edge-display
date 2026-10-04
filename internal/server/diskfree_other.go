//go:build !linux && !darwin

package server

// diskFree 在不支持的平台上读不到，返回 -1。
func diskFree(string) int64 { return -1 }
