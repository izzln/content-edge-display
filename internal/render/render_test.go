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

func testTemplate() store.Template {
	t := store.Template{ID: "t1", Regions: []store.Region{
		{ID: "left", X: 0, Y: 0, W: 720, H: 900, Type: "attribute", Key: "room", Bg: "#1E3A8A"},
		{ID: "right", X: 720, Y: 0, W: 720, H: 900, Type: "image"},
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

func TestRenderDeterministicAndAttrSensitive(t *testing.T) {
	r, err := New("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tpl := testTemplate()

	img1, err := r.Render(tpl, map[string]string{"room": "302"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := img1.Bounds(); got.Dx() != 1440 || got.Dy() != 900 {
		t.Fatalf("wrong canvas size: %v", got)
	}
	img2, err := r.Render(tpl, map[string]string{"room": "302"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(encode(t, img1)) != sha256.Sum256(encode(t, img2)) {
		t.Fatal("same input should render identical output")
	}
	img3, err := r.Render(tpl, map[string]string{"room": "999"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(encode(t, img1)) == sha256.Sum256(encode(t, img3)) {
		t.Fatal("attribute change must change output")
	}
	// 区域底色确实画上了
	if c := img1.RGBAAt(360, 10); c != (color.RGBA{0x1E, 0x3A, 0x8A, 0xFF}) {
		t.Fatalf("left region bg wrong: %+v", c)
	}
}

func TestRenderImageRegion(t *testing.T) {
	uploads := t.TempDir()
	// 100×50 纯红图片
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 100; x++ {
			src.SetRGBA(x, y, color.RGBA{0xFF, 0, 0, 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "red.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := New("", uploads)
	if err != nil {
		t.Fatal(err)
	}
	img, err := r.Render(testTemplate(), nil, map[string]string{"right": "red.png"})
	if err != nil {
		t.Fatal(err)
	}
	// 右区中心应为红色（等比缩放居中后中心必然被覆盖）
	if c := img.RGBAAt(720+360, 450); c.R < 0xF0 || c.G > 0x20 {
		t.Fatalf("image region center not red: %+v", c)
	}

	// 路径穿越防护
	if _, err := r.Render(testTemplate(), nil, map[string]string{"right": "../red.png"}); err == nil {
		t.Fatal("path traversal in binding accepted")
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
