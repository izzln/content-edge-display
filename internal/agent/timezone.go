package agent

import (
	"cmp"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
)

// 设备的系统时区跟随服务端（随响应头下发，见 schedule.go）。系统时钟已按服务端校准（clock.go），
// 时区也一致后，设备上的 date、journalctl、救援屏显示的时间都与服务端、后台看到的一样。
// Armbian 装好默认是 UTC。

// zoneTarget 是系统时区文件的位置（测试里换成临时目录）。
type zoneTarget struct {
	localtime    string // 指向 zoneinfo 里某个文件的符号链接，systemd、glibc 都读它
	timezoneFile string // Debian 旧版还有的纯文本时区名称：有就一起改，没有不建
	zoneinfo     string // tzdata 的目录
}

var systemZone = zoneTarget{localtime: "/etc/localtime", timezoneFile: "/etc/timezone", zoneinfo: "/usr/share/zoneinfo"}

// zoneNamePattern 是可接受的时区名称（Asia/Shanghai、Etc/GMT+8）：不许 .. 之类跑出 zoneinfo 目录。
var zoneNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

// current 返回当前系统时区的名称：/etc/localtime 链接到 zoneinfo 下的哪个文件。不是链接时为空。
func (t zoneTarget) current() string {
	target, err := os.Readlink(t.localtime)
	if err != nil {
		return ""
	}
	if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
		return name
	}
	return ""
}

// apply 把系统时区设为 name：先在旁边建好新链接再改名覆盖，/etc/localtime 任何时刻都是完整的。
// 不用 timedatectl：省掉对 dbus、systemd-timedated 的依赖，效果一样（systemd、journalctl 每次都读 /etc/localtime）。
func (t zoneTarget) apply(name string) error {
	if !zoneNamePattern.MatchString(name) {
		return fmt.Errorf("invalid time zone name %q", name)
	}
	file := filepath.Join(t.zoneinfo, name)
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("time zone %s: %w (is tzdata installed?)", name, err)
	}
	tmp := t.localtime + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(file, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.localtime); err != nil {
		os.Remove(tmp)
		return err
	}
	if _, err := os.Stat(t.timezoneFile); err == nil {
		return fsutil.WriteFile(t.timezoneFile, []byte(name+"\n"), 0o644)
	}
	return nil
}

// applyZone 把系统时区设成服务端的；已经一样就什么都不做。失败（如缺 tzdata）隔一会儿再试，不影响播放。
func (a *Agent) applyZone() {
	want := a.sched.Zone()
	if want == "" || a.zoneSys.localtime == "" || !a.zoneRetry.due() {
		return
	}
	was := a.zoneSys.current()
	if was != want {
		if err := a.zoneSys.apply(want); err != nil {
			a.zoneRetry.fail(err.Error())
			log.Printf("agent: cannot set the time zone: %v (retry in %s)", err, accessRetryInterval)
			return
		}
		log.Printf("agent: time zone set to %s by server (was %s)", want, cmp.Or(was, "unknown"))
	}
	a.zoneRetry.clear()
	if loc := a.zone.Load(); loc == nil || loc.String() != want {
		if loc, err := time.LoadLocation(want); err == nil {
			a.zone.Store(loc)
		}
	}
}

// localZone 是显示时间、判断亮度时段用的时区：服务端规定的那个，还没学到时用进程启动时的系统时区。
// 不依赖系统时区设没设成功（时区数据已编进程序，见 cmd/display-agent）。
func (a *Agent) localZone() *time.Location {
	if loc := a.zone.Load(); loc != nil && loc.String() == a.sched.Zone() {
		return loc
	}
	if z := a.sched.Zone(); z != "" {
		if loc, err := time.LoadLocation(z); err == nil {
			a.zone.Store(loc)
			return loc
		}
	}
	if loc := a.zone.Load(); loc != nil {
		return loc
	}
	return time.Local
}
