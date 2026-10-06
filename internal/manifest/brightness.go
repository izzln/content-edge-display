package manifest

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// 分时段亮度：后台设若干时段，每段一个亮度（如 22:00–07:00 30%），不在任何时段内是 100%。
// 设置统一下发给所有设备（响应头 HeaderBrightness，与轮询间隔、时区同一条路），
// 设备按服务端时区自己判断此刻在哪一段——到点切换不依赖网络。

// HeaderBrightness 是亮度计划：FormatBrightness 的结果，没有时段时为 "none"。头缺失时设备保持现状。
const HeaderBrightness = "X-Brightness"

// 亮度取值：10%~100%，每档 10%。最低 10%：全黑会被当成死机。
const (
	MinBrightness        = 10
	BrightnessStep       = 10
	MaxBrightnessPeriods = 8
)

// BrightnessPeriod 是一个亮度时段：[Start, End)，"HH:MM"，End 早于 Start 表示跨午夜。
type BrightnessPeriod struct {
	Start   string `json:"start"`
	End     string `json:"end"`
	Percent int    `json:"percent"`
}

// minuteOfDay 解析 "HH:MM"。
func minuteOfDay(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || len(h) != 2 || len(m) != 2 || err1 != nil || err2 != nil || hh > 23 || mm > 59 || hh < 0 || mm < 0 {
		return 0, fmt.Errorf("时间 %q 应为 HH:MM", s)
	}
	return hh*60 + mm, nil
}

// covers 判断这一段是否包含一天中的第 m 分钟（起点包含、终点不包含）。时间已校验过。
func (p BrightnessPeriod) covers(m int) bool {
	s, _ := minuteOfDay(p.Start)
	e, _ := minuteOfDay(p.End)
	if s < e {
		return m >= s && m < e
	}
	return m >= s || m < e // 跨午夜
}

// ValidateBrightness 校验亮度计划并按起始时间排好序：时间格式、起止不同、亮度档位、时段不重叠、段数上限。
func ValidateBrightness(periods []BrightnessPeriod) error {
	if len(periods) > MaxBrightnessPeriods {
		return fmt.Errorf("亮度时段最多 %d 个", MaxBrightnessPeriods)
	}
	for _, p := range periods {
		s, err := minuteOfDay(p.Start)
		if err != nil {
			return err
		}
		e, err := minuteOfDay(p.End)
		if err != nil {
			return err
		}
		if s == e {
			return fmt.Errorf("时段 %s-%s 的起止时间不能相同", p.Start, p.End)
		}
		if p.Percent < MinBrightness || p.Percent > 100 || p.Percent%BrightnessStep != 0 {
			return fmt.Errorf("亮度须为 %d%%~100%%、%d%% 的整数倍", MinBrightness, BrightnessStep)
		}
	}
	for m := 0; m < 24*60; m++ {
		var hit []BrightnessPeriod
		for _, p := range periods {
			if p.covers(m) {
				hit = append(hit, p)
			}
		}
		if len(hit) > 1 {
			return fmt.Errorf("时段 %s-%s 与 %s-%s 重叠", hit[0].Start, hit[0].End, hit[1].Start, hit[1].End)
		}
	}
	slices.SortFunc(periods, func(a, b BrightnessPeriod) int { return strings.Compare(a.Start, b.Start) })
	return nil
}

// BrightnessAt 返回一天中第 m 分钟的亮度（百分比）及所在时段；不在任何时段内是 100、nil。
func BrightnessAt(periods []BrightnessPeriod, m int) (int, *BrightnessPeriod) {
	for i := range periods {
		if periods[i].covers(m) {
			return periods[i].Percent, &periods[i]
		}
	}
	return 100, nil
}

// FormatBrightness 把亮度计划写成响应头的值："18:00-22:00 70;22:00-07:00 30"，没有时段为 "none"。
func FormatBrightness(periods []BrightnessPeriod) string {
	if len(periods) == 0 {
		return "none"
	}
	parts := make([]string, len(periods))
	for i, p := range periods {
		parts[i] = fmt.Sprintf("%s-%s %d", p.Start, p.End, p.Percent)
	}
	return strings.Join(parts, ";")
}

// ParseBrightness 解析 FormatBrightness 的结果，并校验（服务端配错了也不能把设备带偏）。
func ParseBrightness(v string) ([]BrightnessPeriod, error) {
	v = strings.TrimSpace(v)
	if v == "none" {
		return nil, nil
	}
	if v == "" {
		return nil, errors.New("empty brightness schedule")
	}
	var out []BrightnessPeriod
	for _, part := range strings.Split(v, ";") {
		span, pct, ok := strings.Cut(strings.TrimSpace(part), " ")
		start, end, ok2 := strings.Cut(span, "-")
		n, err := strconv.Atoi(pct)
		if !ok || !ok2 || err != nil {
			return nil, fmt.Errorf("bad brightness period %q", part)
		}
		out = append(out, BrightnessPeriod{Start: start, End: end, Percent: n})
	}
	if err := ValidateBrightness(out); err != nil {
		return nil, err
	}
	return out, nil
}
