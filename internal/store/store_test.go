package store

import (
	"os"
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
	err = st.Update(func(s *State) error {
		s.DeviceAttrs["dev-001"] = map[string]string{"room": "302"}
		s.Templates["t1"] = Template{ID: "t1", Name: "T", W: 100, H: 100,
			Regions: []Region{{ID: "r1", W: 100, H: 100, Type: "text", Key: "x"}}}
		s.Displays["dev-001"] = DisplayConfig{Mode: "template", TemplateID: "t1"}
		s.TestUntil["dev-001"] = until
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Attrs("dev-001")["room"] != "302" {
		t.Fatal("attrs not persisted")
	}
	if _, ok := st2.Template("t1"); !ok {
		t.Fatal("template not persisted")
	}
	if st2.Display("dev-001").Mode != "template" {
		t.Fatal("display not persisted")
	}
	if !st2.TestUntil("dev-001").Equal(until) {
		t.Fatal("test_until not persisted")
	}
	// 未配置设备的默认值
	if st2.Display("dev-999").Mode != "playlist" {
		t.Fatal("default display mode should be playlist")
	}
}

func TestUpdateErrorDoesNotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, _ := Open(path)
	_ = st.Update(func(s *State) error { s.DeviceAttrs["d"] = map[string]string{"a": "1"}; return nil })
	if err := st.Update(func(s *State) error { return os.ErrInvalid }); err == nil {
		t.Fatal("expected error")
	}
	st2, _ := Open(path)
	if st2.Attrs("d")["a"] != "1" {
		t.Fatal("previous state lost")
	}
}

func TestConcurrentAccess(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = st.Update(func(s *State) error { s.DeviceAttrs["d"] = map[string]string{"n": "1"}; return nil })
		}()
		go func() { defer wg.Done(); _ = st.Attrs("d") }()
	}
	wg.Wait()
}

func TestValidateTemplate(t *testing.T) {
	valid := func() Template {
		return Template{ID: "t1", Regions: []Region{
			{ID: "left", X: 0, Y: 0, W: 720, H: 900, Type: "attribute", Key: "room"},
			{ID: "right", X: 720, Y: 0, W: 720, H: 900, Type: "image"},
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

	cases := []func(*Template){
		func(x *Template) { x.ID = "bad id!" },
		func(x *Template) { x.Background = "red" },
		func(x *Template) { x.Regions = nil },
		func(x *Template) { x.Regions[0].Type = "video" },
		func(x *Template) { x.Regions[0].Key = "" },    // attribute 必须有 key
		func(x *Template) { x.Regions[1].ID = "left" }, // 重复 id
		func(x *Template) { x.Regions[0].W = 2000 },    // 越界
		func(x *Template) { x.Regions[0].X = -1 },
		func(x *Template) { x.Regions[0].Align = "top" },
		func(x *Template) { x.Regions[0].Color = "#12345" },
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
			return Template{ID: "t1", Regions: []Region{{ID: "right", Type: "image"}}}, true
		}
		return Template{}, false
	}

	d := DisplayConfig{}
	if err := ValidateDisplay(&d, get); err != nil || d.Mode != "playlist" {
		t.Fatalf("empty config should default to playlist: %v %+v", err, d)
	}
	d = DisplayConfig{Mode: "template", TemplateID: "t1", Bindings: map[string]string{"right": "a.png"}}
	if err := ValidateDisplay(&d, get); err != nil {
		t.Fatalf("valid display rejected: %v", err)
	}
	d = DisplayConfig{Mode: "template", TemplateID: "missing"}
	if err := ValidateDisplay(&d, get); err == nil {
		t.Fatal("missing template accepted")
	}
	d = DisplayConfig{Mode: "template", TemplateID: "t1", Bindings: map[string]string{"nope": "a.png"}}
	if err := ValidateDisplay(&d, get); err == nil {
		t.Fatal("binding to unknown region accepted")
	}
	d = DisplayConfig{Mode: "weird"}
	if err := ValidateDisplay(&d, get); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
