package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeDRM 在临时目录里造一份 /sys/class/drm 的连接器信息。
func fakeDRM(t *testing.T, connectors map[string][2]string) {
	t.Helper()
	root := t.TempDir()
	for name, v := range connectors {
		d := filepath.Join(root, name)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "status"), []byte(v[0]+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "modes"), []byte(v[1]), 0o644)
	}
	old := drmSysfs
	drmSysfs = root
	t.Cleanup(func() { drmSysfs = old })
}

// 面板 EDID 首选 1080p、但也提供 1440x900：要显式让 mpv 用 1440x900（mpv 默认用首选模式）。
func TestDRMModeArgsForcesAvailableMode(t *testing.T) {
	fakeDRM(t, map[string][2]string{
		"card0-HDMI-A-1": {"connected", "1920x1080\n1440x900\n1280x720\n"},
	})
	if got := DRMModeArgs("1440x900@60"); !reflect.DeepEqual(got, []string{"--drm-mode=1440x900@60"}) {
		t.Fatalf("got %v", got)
	}
}

// 屏幕根本没有这个模式时绝不能传：mpv 找不到模式会初始化失败，设备黑屏。
func TestDRMModeArgsSkipsUnavailableMode(t *testing.T) {
	fakeDRM(t, map[string][2]string{
		"card0-HDMI-A-1": {"connected", "1920x1080\n1280x720\n"},
		"card0-HDMI-A-2": {"disconnected", "1440x900\n"}, // 没接的口不算
	})
	if got := DRMModeArgs("1440x900@60"); got != nil {
		t.Fatalf("模式不可用时不应强制，得到 %v", got)
	}
}

func TestDRMModeArgsWithoutSysfs(t *testing.T) {
	old := drmSysfs
	drmSysfs = filepath.Join(t.TempDir(), "nope")
	defer func() { drmSysfs = old }()
	if got := DRMModeArgs("1440x900"); got != nil {
		t.Fatalf("读不到 sysfs 时不应强制，得到 %v", got)
	}
	if got := DRMModeArgs(""); got != nil {
		t.Fatalf("未配置时不应强制，得到 %v", got)
	}
}

// 格式不对 mpv 会拒绝启动，配置加载时就要挡住。
func TestDisplayModeValidated(t *testing.T) {
	for mode, ok := range map[string]bool{
		"1440x900": true, "1440x900@60": true, "1920x1080@59.94": true, "": true,
		"1440*900": false, "big": false, "1440x900@": false, "0x0": false,
	} {
		c := &Config{ServerURL: "http://x", EnrollToken: "t", DisplayMode: mode}
		if err := c.fillDefaults(); (err == nil) != ok {
			t.Errorf("display_mode %q: err=%v，期望 ok=%v", mode, err, ok)
		}
	}
}
