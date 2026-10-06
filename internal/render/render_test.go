package render

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"golang.org/x/image/font/sfnt"

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
	r, err := New("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRenderDeterministicAndAttrSensitive(t *testing.T) {
	r := newRenderer(t)
	tpl := mediaTemplate()
	render := func(room string) *image.RGBA {
		out, err := r.Render(tpl, map[string]string{"room": room}, false, false)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	one := render("302")
	if got := one.Bounds(); got.Dx() != 1440 || got.Dy() != 900 {
		t.Fatalf("wrong canvas size: %v", got)
	}
	if sha256.Sum256(encode(t, one)) != sha256.Sum256(encode(t, render("302"))) {
		t.Fatal("same input should render identical output（版本号稳定依赖于此）")
	}
	if sha256.Sum256(encode(t, one)) == sha256.Sum256(encode(t, render("999"))) {
		t.Fatal("attribute change must change output")
	}
	if c := one.RGBAAt(360, 10); c != (color.RGBA{0x1E, 0x3A, 0x8A, 0xFF}) {
		t.Fatalf("left region bg wrong: %+v", c)
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
	// 属性底色跟着换到右半边
	if c := flipped.RGBAAt(1080, 10); c != (color.RGBA{0x1E, 0x3A, 0x8A, 0xFF}) {
		t.Fatalf("mirror 后属性区未移到右半边: %+v", c)
	}
	if c := plain.RGBAAt(1080, 10); c.A != 0 {
		t.Fatalf("未 mirror 时右半边应是透明媒体区: %+v", c)
	}
}

// 属性文字水平居中，offset_y 把它相对垂直中心整体上移/下移。
func TestRenderAttrOffsetY(t *testing.T) {
	r := newRenderer(t)
	textBox := func(off int) image.Rectangle {
		tpl := mediaTemplate()
		tpl.Regions[0].OffsetY = off
		if err := store.ValidateTemplate(&tpl); err != nil {
			t.Fatal(err)
		}
		out, err := r.Render(tpl, map[string]string{"room": "302"}, false, false)
		if err != nil {
			t.Fatal(err)
		}
		bg := color.RGBA{0x1E, 0x3A, 0x8A, 0xFF}
		var box image.Rectangle
		for y := 0; y < 900; y++ {
			for x := 0; x < 720; x++ {
				if out.RGBAAt(x, y) != bg {
					box = box.Union(image.Rect(x, y, x+1, y+1))
				}
			}
		}
		if box.Empty() {
			t.Fatal("属性区没画出文字")
		}
		return box
	}
	center, up := textBox(0), textBox(-200)
	if up != center.Add(image.Pt(0, -200)) {
		t.Fatalf("offset_y=-200 应整体上移 200px：%v → %v", center, up)
	}
	if mid := (center.Min.X + center.Max.X) / 2; mid < 350 || mid > 370 {
		t.Fatalf("文字应水平居中：%v", center)
	}
}

func TestRenderMediaRegionOverlayVsFullscreen(t *testing.T) {
	r := newRenderer(t)
	tpl := mediaTemplate()

	overlay, err := r.Render(tpl, map[string]string{"room": "302"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	// 叠加图：媒体区全透明（且是预乘 alpha 的全零，显示图层需要）
	for _, p := range []image.Point{{720, 0}, {1080, 450}, {1439, 899}} {
		if c := overlay.RGBAAt(p.X, p.Y); c != (color.RGBA{}) {
			t.Fatalf("叠加图媒体区 (%d,%d) 不透明: %+v", p.X, p.Y, c)
		}
	}
	// 属性区仍然不透明，否则会被视频透出来
	if c := overlay.RGBAAt(360, 450); c.A != 0xFF {
		t.Fatalf("叠加图属性区应完全不透明: %+v", c)
	}

	// 整屏图：媒体区填自己的底色（媒体区没内容时用这张，不会黑屏）
	full, err := r.Render(tpl, map[string]string{"room": "302"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := full.RGBAAt(1080, 450); c != (color.RGBA{0x11, 0x22, 0x33, 0xFF}) {
		t.Fatalf("整屏图媒体区底色错误: %+v", c)
	}
}

func TestRenderTestCard(t *testing.T) {
	r := newRenderer(t)
	until := time.Now().Add(5 * time.Minute)
	img, err := r.RenderTestCard("dev-001", map[string]string{"room": "302"}, until)
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
	img2, _ := r.RenderTestCard("dev-001", map[string]string{"room": "302"}, until)
	if sha256.Sum256(encode(t, img)) != sha256.Sum256(encode(t, img2)) {
		t.Fatal("test card render not deterministic")
	}
}

func TestNewBadFontPath(t *testing.T) {
	if _, err := New("/no/such/font.ttf", ""); err == nil {
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
		img, err := r.RenderTestCard("dev-001", nil, at)
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

// 文档推荐的 NotoSansCJK-Regular.ttc（apt install fonts-noto-cjk）是字体集合，必须能加载，并选中简体中文那一款。
func TestFontCollection(t *testing.T) {
	const p = "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"
	if _, err := os.Stat(p); err != nil {
		t.Skip("本机没装 fonts-noto-cjk")
	}
	r, err := New(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := r.font.Name(nil, sfnt.NameIDFamily); name != "Noto Sans CJK SC" {
		t.Fatalf("应选中简体中文字体，得到 %q", name)
	}
}
