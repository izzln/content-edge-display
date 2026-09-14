// Package store 持久化服务端可变状态：设备属性、显示模板、显示配置、测试屏截止时间。
// 规模小（几十台设备），用单个 JSON 文件 + 内存镜像 + 原子写，避免引入数据库依赖。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Template 是运营方定义的显示模板（画布 + 若干区域）。
type Template struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	W          int      `json:"w"`
	H          int      `json:"h"`
	Background string   `json:"background"`
	Regions    []Region `json:"regions"`
}

// Region 是模板中的一个显示区域。
type Region struct {
	ID       string `json:"id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
	Type     string `json:"type"` // "attribute" | "text" | "image"
	Key      string `json:"key"`  // attribute: 属性名; text: 静态文字内容
	FontSize int    `json:"font_size"`
	Color    string `json:"color"`
	Bg       string `json:"bg"`
	Align    string `json:"align"` // "left" | "center" | "right"
}

// DisplayConfig 是一台设备的显示配置。
type DisplayConfig struct {
	Mode       string            `json:"mode"` // "playlist"(默认) | "template"
	TemplateID string            `json:"template_id,omitempty"`
	Bindings   map[string]string `json:"bindings,omitempty"` // region_id -> 上传图片文件名
}

// State 是全部可变状态；字段直接序列化到 state.json。
type State struct {
	DeviceAttrs map[string]map[string]string `json:"device_attrs"`
	Templates   map[string]Template          `json:"templates"`
	Displays    map[string]DisplayConfig     `json:"displays"`
	TestUntil   map[string]time.Time         `json:"test_until"`
}

func (s *State) init() {
	if s.DeviceAttrs == nil {
		s.DeviceAttrs = map[string]map[string]string{}
	}
	if s.Templates == nil {
		s.Templates = map[string]Template{}
	}
	if s.Displays == nil {
		s.Displays = map[string]DisplayConfig{}
	}
	if s.TestUntil == nil {
		s.TestUntil = map[string]time.Time{}
	}
}

// Store 是 State 的持久化容器，方法并发安全。
type Store struct {
	mu   sync.RWMutex
	path string
	s    State
}

// Open 加载（或初始化）状态文件。
func Open(path string) (*Store, error) {
	st := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &st.s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case os.IsNotExist(err):
	default:
		return nil, err
	}
	st.s.init()
	return st, nil
}

// View 在读锁下访问状态；fn 内不得修改或保留 State 引用之外的可变数据。
func (st *Store) View(fn func(*State)) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	fn(&st.s)
}

// Update 在写锁下修改状态并原子持久化；fn 返回错误则不落盘（内存修改不回滚，
// 因此 fn 应先校验后修改）。
func (st *Store) Update(fn func(*State) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := fn(&st.s); err != nil {
		return err
	}
	return st.persistLocked()
}

func (st *Store) persistLocked() error {
	data, err := json.MarshalIndent(&st.s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// ---- 便捷读取 ----

// Attrs 返回设备属性的副本。
func (st *Store) Attrs(deviceID string) map[string]string {
	out := map[string]string{}
	st.View(func(s *State) {
		for k, v := range s.DeviceAttrs[deviceID] {
			out[k] = v
		}
	})
	return out
}

// Display 返回设备显示配置（未配置时为 playlist 模式）。
func (st *Store) Display(deviceID string) DisplayConfig {
	var d DisplayConfig
	st.View(func(s *State) {
		d = s.Displays[deviceID]
		b := map[string]string{}
		for k, v := range d.Bindings {
			b[k] = v
		}
		d.Bindings = b
	})
	if d.Mode == "" {
		d.Mode = "playlist"
	}
	return d
}

// Template 按 ID 取模板。
func (st *Store) Template(id string) (Template, bool) {
	var t Template
	var ok bool
	st.View(func(s *State) { t, ok = s.Templates[id] })
	return t, ok
}

// TestUntil 返回设备测试屏截止时间（零值表示未在测试）。
func (st *Store) TestUntil(deviceID string) time.Time {
	var t time.Time
	st.View(func(s *State) { t = s.TestUntil[deviceID] })
	return t
}

// ---- 校验 ----

var (
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// ValidateTemplate 填充默认值并校验模板定义。
func ValidateTemplate(t *Template) error {
	if !idPattern.MatchString(t.ID) {
		return errors.New("template: id 只能包含字母数字-_，长度1~64")
	}
	if t.Name == "" {
		t.Name = t.ID
	}
	if t.W <= 0 {
		t.W = 1440
	}
	if t.H <= 0 {
		t.H = 900
	}
	if t.Background == "" {
		t.Background = "#000000"
	}
	if !colorPattern.MatchString(t.Background) {
		return fmt.Errorf("template: 非法背景色 %q", t.Background)
	}
	if len(t.Regions) == 0 {
		return errors.New("template: 至少需要一个区域")
	}
	seen := map[string]bool{}
	for i := range t.Regions {
		r := &t.Regions[i]
		if !idPattern.MatchString(r.ID) {
			return fmt.Errorf("region %d: id 非法", i)
		}
		if seen[r.ID] {
			return fmt.Errorf("region %q: id 重复", r.ID)
		}
		seen[r.ID] = true
		switch r.Type {
		case "attribute", "text", "image":
		default:
			return fmt.Errorf("region %q: 未知类型 %q", r.ID, r.Type)
		}
		if r.Type == "attribute" && r.Key == "" {
			return fmt.Errorf("region %q: attribute 区域必须指定 key", r.ID)
		}
		if r.W <= 0 || r.H <= 0 || r.X < 0 || r.Y < 0 ||
			r.X+r.W > t.W || r.Y+r.H > t.H {
			return fmt.Errorf("region %q: 位置/尺寸越界", r.ID)
		}
		if r.FontSize <= 0 {
			r.FontSize = 48
		}
		if r.Color == "" {
			r.Color = "#FFFFFF"
		}
		if !colorPattern.MatchString(r.Color) {
			return fmt.Errorf("region %q: 非法文字颜色", r.ID)
		}
		if r.Bg != "" && !colorPattern.MatchString(r.Bg) {
			return fmt.Errorf("region %q: 非法底色", r.ID)
		}
		switch r.Align {
		case "":
			r.Align = "center"
		case "left", "center", "right":
		default:
			return fmt.Errorf("region %q: 非法对齐 %q", r.ID, r.Align)
		}
	}
	return nil
}

// ValidateDisplay 校验显示配置引用的模板与区域绑定。
func ValidateDisplay(d *DisplayConfig, getTemplate func(string) (Template, bool)) error {
	switch d.Mode {
	case "", "playlist":
		d.Mode = "playlist"
		return nil
	case "template":
	default:
		return fmt.Errorf("display: 未知模式 %q", d.Mode)
	}
	t, ok := getTemplate(d.TemplateID)
	if !ok {
		return fmt.Errorf("display: 模板 %q 不存在", d.TemplateID)
	}
	regionIDs := map[string]bool{}
	for _, r := range t.Regions {
		regionIDs[r.ID] = true
	}
	for rid := range d.Bindings {
		if !regionIDs[rid] {
			return fmt.Errorf("display: 绑定了不存在的区域 %q", rid)
		}
	}
	return nil
}
