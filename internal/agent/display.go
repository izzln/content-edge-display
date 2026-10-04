package agent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// drmSysfs 是 DRM 连接器信息所在目录（测试可替换）。
var drmSysfs = "/sys/class/drm"

// OutputMode 决定显示输出分辨率：播放进程按它设置显示模式，叠加图也按它光栅化。
//
// /boot/armbianEnv.txt 里的 video=HDMI-A-1:1440x900@60 只管内核控制台；程序自己设置显示模式时，
// 不指定就只能用 EDID 的"首选模式"——面板报 1920×1080，就按 1080p 输出，跟模板画布对不上。
// 所以按 display_mode 选模式；显示屏不提供这个模式时退回首选模式（叠加图与媒体区会按实际分辨率
// 换算，画面不会错位），而不是去设一个显示屏不认的模式、落得黑屏。
func OutputMode(mode string) (w, h int) {
	available, err := connectorModes()
	switch {
	case err != nil || len(available) == 0:
		// 读不到 sysfs（非 Linux / 容器 / 开发机）：按配置值，没配就用模板画布尺寸
		if mode == "" {
			return manifest.CanvasW, manifest.CanvasH
		}
	case mode == "" || slices.Contains(available, mode):
		if mode == "" {
			mode = available[0] // 内核把首选模式排在最前
		}
	default:
		log.Printf("agent: the display does not offer mode %s (available: %s); using the preferred mode %s "+
			"and scaling the overlay. To force the mode, append ,e to the video= parameter in /boot/armbianEnv.txt and reboot",
			mode, strings.Join(available, " "), available[0])
		mode = available[0]
	}
	fmt.Sscanf(mode, "%dx%d", &w, &h)
	return w, h
}

// connectorModes 列出已连接的显示接口所提供的分辨率（去重，保持内核给出的顺序）。
func connectorModes() ([]string, error) {
	dirs, err := filepath.Glob(filepath.Join(drmSysfs, "card*-*"))
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no drm connectors under %s", drmSysfs)
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		if status, err := os.ReadFile(filepath.Join(d, "status")); err == nil &&
			strings.TrimSpace(string(status)) != "connected" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d, "modes"))
		if err != nil {
			continue
		}
		for _, line := range strings.Fields(string(data)) {
			// 内核可能给隔行模式加 i 后缀（1920x1080i），只取逐行模式
			if !seen[line] && !strings.HasSuffix(line, "i") {
				seen[line] = true
				out = append(out, line)
			}
		}
	}
	return out, nil
}
