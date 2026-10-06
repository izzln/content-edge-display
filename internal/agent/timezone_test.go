package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/server"
)

// fakeZone 在临时目录里搭一份 /etc 与 zoneinfo（测试绝不能改到本机的时区）。
func fakeZone(t *testing.T, zones ...string) zoneTarget {
	dir := t.TempDir()
	z := zoneTarget{localtime: filepath.Join(dir, "etc", "localtime"), timezoneFile: filepath.Join(dir, "etc", "timezone"),
		zoneinfo: filepath.Join(dir, "zoneinfo")}
	for _, name := range append(zones, "Etc/UTC") {
		os.MkdirAll(filepath.Dir(filepath.Join(z.zoneinfo, name)), 0o755)
		os.WriteFile(filepath.Join(z.zoneinfo, name), []byte("TZif"), 0o644)
	}
	os.MkdirAll(filepath.Dir(z.localtime), 0o755)
	os.Symlink(filepath.Join(z.zoneinfo, "Etc/UTC"), z.localtime) // Armbian 装好默认 UTC
	return z
}

// 设备的系统时区跟随服务端：随响应头到达，改 /etc/localtime；已一致就不动；救援屏按它显示时间。
func TestZoneFollowsServer(t *testing.T) {
	e := newEnv(t, true, func(c *server.Config) { c.Timezone = "Asia/Shanghai" })
	e.a.zoneSys = fakeZone(t, "Asia/Shanghai")
	ctx := context.Background()
	if err := e.a.step(ctx); err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(e.a.zoneSys.localtime); target != filepath.Join(e.a.zoneSys.zoneinfo, "Asia/Shanghai") {
		t.Fatalf("/etc/localtime 应指向服务端的时区：%q", target)
	}
	if _, err := os.Stat(e.a.zoneSys.timezoneFile); !os.IsNotExist(err) {
		t.Fatal("系统本来没有 /etc/timezone 时不应新建")
	}
	if e.a.localZone().String() != "Asia/Shanghai" {
		t.Fatalf("救援屏应按服务端时区显示时间：%s", e.a.localZone())
	}
	at := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	e.a.link.okAt = at
	if info := e.a.rescueInfo(at, nil, time.Minute); !strings.Contains(info, "2026-10-06 09:02:03") {
		t.Fatalf("救援屏的时间应是东八区：\n%s", info)
	}

	// 已一致：不再改（链接的修改时间不变）
	fi, _ := os.Lstat(e.a.zoneSys.localtime)
	time.Sleep(10 * time.Millisecond)
	e.a.step(ctx)
	if fi2, _ := os.Lstat(e.a.zoneSys.localtime); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatal("时区没变时不应重写 /etc/localtime")
	}
}

// 旧系统有 /etc/timezone 时一起改；名称不合法或缺 tzdata 时拒绝、不动现有设置。
func TestZoneApply(t *testing.T) {
	z := fakeZone(t, "Asia/Tokyo")
	os.WriteFile(z.timezoneFile, []byte("Etc/UTC\n"), 0o644)
	if err := z.apply("Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(z.timezoneFile); string(b) != "Asia/Tokyo\n" || z.current() != "Asia/Tokyo" {
		t.Fatalf("应同时改 /etc/timezone 与 /etc/localtime：%q %q", b, z.current())
	}
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "Asia/Nowhere", ""} {
		if err := z.apply(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
		if z.current() != "Asia/Tokyo" {
			t.Fatalf("拒绝 %q 后不应改动现有时区：%q", bad, z.current())
		}
	}
	if _, err := os.Lstat(z.localtime + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("不应留下临时链接")
	}
}

// 失败（缺 tzdata）隔一会儿再试，不影响轮询。
func TestZoneRetry(t *testing.T) {
	e := newEnv(t, true, func(c *server.Config) { c.Timezone = "Asia/Shanghai" })
	e.a.zoneSys = fakeZone(t) // zoneinfo 里没有 Asia/Shanghai
	if err := e.a.step(context.Background()); err != nil {
		t.Fatalf("设时区失败不应让轮询失败：%v", err)
	}
	if e.a.zoneRetry.err == "" || e.a.zoneSys.current() != "Etc/UTC" {
		t.Fatalf("应记下失败、保持原时区：%q %q", e.a.zoneRetry.err, e.a.zoneSys.current())
	}
	os.MkdirAll(filepath.Join(e.a.zoneSys.zoneinfo, "Asia"), 0o755)
	os.WriteFile(filepath.Join(e.a.zoneSys.zoneinfo, "Asia/Shanghai"), []byte("TZif"), 0o644)
	e.a.step(context.Background())
	if e.a.zoneSys.current() != "Etc/UTC" {
		t.Fatal("重试间隔没到时不应再试")
	}
	e.a.zoneRetry.at = time.Time{}
	e.a.step(context.Background())
	if e.a.zoneSys.current() != "Asia/Shanghai" || e.a.zoneRetry.err != "" {
		t.Fatalf("重试应成功：%q %q", e.a.zoneSys.current(), e.a.zoneRetry.err)
	}
}
