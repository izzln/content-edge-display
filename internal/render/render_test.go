package render

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/store"
)

// testTemplate: 左半属性 + 右半静态图片（不含媒体区）。
func testTemplate() store.Template {
	t := store.Template{ID: "t1", Regions: []store.Region{
		{ID: "left", X: 0, Y: 0, W: 720, H: 900, Type: store.RegionAttribute, Key: "room", Bg: "#1E3A8A"},
		{ID: "right", X: 720, Y: 0, W: 720, H: 900, Type: store.RegionImage},
	}}
	if err := store.ValidateTemplate(&t); err != nil {
		panic(err)
	}
	return t
}

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
	if err := EncodePNG(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writePNG 写一张 w×h 的纯色图到 dir/name。
func writePNG(t *testing.T, dir, name string, w, h int, c color.RGBA) {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRenderDeterministicAndAttrSensitive(t *testing.T) {
	r, err := New("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tpl := testTemplate()

	one, err := r.Render(tpl, map[string]string{"room": "302"}, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := one.Image.Bounds(); got.Dx() != 1440 || got.Dy() != 900 {
		t.Fatalf("wrong canvas size: %v", got)
	}
	if one.HasMedia {
		t.Fatal("模板没有媒体区，HasMedia 应为 false")
	}
	two, err := r.Render(tpl, map[string]string{"room": "302"}, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(encode(t, one.Image)) != sha256.Sum256(encode(t, two.Image)) {
		t.Fatal("same input should render identical output")
	}
	three, err := r.Render(tpl, map[string]string{"room": "999"}, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(encode(t, one.Image)) == sha256.Sum256(encode(t, three.Image)) {
		t.Fatal("attribute change must change output")
	}
	// 区域底色确实画上了
	if c := one.Image.RGBAAt(360, 10); c != (color.RGBA{0x1E, 0x3A, 0x8A, 0xFF}) {
		t.Fatalf("left region bg wrong: %+v", c)
	}
}

func TestRenderImageRegionCover(t *testing.T) {
	uploads := t.TempDir()
	// 源图比区域扁得多（100×50 对 720×900）：cover 必须按宽撑满并裁掉上下，
	// 区域四角都应被填满，不留底色。
	writePNG(t, uploads, "red.png", 100, 50, color.RGBA{0xFF, 0, 0, 0xFF})

	r, err := New("", uploads)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render(testTemplate(), nil, map[string]string{"right": "red.png"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	red := func(x, y int) bool {
		c := out.Image.RGBAAt(x, y)
		return c.R > 0xF0 && c.G < 0x20 && c.B < 0x20
	}
	for _, p := range []image.Point{
		{720 + 360, 450}, // 中心
		{721, 1},         // 左上
		{1438, 1},        // 右上
		{721, 898},       // 左下
		{1438, 898},      // 右下
	} {
		if !red(p.X, p.Y) {
			t.Fatalf("cover 未铺满区域，(%d,%d) = %+v", p.X, p.Y, out.Image.RGBAAt(p.X, p.Y))
		}
	}

	// 路径穿越防护
	if _, err := r.Render(testTemplate(), nil, map[string]string{"right": "../red.png"}, false, false); err == nil {
		t.Fatal("path traversal in binding accepted")
	}
}

func TestRenderMirror(t *testing.T) {
	r, err := New("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tpl := mediaTemplate()

	plain, err := r.Render(tpl, map[string]string{"room": "302"}, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := r.Render(tpl, map[string]string{"room": "302"}, nil, true, true)
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
	r, err := New("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tpl := mediaTemplate()

	overlay, err := r.Render(tpl, map[string]string{"room": "302"}, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !overlay.HasMedia {
		t.Fatal("模板有媒体区，HasMedia 应为 true")
	}
	// 叠加图：媒体区全透明（且是预乘 alpha 的全零，mpv overlay-add 需要）
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
	full, err := r.Render(tpl, map[string]string{"room": "302"}, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := full.Image.RGBAAt(1080, 450); c != (color.RGBA{0x11, 0x22, 0x33, 0xFF}) {
		t.Fatalf("整屏图媒体区底色错误: %+v", c)
	}
}

func TestRenderTestCard(t *testing.T) {
	r, err := New("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Minute)
	img, err := r.RenderTestCard(1440, 900, "dev-001", "客户A", map[string]string{"room": "302"}, until)
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
	img2, _ := r.RenderTestCard(1440, 900, "dev-001", "客户A", map[string]string{"room": "302"}, until)
	if sha256.Sum256(encode(t, img)) != sha256.Sum256(encode(t, img2)) {
		t.Fatal("test card render not deterministic")
	}
}

func TestNewBadFontPath(t *testing.T) {
	if _, err := New("/no/such/font.ttf", t.TempDir()); err == nil {
		t.Fatal("expected error for missing font file")
	}
}
