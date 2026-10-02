package agent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// displayModePattern 是 mpv --drm-mode 接受的 WxH[@R] 格式。
// 格式不对 mpv 会直接拒绝启动（设备就会黑屏并反复重启），所以配置加载时先挡住。
var displayModePattern = regexp.MustCompile(`^[1-9][0-9]{2,4}x[1-9][0-9]{2,4}(@[0-9]{2,3}(\.[0-9]+)?)?$`)

// drmSysfs 是 DRM 连接器信息所在目录（测试可替换）。
var drmSysfs = "/sys/class/drm"

// DRMModeArgs 返回让 mpv 以指定分辨率输出的参数。
//
// 为什么需要它：/boot/armbianEnv.txt 里的 video=HDMI-A-1:1440x900@60 只管内核控制台，
// mpv 走 DRM 输出时默认用的是 EDID 的"首选模式"（--drm-mode=preferred）——
// 面板 EDID 报 1920×1080，mpv 就按 1080p 输出，跟 1440×900 的模板对不上。
// 所以要再显式告诉 mpv 用哪个模式。
//
// 只在连接器确实提供这个模式时才传：mpv 找不到指定模式时视频输出初始化失败，
// 屏幕会黑掉——宁可按首选模式输出（叠加图会按实际分辨率缩放，画面不会错位），也不能黑屏。
func DRMModeArgs(mode string) []string {
	if mode == "" {
		return nil
	}
	wh, _, _ := strings.Cut(mode, "@")
	available, err := connectorModes()
	if err != nil {
		// 读不到 sysfs（非 Linux / 容器 / X11 调试环境）：不强制，交给 mpv 默认行为
		return nil
	}
	for _, m := range available {
		if m == wh {
			return []string{"--drm-mode=" + mode}
		}
	}
	log.Printf("agent: the display does not offer mode %s (available: %s); using the EDID preferred mode "+
		"and scaling the overlay. To force the mode, append ,e to the video= parameter in /boot/armbianEnv.txt and reboot",
		wh, strings.Join(available, " "))
	return nil
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
