package server

import (
	"os"
	"strings"
	"time"
)

// zoneFiles 是系统记录时区的位置（测试里换成临时文件）。
type zoneFiles struct{ localtime, timezone string }

var systemZoneFiles = zoneFiles{localtime: "/etc/localtime", timezone: "/etc/timezone"}

// zoneName 返回服务端时区的 IANA 名称，下发给设备设成系统时区（manifest.HeaderTimezone）。
// server.json 设了 timezone 就用它；没设时用系统时区——time.Local 只知道自己叫 "Local"，
// 名称要另外找：TZ 环境变量、/etc/localtime 链接到的 zoneinfo 文件（Linux、macOS 都是）、/etc/timezone。
// 都找不到返回空串（设备保持自己的时区）。
func zoneName(configured string, f zoneFiles) string {
	if configured != "" {
		return configured
	}
	valid := func(name string) bool {
		name = strings.TrimSpace(name)
		if name == "" || name == "Local" {
			return false
		}
		_, err := time.LoadLocation(name)
		return err == nil
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); valid(tz) {
		return strings.TrimSpace(tz)
	}
	if target, err := os.Readlink(f.localtime); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && valid(name) {
			return name
		}
	}
	if b, err := os.ReadFile(f.timezone); err == nil && valid(string(b)) {
		return strings.TrimSpace(string(b))
	}
	return ""
}
