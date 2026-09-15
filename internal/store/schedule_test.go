package store

import (
	"testing"
	"time"
)

func TestValidateSchedules(t *testing.T) {
	get := func(id string) (Template, bool) { return Template{ID: id}, id == "day" || id == "night" }

	list := []Schedule{
		{TemplateID: "day", Days: []int{1, 2, 3, 4, 5}, Start: "08:00", End: "18:00"},
		{TemplateID: "night", Start: "22:00", End: "06:00"},
	}
	if err := ValidateSchedules(list, get); err != nil {
		t.Fatalf("valid schedules rejected: %v", err)
	}
	if list[0].ID != "s1" || list[1].ID != "s2" || list[1].Days == nil {
		t.Fatalf("defaults not filled: %+v", list)
	}

	bad := [][]Schedule{
		{{TemplateID: "missing", Start: "08:00", End: "18:00"}},
		{{TemplateID: "day", Start: "8:00", End: "18:00"}},
		{{TemplateID: "day", Start: "08:00", End: "08:00"}},
		{{TemplateID: "day", Days: []int{7}, Start: "08:00", End: "18:00"}},
		{{ID: "a", TemplateID: "day", Start: "08:00", End: "18:00"}, {ID: "a", TemplateID: "day", Start: "08:00", End: "18:00"}},
	}
	for i, l := range bad {
		if err := ValidateSchedules(l, get); err == nil {
			t.Errorf("case %d: invalid schedules accepted: %+v", i, l)
		}
	}
}

func TestActiveSchedule(t *testing.T) {
	list := []Schedule{
		{ID: "work", TemplateID: "day", Days: []int{1, 2, 3, 4, 5}, Start: "08:00", End: "18:00"},
		{ID: "night", TemplateID: "night", Days: []int{5}, Start: "22:00", End: "06:00"}, // 周五夜到周六早
	}
	// 2026-09-16 是周三
	at := func(day, hhmm string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04", day+" "+hhmm)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		when time.Time
		want string
	}{
		{at("2026-09-16", "09:30"), "work"},  // 周三上班时间
		{at("2026-09-16", "18:00"), ""},      // 结束时刻不含
		{at("2026-09-16", "07:59"), ""},      // 开始前
		{at("2026-09-19", "10:00"), ""},      // 周六不在 work 的星期内
		{at("2026-09-18", "23:00"), "night"}, // 周五夜
		{at("2026-09-19", "03:00"), "night"}, // 周六凌晨：跨午夜，按起始日周五匹配
		{at("2026-09-17", "03:00"), ""},      // 周四凌晨：起始日周三不在 night 的星期内
	}
	for _, c := range cases {
		sc, ok := ActiveSchedule(list, c.when)
		got := ""
		if ok {
			got = sc.ID
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.when, got, c.want)
		}
	}

	// 无星期限制 = 每天
	daily := []Schedule{{ID: "d", TemplateID: "x", Start: "00:00", End: "23:59"}}
	if _, ok := ActiveSchedule(daily, at("2026-09-19", "12:00")); !ok {
		t.Fatal("daily schedule should match")
	}
}
