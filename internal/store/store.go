// Package store 持久化服务端可变状态：设备（含属性、显示配置、测试屏、更新目标）、模板、程序包、
// 缓存区配额、设备访问凭据与管理口令。规模小（几十台设备），用单个 JSON 文件 + 内存镜像 + 原子写，避免引入数据库依赖。
package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
)

// DefaultImageDurationS 是模板未指定时图片在媒体区的停留秒数。
const DefaultImageDurationS = 10

// 区域类型。
const (
	RegionAttribute = "attribute" // 显示设备属性值
	RegionText      = "text"      // 静态文字
	RegionMedia     = "media"     // 播放列表区：图片/视频由设备端播放，每个模板至多一个
)

// Template 是运营方定义的显示模板（画布 + 若干区域）。
type Template struct {
	ID         string `json:"id"` // 自动生成，管理后台不暴露给使用者
	Name       string `json:"name"`
	W          int    `json:"w"`
	H          int    `json:"h"`
	Background string `json:"background"`
	// 底图（节日主题等区域画不出来的画面，PNG）：data_dir/backgrounds/ 下的文件，按内容命名（IsBackgroundFile）。
	// 压在媒体区上方：媒体区里透明的地方露出视频，不透明的装饰盖在视频上。
	// 开了左右对调的设备用对调版；没有对调版时用原图（不翻转：底图里的文字翻过来就是反字）。
	BackgroundImage       string   `json:"background_image,omitempty"`
	BackgroundImageMirror string   `json:"background_image_mirror,omitempty"`
	ImageDurationS        int      `json:"image_duration_s"` // 媒体区里每张图片停留几秒（视频播完即切）
	Regions               []Region `json:"regions"`
}

// BackgroundFor 返回这个模板在 mirror 设置下要用的底图（可能为空）。
func (t Template) BackgroundFor(mirror bool) string {
	if mirror && t.BackgroundImageMirror != "" {
		return t.BackgroundImageMirror
	}
	return t.BackgroundImage
}

// MediaRect 返回模板的媒体区在画布上的位置（mirror 时是左右对调后的）；没有媒体区时 ok 为 false。
func (t Template) MediaRect(mirror bool) (rect image.Rectangle, ok bool) {
	for _, r := range t.Regions {
		if r.Type == RegionMedia {
			if mirror {
				r = Mirrored(r, t.W)
			}
			return r.Rect(), true
		}
	}
	return image.Rectangle{}, false
}

