package agent

import (
	"os"
	"path/filepath"
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

// 面板 EDID 首选 1080p、但也提供 1440x900：按配置用 1440x900（不指定就会按首选模式输出）。
func TestOutputModeUsesConfiguredModeWhenAvailable(t *testing.T) {
	fakeDRM(t, map[string][2]string{
		"card0-HDMI-A-1": {"connected", "1920x1080\n1440x900\n1280x720\n"},
	})
	if w, h := OutputMode("1440x900@60"); w != 1440 || h != 900 {
		t.Fatalf("got %dx%d", w, h)
	}
	if w, h := OutputMode(""); w != 1920 || h != 1080 {
		t.Fatalf("未配置时应用首选模式，得到 %dx%d", w, h)
	}
}

// 屏幕根本没有这个模式时退回首选模式：去设一个显示屏不认的模式只会黑屏。
func TestOutputModeFallsBackToPreferred(t *testing.T) {
	fakeDRM(t, map[string][2]string{
		"card0-HDMI-A-1": {"connected", "1920x1080\n1280x720\n"},
		"card0-HDMI-A-2": {"disconnected", "1440x900\n"}, // 没接的口不算
	})
	if w, h := OutputMode("1440x900@60"); w != 1920 || h != 1080 {
		t.Fatalf("模式不可用时应退回首选模式，得到 %dx%d", w, h)
	}
}

func TestOutputModeWithoutSysfs(t *testing.T) {
	old := drmSysfs
	drmSysfs = filepath.Join(t.TempDir(), "nope")
	defer func() { drmSysfs = old }()
	if w, h := OutputMode("1280x720"); w != 1280 || h != 720 {
		t.Fatalf("读不到 sysfs 时按配置值，得到 %dx%d", w, h)
	}
	if w, h := OutputMode(""); w != 1440 || h != 900 {
		t.Fatalf("未配置时按模板画布尺寸，得到 %dx%d", w, h)
	}
}

// 格式不对的 display_mode 配置加载时就要挡住，装机时就能发现。
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
