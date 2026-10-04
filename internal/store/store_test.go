package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute).Truncate(time.Second)
	err = st.Update(func(s *State) {
		s.DeviceAttrs["dev-001"] = map[string]string{"room": "302"}
		s.Templates["t1"] = Template{ID: "t1", Name: "T", W: 100, H: 100,
			Regions: []Region{{ID: "r1", W: 100, H: 100, Type: "text", Key: "x"}}}
		s.Displays["dev-001"] = DisplayConfig{TemplateID: "t1"}
		s.TestUntil["dev-001"] = until
	})
	if err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st2.Template("t1"); !ok {
		t.Fatal("template not persisted")
	}
	st2.View(func(s *State) {
		if s.DeviceAttrs["dev-001"]["room"] != "302" || s.Displays["dev-001"].TemplateID != "t1" || !s.TestUntil["dev-001"].Equal(until) {
			t.Fatalf("state not persisted: %+v", s)
		}
		if s.CacheQuotaGB != DefaultCacheQuotaGB {
			t.Fatalf("缓存区配额默认值应在加载时填好：%d", s.CacheQuotaGB)
		}
	})
}

func TestConcurrentAccess(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = st.Update(func(s *State) { s.DeviceAttrs["d"] = map[string]string{"n": "1"} })
		}()
		go func() { defer wg.Done(); st.View(func(s *State) { _ = s.DeviceAttrs["d"] }) }()
	}
	wg.Wait()
}

func TestValidateTemplate(t *testing.T) {
	valid := func() Template {
		return Template{ID: "t1", Regions: []Region{
			{ID: "left", X: 0, Y: 0, W: 720, H: 900, Type: "attribute", Key: "room"},
			{ID: "right", X: 720, Y: 0, W: 720, H: 900, Type: "media"},
		}}
	}

	tpl := valid()
	if err := ValidateTemplate(&tpl); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
	if tpl.W != 1440 || tpl.H != 900 || tpl.Background != "#000000" {
		t.Fatalf("defaults not filled: %+v", tpl)
	}
	if tpl.Regions[0].FontSize != 48 || tpl.Regions[0].Align != "center" || tpl.Regions[0].Color != "#FFFFFF" {
		t.Fatalf("region defaults not filled: %+v", tpl.Regions[0])
	}
	if tpl.ImageDurationS != DefaultImageDurationS {
		t.Fatalf("图片停留时长默认值未填充：%d", tpl.ImageDurationS)
	}

	cases := []func(*Template){
		func(x *Template) { x.ID = "bad id!" },
		func(x *Template) { x.Background = "red" },
		func(x *Template) { x.Regions = nil },
		func(x *Template) { x.Regions[0].Type = "video" },
		func(x *Template) { x.Regions[1].Type = "image" }, // 未知区域类型
		func(x *Template) { x.Regions[0].Key = "" },       // attribute 必须有 key
		func(x *Template) { x.Regions[1].ID = "left" },    // 重复 id
		func(x *Template) { x.Regions[0].W = 2000 },       // 越界
		func(x *Template) { x.Regions[0].X = -1 },
		func(x *Template) { x.Regions[0].Align = "top" },
		func(x *Template) { x.Regions[0].Color = "#12345" },
		func(x *Template) { x.ImageDurationS = 99999 }, // 停留时长上限
		func(x *Template) { // 一个模板至多一个媒体区（只有一个视频图层）
			x.Regions[0].Type, x.Regions[0].Key = RegionMedia, ""
		},
	}
	for i, mutate := range cases {
		tpl := valid()
		mutate(&tpl)
		if err := ValidateTemplate(&tpl); err == nil {
			t.Errorf("case %d: invalid template accepted: %+v", i, tpl)
		}
	}
}

func TestValidateDisplay(t *testing.T) {
	get := func(id string) (Template, bool) {
		if id == "t1" {
			return Template{ID: "t1", Regions: []Region{{ID: "right", Type: RegionMedia}}}, true
		}
		return Template{}, false
	}

	for id, ok := range map[string]bool{"": true /* 跟随全局 */, "t1": true, "missing": false} {
		if err := ValidateDisplay(DisplayConfig{TemplateID: id}, get); (err == nil) != ok {
			t.Errorf("template_id %q: err=%v，期望 ok=%v", id, err, ok)
		}
	}
}

// 首启播种：开箱就得有一个能用的左右分屏模板，并被设为全局默认。
func TestSeedDefaultTemplateAndEnsureGlobal(t *testing.T) {
	st := &State{}
	st.init()

	tpl, seeded := SeedDefaultTemplate(st)
	if !seeded || len(st.Templates) != 1 {
		t.Fatalf("首启应当播种一个模板：seeded=%v templates=%d", seeded, len(st.Templates))
	}
	if _, ok := tpl.MediaRegion(); !ok {
		t.Fatal("默认模板必须带一个媒体区，否则没法放图片/视频")
	}
	if tpl.ID == "" || tpl.ImageDurationS <= 0 {
		t.Fatalf("默认模板字段不完整：%+v", tpl)
	}

	// 已有模板时不再播种
	if _, seeded := SeedDefaultTemplate(st); seeded {
		t.Fatal("已有模板时不应重复播种")
	}

	// 全局默认模板缺失 → 自动指向现存模板
	if !EnsureGlobalTemplate(st) || st.Global.TemplateID != tpl.ID {
		t.Fatalf("全局默认模板未自动指向播种出来的模板：%q", st.Global.TemplateID)
	}
	if EnsureGlobalTemplate(st) {
		t.Fatal("已经指向有效模板时不应再改")
	}

	// 全局指向被删掉的模板 → 自动纠正到 ID 最小的现存模板
	st.Templates["tpl-0000"] = tpl
	st.Global.TemplateID = "已删除"
	if !EnsureGlobalTemplate(st) || st.Global.TemplateID != "tpl-0000" {
		t.Fatalf("全局指向失效模板时应自动纠正，得到 %q", st.Global.TemplateID)
	}
}

// Mirrored 只换位置不镜像文字：一个模板即可覆盖“属性在左”和“属性在右”两种设备。
func TestMirrored(t *testing.T) {
	left := Region{ID: "left", X: 0, Y: 100, W: 720, H: 900, Align: "left"}
	got := Mirrored(left, 1440)
	if got.X != 720 || got.Y != 100 || got.W != 720 || got.H != 900 {
		t.Fatalf("对调后位置错误：%+v", got)
	}
	if got.Align != "right" {
		t.Fatalf("对调后对齐应跟着换边，得到 %q", got.Align)
	}
	// 对调两次回到原处
	if back := Mirrored(got, 1440); back != left {
		t.Fatalf("对调两次应回到原处：%+v", back)
	}
	if c := Mirrored(Region{X: 100, W: 200, Align: "center"}, 1000); c.X != 700 || c.Align != "center" {
		t.Fatalf("居中对齐不应改变：%+v", c)
	}
}
