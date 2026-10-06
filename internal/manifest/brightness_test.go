package manifest

import (
	"strings"
	"testing"
)

func TestBrightnessSchedule(t *testing.T) {
	p := []BrightnessPeriod{{Start: "22:00", End: "07:00", Percent: 30}, {Start: "18:00", End: "22:00", Percent: 70}}
	if err := ValidateBrightness(p); err != nil {
		t.Fatal(err)
	}
	if p[0].Start != "18:00" {
		t.Fatalf("应按起始时间排序：%+v", p)
	}
	at := func(hm string) int {
		m, _ := minuteOfDay(hm)
		v, _ := BrightnessAt(p, m)
		return v
	}
	for hm, want := range map[string]int{"17:59": 100, "18:00": 70, "21:59": 70, "22:00": 30, "23:59": 30, "00:00": 30, "06:59": 30, "07:00": 100, "12:00": 100} {
		if got := at(hm); got != want {
			t.Errorf("%s：亮度应为 %d，得到 %d", hm, want, got)
		}
	}
	if v, per := BrightnessAt(nil, 100); v != 100 || per != nil {
		t.Fatal("没有时段时应是 100")
	}

	h := FormatBrightness(p)
	if h != "18:00-22:00 70;22:00-07:00 30" {
		t.Fatalf("响应头格式：%q", h)
	}
	back, err := ParseBrightness(h)
	if err != nil || FormatBrightness(back) != h {
		t.Fatalf("往返不一致：%v %+v", err, back)
	}
	if FormatBrightness(nil) != "none" {
		t.Fatal("没有时段应写 none")
	}
	if got, err := ParseBrightness("none"); err != nil || got != nil {
		t.Fatalf("none 应解析为空：%v %v", got, err)
	}

	for _, c := range []struct {
		p    []BrightnessPeriod
		want string
	}{
		{[]BrightnessPeriod{{Start: "8:00", End: "09:00", Percent: 50}}, "HH:MM"},
		{[]BrightnessPeriod{{Start: "24:00", End: "09:00", Percent: 50}}, "HH:MM"},
		{[]BrightnessPeriod{{Start: "09:00", End: "09:00", Percent: 50}}, "不能相同"},
		{[]BrightnessPeriod{{Start: "09:00", End: "10:00", Percent: 5}}, "10%"},
		{[]BrightnessPeriod{{Start: "09:00", End: "10:00", Percent: 55}}, "整数倍"},
		{[]BrightnessPeriod{{Start: "09:00", End: "10:00", Percent: 110}}, "100%"},
		{[]BrightnessPeriod{{Start: "22:00", End: "07:00", Percent: 30}, {Start: "06:00", End: "08:00", Percent: 50}}, "重叠"},
	} {
		if err := ValidateBrightness(c.p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v：应报 %q，得到 %v", c.p, c.want, err)
		}
	}
	if _, err := ParseBrightness("garbage"); err == nil {
		t.Fatal("乱码应解析失败")
	}
}
