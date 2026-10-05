package render

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/izzln/content-edge-display/internal/store"
)

// writeBackground 写一张 w×h 的底图：左半 left 色、右半 right 色。
func writeBackground(t *testing.T, dir, name string, w, h int, left, right color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, image.Rect(0, 0, w/2, h), image.NewUniform(left), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(w/2, 0, w, h), image.NewUniform(right), image.Point{}, draw.Src)
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

var (
	red   = color.RGBA{0xFF, 0, 0, 0xFF}
	blue  = color.RGBA{0, 0, 0xFF, 0xFF}
	green = color.RGBA{0, 0xFF, 0, 0xFF}
)

func near(c color.Color, want color.RGBA) bool {
	r, g, b, a := c.RGBA()
	d := func(x uint32, y uint8) bool { return int(x>>8)-int(y) < 8 && int(y)-int(x>>8) < 8 }
	return d(r, want.R) && d(g, want.G) && d(b, want.B) && d(a, want.A)
}

// 底图垫在最下面：按比例铺满画布、居中裁切；区域不设底色时透明、透出底图；叠加模式下媒体区仍是透明的洞。
// 对调的设备用对调版底图，没有对调版时用原图、不翻转。
func TestBackgroundImage(t *testing.T) {
	dir := t.TempDir()
	r, err := New("", dir)
	if err != nil {
		t.Fatal(err)
	}
	// 2:1 的底图放进 1:1 的画布：裁掉左右各四分之一，画布左半红、右半蓝
	writeBackground(t, dir, "bg-0000000000000001.png", 400, 200, red, blue)
	writeBackground(t, dir, "bg-0000000000000002.png", 200, 200, green, green)
	tpl := store.Template{ID: "t", W: 200, H: 200, BackgroundImage: "bg-0000000000000001.png", Regions: []store.Region{
		{ID: "a", X: 0, Y: 0, W: 100, H: 40, Type: store.RegionAttribute, Key: "room"}, // 没有底色：透明
		{ID: "m", X: 100, Y: 0, W: 100, H: 200, Type: store.RegionMedia},
	}}
	if err := store.ValidateTemplate(&tpl); err != nil {
		t.Fatal(err)
	}

	full, err := r.Render(tpl, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !near(full.Image.At(20, 150), red) || !near(full.Image.At(180, 150), blue) || !near(full.Image.At(5, 5), red) {
		t.Fatalf("底图应铺满画布、居中裁切，且透出无底色的区域：%v %v %v", full.Image.At(20, 150), full.Image.At(180, 150), full.Image.At(5, 5))
	}
	ovl, _ := r.Render(tpl, nil, false, true)
	if _, _, _, a := ovl.Image.At(150, 150).RGBA(); a != 0 || !near(ovl.Image.At(20, 150), red) {
		t.Fatal("叠加模式下媒体区必须是透明的洞，其余部分是底图")
	}

	// 对调但没有对调版：原图不翻转（左边仍是红，右边——现在是属性区——仍是蓝）
	mir, _ := r.Render(tpl, nil, true, true)
	if !near(mir.Image.At(180, 150), blue) {
		t.Fatalf("没有对调版底图时不应翻转原图：%v", mir.Image.At(180, 150))
	}
	if _, _, _, a := mir.Image.At(50, 150).RGBA(); a != 0 {
		t.Fatal("对调后媒体区在左边，应为透明")
	}
	tpl.BackgroundImageMirror = "bg-0000000000000002.png"
	mir, _ = r.Render(tpl, nil, true, true)
	if !near(mir.Image.At(180, 150), green) {
		t.Fatalf("对调的设备应使用对调版底图：%v", mir.Image.At(180, 150))
	}

	// 底图文件丢了：退回底色，不让清单生成失败
	tpl.BackgroundImage, tpl.BackgroundImageMirror = "bg-00000000000000ff.png", ""
	if out, err := r.Render(tpl, nil, false, false); err != nil || !near(out.Image.At(20, 150), color.RGBA{0, 0, 0, 0xFF}) {
		t.Fatalf("底图缺失时应退回底色：%v %v", err, out.Image.At(20, 150))
	}
}

// 设计参考图：画布原尺寸，媒体区标红（对调版标在另一边），其余是浅灰。
func TestRenderGuide(t *testing.T) {
	r := newRenderer(t)
	tpl := mediaTemplate() // 右半是媒体区
	img, err := r.RenderGuide(tpl, false)
	if err != nil {
		t.Fatal(err)
	}
	mediaRed := color.RGBA{0xFF, 0x3B, 0x30, 0xFF}
	if img.Bounds().Dx() != tpl.W || img.Bounds().Dy() != tpl.H || !near(img.At(1430, 890), mediaRed) || near(img.At(360, 890), mediaRed) {
		t.Fatalf("参考图尺寸应等于画布、媒体区标红：%v %v %v", img.Bounds(), img.At(1430, 890), img.At(360, 890))
	}
	if img, _ = r.RenderGuide(tpl, true); !near(img.At(10, 890), mediaRed) {
		t.Fatal("对调版参考图里媒体区应在左边")
	}
}
