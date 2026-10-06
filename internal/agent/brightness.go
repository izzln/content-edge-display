package agent

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
)

// 分时段亮度：后台设若干时段与亮度（manifest.BrightnessPeriod），随响应头下发（schedule.go）。
// 设备按服务端时区自己判断此刻在哪一段、到点切换：不依赖轮询成功，断网照样按时调暗/恢复。
// 计划存进缓存目录，断网重启后也照旧。画面亮度由播放器在整屏最上层叠黑幕实现，时段还可以停播媒体区
// （player.SetBrightness）。

// brightnessCheck 是到点切换的检查间隔。
const brightnessCheck = 30 * time.Second

func (a *Agent) brightnessPath() string { return filepath.Join(a.cfg.CacheDir, "brightness") }

// 存盘格式两行：服务端时区、亮度计划（FormatBrightness）。时区一起存，是因为时段按它判断，
// 断网重启后还没联系上服务端时也要按它算。
func (a *Agent) brightnessFile() string {
	return a.sched.Zone() + "\n" + func() string { p, _ := a.sched.Brightness(); return manifest.FormatBrightness(p) }() + "\n"
}

// loadBrightness 读回上次存下的亮度计划与时区，作为联系上服务端之前的设置。
func (a *Agent) loadBrightness() {
	b, err := os.ReadFile(a.brightnessPath())
	if err != nil {
		return
	}
	zone, sched, _ := strings.Cut(strings.TrimSuffix(string(b), "\n"), "\n")
	periods, err := manifest.ParseBrightness(sched)
	if err != nil {
		return
	}
	a.brightSaved = string(b)
	a.sched.bright.Store(&periods)
	if zone != "" && a.sched.Zone() == "" {
		a.sched.zone.Store(&zone)
	}
}

// applyBrightness 按当前时间设播放器亮度；计划有变化时存盘。还没有计划（从没联系上服务端）时不动。
func (a *Agent) applyBrightness() {
	periods, ok := a.sched.Brightness()
	if !ok {
		return
	}
	if v := a.brightnessFile(); v != a.brightSaved {
		if err := fsutil.WriteFile(a.brightnessPath(), []byte(v), 0o644); err != nil {
			log.Printf("agent: %v", err)
		}
		a.brightSaved = v
	}
	now := a.clock.Now().In(a.localZone())
	pct, period := manifest.BrightnessAt(periods, now.Hour()*60+now.Minute())
	noMedia := period != nil && period.HideMedia
	if pct == a.brightness && noMedia == a.noMedia {
		return
	}
	a.brightness, a.noMedia = pct, noMedia
	a.player.SetBrightness(pct, !noMedia)
	media := ""
	if noMedia {
		media = ", media area off"
	}
	if period != nil {
		log.Printf("agent: brightness %d%%%s (period %s-%s)", pct, media, period.Start, period.End)
	} else {
		log.Printf("agent: brightness %d%%", pct)
	}
}