// Region 是模板中的一个显示区域。
type Region struct {
	ID       string `json:"id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
	Type     string `json:"type"`
	Key      string `json:"key"` // attribute: 属性名; text: 静态文字内容
	FontSize int    `json:"font_size"`
	Color    string `json:"color"`
	Bg       string `json:"bg"`
	Align    string `json:"align"`              // "left" | "center" | "right"
	OffsetY  int    `json:"offset_y,omitempty"` // 文字在垂直居中的基础上下移的像素（负数上移）
}

// Rect 返回区域在画布上的矩形。
func (r Region) Rect() image.Rectangle { return image.Rect(r.X, r.Y, r.X+r.W, r.Y+r.H) }

// DisplayConfig 是一台设备的显示配置。
type DisplayConfig struct {
	TemplateID string   `json:"template_id,omitempty"` // 本设备专属模板；空 = 跟随全局模板
	Mirror     bool     `json:"mirror,omitempty"`      // 左右对调：属性在左还是在右，一个全局模板即可覆盖两种设备
	Playlist   []string `json:"playlist,omitempty"`    // 媒体区播放顺序，文件位于 media_root/<设备>/
}

// Device 是自注册设备及运营方给它的全部设置。设备编号就是它的名字（后台不提供改名）。
type Device struct {
	ID           string    `json:"id"`
	Secret       string    `json:"secret"`
	RegisteredAt time.Time `json:"registered_at"`
	Hardware
	// Rekey 是"同编号、新密钥"的注册请求，等运营方在后台确认。设备丢了身份文件（重装、换卡）
	// 就会这样；也可能是另一台机器撞了编号或有人冒充，所以不自动接受。
	Rekey *RekeyRequest `json:"rekey,omitempty"`

	Attrs     map[string]string `json:"attrs,omitempty"`     // 属性（模板的属性区域显示）
	Display   DisplayConfig     `json:"display,omitzero"`    // 专属模板、左右对调、播放顺序
	TestUntil time.Time         `json:"test_until,omitzero"` // 在此之前显示测试卡
	Update    *UpdateTarget     `json:"update,omitempty"`    // 程序更新目标
}

// Hardware 是设备注册时报上来的信息（后台显示，供运营方辨认）。
type Hardware struct {
	Hostname     string `json:"hostname,omitempty"`
	HWSerial     string `json:"hw_serial,omitempty"`
	MAC          string `json:"mac,omitempty"`
	IP           string `json:"ip,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// RekeyRequest 是一次待确认的换密钥注册请求，附带请求方信息供运营方核对。
type RekeyRequest struct {
	Secret      string    `json:"secret"`
	Fingerprint string    `json:"fingerprint"` // 密钥指纹，与设备日志里的 key= 对照
	At          time.Time `json:"at"`
	Hardware
}

// Package 是已上传的设备端程序包（make package 产出的 tar.gz，装机与 OTA 共用）。
type Package struct {
	Version    string    `json:"version"`
	File       string    `json:"file"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Notes      string    `json:"notes,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// UpdateTarget 是下发给某台设备的更新目标。
type UpdateTarget struct {
	Version   string    `json:"version"`
	NotBefore time.Time `json:"not_before,omitzero"` // 零值=立即
}

// GlobalConfig 是全局显示设置（运营方在管理后台改，立即对所有设备生效）。
type GlobalConfig struct {
	TemplateID string `json:"template_id,omitempty"` // 全局默认模板
}

// Access 是统一下发给所有设备的访问凭据（后台「设备访问」）。
type Access struct {
	RootPasswordHash  string    `json:"root_password_hash,omitempty"` // SHA-512 crypt（$6$），设备用 chpasswd -e 写入
	RootPasswordSetAt time.Time `json:"root_password_set_at,omitzero"`
	SSHKeys           []string  `json:"ssh_keys,omitempty"` // 允许 SSH 登录 root 的公钥；非空时设备禁止密码 SSH
}

// Admin 是后台的管理口令：只存 PBKDF2 哈希。ConfigHash 是 server.json 里 admin_token 的哈希，
// 用来发现有人在服务器上把它改成了新值（找回口令，见 server.syncAdminToken）。
type Admin struct {
	TokenHash  string    `json:"token_hash"`
	ConfigHash string    `json:"config_hash"`
	ChangedAt  time.Time `json:"changed_at,omitzero"` // 在后台改口令的时间
}

// State 是全部可变状态；字段直接序列化到 state.json。
type State struct {
	Devices      map[string]*Device  `json:"devices"`
	Templates    map[string]Template `json:"templates"`
	Packages     map[string]Package  `json:"packages"`
	Global       GlobalConfig        `json:"global"`
	CacheQuotaGB int                 `json:"cache_quota_gb"` // 服务端文件缓存区的配额（GB）
	Access       Access              `json:"access"`
	Admin        Admin               `json:"admin"`
}

// DefaultCacheQuotaGB 是文件缓存区的默认配额。
const DefaultCacheQuotaGB = 16

func (s *State) init() {
	if s.Devices == nil {
		s.Devices = map[string]*Device{}
	}
	if s.Templates == nil {
		s.Templates = map[string]Template{}
	}
	if s.Packages == nil {
		s.Packages = map[string]Package{}
	}
	if s.CacheQuotaGB <= 0 {
		s.CacheQuotaGB = DefaultCacheQuotaGB
	}
}

// LatestPackage 返回最近上传的程序包（新设备装机时下载它）。
func (s *State) LatestPackage() (Package, bool) {
	var latest Package
	for _, p := range s.Packages {
		if p.UploadedAt.After(latest.UploadedAt) {
			latest = p
		}
	}
	return latest, latest.File != ""
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

// Update 在写锁下修改状态并原子持久化。fn 返回错误时不写盘，原样返回该错误——
// 所以 fn 要先检查、再修改（检查不通过时状态不能已经改了一半）。
func (st *Store) Update(fn func(*State) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := fn(&st.s); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&st.s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFile(st.path, data, 0o644)
}

// Device 返回已注册设备的副本。
func (st *Store) Device(id string) (Device, bool) {
	var d Device
	var ok bool
	st.View(func(s *State) {
		var p *Device
		if p, ok = s.Devices[id]; ok {
			d = *p
		}
	})
	return d, ok
}

// Template 按 ID 取模板。
func (st *Store) Template(id string) (Template, bool) {
	var t Template
	var ok bool
	st.View(func(s *State) { t, ok = s.Templates[id] })
	return t, ok
}

// ---- 校验 ----

var (
	idPattern         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	colorPattern      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	backgroundPattern = regexp.MustCompile(`^bg-[0-9a-f]{16}\.png$`)
)

// IsBackgroundFile 判断 name 是不是底图文件名（见 BackgroundFile）。
func IsBackgroundFile(name string) bool { return backgroundPattern.MatchString(name) }

// BackgroundFile 是底图的文件名：bg-<内容 sha256（十六进制）前 16 位>.png。按内容命名，换图就换名。
func BackgroundFile(sha256Hex string) string { return "bg-" + sha256Hex[:16] + ".png" }

// ValidateTemplate 填充默认值并校验模板定义。
func ValidateTemplate(t *Template) error {
	if !idPattern.MatchString(t.ID) {
		return errors.New("template: id 只能包含字母数字-_，长度1~64")
	}
	if t.Name == "" {
		t.Name = t.ID
	}
	if t.W <= 0 {
		t.W = manifest.CanvasW
	}
	if t.H <= 0 {
		t.H = manifest.CanvasH
	}
	if t.Background == "" {
		t.Background = "#000000"
	}
	if !colorPattern.MatchString(t.Background) {
		return fmt.Errorf("template: 非法背景色 %q", t.Background)
	}
	for _, f := range []string{t.BackgroundImage, t.BackgroundImageMirror} {
		if f != "" && !IsBackgroundFile(f) {
			return fmt.Errorf("template: 非法底图 %q（请在后台「底图」里上传）", f)
		}
	}
	if t.BackgroundImage == "" && t.BackgroundImageMirror != "" {
		return errors.New("template: 有对调版底图时必须先有常规底图")
	}
	if len(t.Regions) == 0 {
		return errors.New("template: 至少需要一个区域")
	}
	if t.ImageDurationS <= 0 {
		t.ImageDurationS = DefaultImageDurationS
	}
	if t.ImageDurationS > 3600 {
		return errors.New("template: 图片停留时长须在 1~3600 秒之间")
	}
	seen := map[string]bool{}
	media := 0
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
		case RegionAttribute, RegionText:
		case RegionMedia:
			media++
			if media > 1 {
				return errors.New("template: 至多只能有一个媒体区（只有一个视频图层）")
			}
		default:
			return fmt.Errorf("region %q: 未知类型 %q", r.ID, r.Type)
		}
		if r.Type == RegionAttribute && r.Key == "" {
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
		if r.OffsetY < -r.H || r.OffsetY > r.H {
			return fmt.Errorf("region %q: 垂直偏移不能超过区域高度 %d", r.ID, r.H)
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

// Mirrored 返回区域左右对调后的位置：属性在左还是在右，用同一个模板即可覆盖两种设备。
// 只换位置，文字本身不镜像。
func Mirrored(r Region, canvasW int) Region {
	r.X = canvasW - (r.X + r.W)
	switch r.Align {
	case "left":
		r.Align = "right"
	case "right":
		r.Align = "left"
	}
	return r
}

// NewTemplateID 生成模板 ID。使用者不需要关心它，管理后台也不暴露。
func NewTemplateID() string { return "tpl-" + strings.ToLower(rand.Text()[:8]) }

// SeedDefaultTemplate 在还没有任何模板时播种一个左右分屏模板（左显示房间号，右放图片/视频），
// 返回是否播种了。运营方开箱就有能用的版式，不必先自己建模板。
func SeedDefaultTemplate(st *State) (Template, bool) {
	if len(st.Templates) > 0 {
		return Template{}, false
	}
	w, h := manifest.CanvasW, manifest.CanvasH
	t := Template{
		ID: NewTemplateID(), Name: "左右分屏", W: w, H: h,
		Background: "#000000", ImageDurationS: DefaultImageDurationS,
		Regions: []Region{
			{ID: "left", X: 0, Y: 0, W: w / 2, H: h, Type: RegionAttribute,
				Key: "room", FontSize: 160, Color: "#FFFFFF", Bg: "#1E3A8A", Align: "center"},
			{ID: "right", X: w / 2, Y: 0, W: w / 2, H: h, Type: RegionMedia, FontSize: 48, Color: "#FFFFFF", Align: "center"},
		},
	}
	st.Templates[t.ID] = t
	return t, true
}

// EnsureGlobalTemplate 保证全局默认模板指向一个真实存在的模板：缺失或指向已删除的模板时，
// 自动指向 ID 最小的现存模板（确定性），返回是否做了改动。
func EnsureGlobalTemplate(st *State) bool {
	if _, ok := st.Templates[st.Global.TemplateID]; ok {
		return false
	}
	if len(st.Templates) == 0 {
		return false
	}
	ids := slices.Sorted(maps.Keys(st.Templates))
	st.Global.TemplateID = ids[0]
	return true
}
