package render

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/store"
)

// mediaTemplate: 左半属性 + 右半媒体区（播放列表）。
func mediaTemplate() store.Template {
	t := store.Template{ID: "t2", Regions: []store.Region{
		{ID: "left", X: 0, Y: 0, W: 720, H: 900, Type: store.RegionAttribute, Key: "room", Bg: "#1E3A8A"},
		{ID: "right", X: 720, Y: 0, W: 720, H: 900, Type: store.RegionMedia, Bg: "#112233"},
	}}
	if err := store.ValidateTemplate(&t); err != nil {
		panic(err)
	}
	return t
}

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newRenderer(t *testing.T) *Renderer {
	t.Helper()
	r, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRenderDeterministicAndAttrSensitive(t *testing.T) {
	r := newRenderer(t)
	tpl := mediaTemplate()
	render := func(room string) *Rendered {
		out, err := r.Render(tpl, map[string]string{"room": room}, false, false)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	one := render("302")
	if got := one.Image.Bounds(); got.Dx() != 1440 || got.Dy() != 900 {
		t.Fatalf("wrong canvas size: %v", got)
	}
	if sha256.Sum256(encode(t, one.Image)) != sha256.Sum256(encode(t, render("302").Image)) {
		t.Fatal("same input should render identical output（版本号稳定依赖于此）")
	}
	if sha256.Sum256(encode(t, one.Image)) == sha256.Sum256(encode(t, render("999").Image)) {
		t.Fatal("attribute change must change output")
	}
	if c := one.Image.RGBAAt(360, 10); c != (color.RGBA{0x1E, 0x3A, 0x8A, 0xFF}) {
		t.Fatalf("left region bg wrong: %+v", c)
	}
}

func TestRenderWithoutMediaRegion(t *testing.T) {
	tpl := store.Template{ID: "t", Regions: []store.Region{
		{ID: "txt", X: 0, Y: 0, W: 1440, H: 900, Type: store.RegionText, Key: "欢迎"},
	}}
	if err := store.ValidateTemplate(&tpl); err != nil {
		t.Fatal(err)
	}
	out, err := newRenderer(t).Render(tpl, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if out.HasMedia {
		t.Fatal("模板没有媒体区，HasMedia 应为 false")
	}
}

func TestRenderMirror(t *testing.T) {
	r := newRenderer(t)
	tpl := mediaTemplate()

	plain, err := r.Render(tpl, map[string]string{"room": "302"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := r.Render(tpl, map[string]string{"room": "302"}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plain.MediaRegion, image.Rect(720, 0, 1440, 900); got != want {
		t.Fatalf("媒体区位置 = %v, 期望 %v", got, want)
	}
	if got, want := flipped.MediaRegion, image.Rect(0, 0, 720, 900); got != want {
		t.Fatalf("mirror 后媒体区位置 = %v, 期望 %v", got, want)
	}
	// 属性底色跟着换到右半边
	if c := flipped.Image.RGBAAt(1080, 10); c != (color.RGBA{0x1E, 0x3A, 0x8A, 0xFF}) {
		t.Fatalf("mirror 后属性区未移到右半边: %+v", c)
	}
	if c := plain.Image.RGBAAt(1080, 10); c.A != 0 {
		t.Fatalf("未 mirror 时右半边应是透明媒体区: %+v", c)
	}
}

func TestRenderMediaRegionOverlayVsFullscreen(t *testing.T) {
	r := newRenderer(t)
	tpl := mediaTemplate()

	overlay, err := r.Render(tpl, map[string]string{"room": "302"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !overlay.HasMedia {
		t.Fatal("模板有媒体区，HasMedia 应为 true")
	}
	// 叠加图：媒体区全透明（且是预乘 alpha 的全零，显示图层需要）
	for _, p := range []image.Point{{720, 0}, {1080, 450}, {1439, 899}} {
		if c := overlay.Image.RGBAAt(p.X, p.Y); c != (color.RGBA{}) {
			t.Fatalf("叠加图媒体区 (%d,%d) 不透明: %+v", p.X, p.Y, c)
		}
	}
	// 属性区仍然不透明，否则会被视频透出来
	if c := overlay.Image.RGBAAt(360, 450); c.A != 0xFF {
		t.Fatalf("叠加图属性区应完全不透明: %+v", c)
	}

	// 整屏图：媒体区填自己的底色（媒体区没内容时用这张，不会黑屏）
	full, err := r.Render(tpl, map[string]string{"room": "302"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := full.Image.RGBAAt(1080, 450); c != (color.RGBA{0x11, 0x22, 0x33, 0xFF}) {
		t.Fatalf("整屏图媒体区底色错误: %+v", c)
	}
}

func TestRenderTestCard(t *testing.T) {
	r := newRenderer(t)
	until := time.Now().Add(5 * time.Minute)
	img, err := r.RenderTestCard(1440, 900, "dev-001", map[string]string{"room": "302"}, until)
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds(); got.Dx() != 1440 || got.Dy() != 900 {
		t.Fatalf("wrong size: %v", got)
	}
	// 底色为测试蓝
	if c := img.RGBAAt(5, 5); c != (color.RGBA{0x00, 0x66, 0xCC, 0xFF}) {
		t.Fatalf("test card bg wrong: %+v", c)
	}
	// 同输入两次渲染字节一致（保证清单版本稳定）
	img2, _ := r.RenderTestCard(1440, 900, "dev-001", map[string]string{"room": "302"}, until)
	if sha256.Sum256(encode(t, img)) != sha256.Sum256(encode(t, img2)) {
		t.Fatal("test card render not deterministic")
	}
}

func TestNewBadFontPath(t *testing.T) {
	if _, err := New("/no/such/font.ttf"); err == nil {
		t.Fatal("expected error for missing font file")
	}
}

// 测试卡上的结束时间按传入的时区显示，不能被换成服务器操作系统的时区
// （系统时区是 UTC、配置是 Asia/Tokyo 时，会差 9 小时）。
func TestRenderTestCardUsesGivenTimezone(t *testing.T) {
	r := newRenderer(t)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC) // 东京 12:00
	render := func(at time.Time) [32]byte {
		img, err := r.RenderTestCard(1440, 900, "dev-001", nil, at)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(encode(t, img))
	}
	asTokyo := render(instant.In(tokyo))
	if asTokyo == render(instant.In(time.UTC)) {
		t.Fatal("同一时刻按不同时区应显示不同的钟点")
	}
	if asTokyo != render(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("应显示东京时间 12:00:00")
	}
}
