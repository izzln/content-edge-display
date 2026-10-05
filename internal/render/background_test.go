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

// writeBackground 写一张 w×h 的透明 PNG，再按 rects 填色（底图在媒体区要透明，装饰可以压进来）。
func writeBackground(t *testing.T, dir, name string, w, h int, rects map[image.Rectangle]color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for r, c := range rects {
		draw.Draw(img, r, image.NewUniform(c), image.Point{}, draw.Src)
	}
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
	red    = color.RGBA{0xFF, 0, 0, 0xFF}
	blue   = color.RGBA{0, 0, 0xFF, 0xFF}
	green  = color.RGBA{0, 0xFF, 0, 0xFF}
	yellow = color.RGBA{0xFF, 0xFF, 0, 0xFF}
	black  = color.RGBA{0, 0, 0, 0xFF}
)

func near(c color.Color, want color.RGBA) bool {
	r, g, b, a := c.RGBA()
	d := func(x uint32, y uint8) bool { return int(x>>8)-int(y) < 8 && int(y)-int(x>>8) < 8 }
	return d(r, want.R) && d(g, want.G) && d(b, want.B) && d(a, want.A)
}

func alphaAt(img image.Image, x, y int) uint32 { _, _, _, a := img.At(x, y).RGBA(); return a }

// 底图压在媒体区上方：媒体区里底图透明的地方露出视频，不透明的装饰盖在视频上；底图铺满画布、居中裁切；
// 区域不设底色时透出底图。对调的设备用对调版底图，没有对调版时用原图、不翻转。
func TestBackgroundImage(t *testing.T) {
	dir := t.TempDir()
	r, err := New("", dir)
	if err != nil {
		t.Fatal(err)
	}
	// 画布 200×200，媒体区在右半边。底图 400×200（2:1）放进 1:1 画布：左右各裁掉四分之一，
	// 所以底图里 x∈[100,300) 对应画布 [0,200)：左半红（不透明），右半透明，媒体区里 (150..170) 有一块蓝色装饰
	writeBackground(t, dir, "bg-0000000000000001.png", 400, 200, map[image.Rectangle]color.RGBA{
		image.Rect(0, 0, 200, 200): red, image.Rect(250, 150, 270, 170): blue})
	writeBackground(t, dir, "bg-0000000000000002.png", 200, 200, map[image.Rectangle]color.RGBA{
		image.Rect(100, 0, 200, 200): green}) // 对调版：媒体区在左，左半透明
	tpl := store.Template{ID: "t", W: 200, H: 200, BackgroundImage: "bg-0000000000000001.png", Regions: []store.Region{
		{ID: "a", X: 0, Y: 0, W: 100, H: 40, Type: store.RegionAttribute, Key: "room"}, // 没有底色：透明
		{ID: "m", X: 100, Y: 0, W: 100, H: 200, Type: store.RegionMedia},
	}}
	if err := store.ValidateTemplate(&tpl); err != nil {
		t.Fatal(err)
	}

	ovl, err := r.Render(tpl, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !near(ovl.Image.At(20, 150), red) || !near(ovl.Image.At(5, 5), red) || alphaAt(ovl.Image, 120, 100) != 0 || !near(ovl.Image.At(160, 160), blue) {
		t.Fatalf("叠加图：媒体区透明处露出视频，装饰压在上面，其余是底图：%v %v %v",
			ovl.Image.At(20, 150), ovl.Image.At(120, 100), ovl.Image.At(160, 160))
	}
	full, _ := r.Render(tpl, nil, false, false)
	if !near(full.Image.At(120, 100), black) || !near(full.Image.At(160, 160), blue) {
		t.Fatal("整屏图：媒体区透明处是底色，装饰照样在")
	}
	if share := TransparentShare(CoverImage(mustDecode(t, dir, "bg-0000000000000001.png"), 200, 200), ovl.MediaRegion); share < 0.97 || share > 0.99 {
		t.Fatalf("媒体区透明比例应约为 98%%：%.3f", share)
	}

	// 效果预览：内容铺进媒体区，模板（含装饰）盖在上面
	content := image.NewRGBA(image.Rect(0, 0, 50, 50))
	draw.Draw(content, content.Bounds(), image.NewUniform(yellow), image.Point{}, draw.Src)
	pv := Preview(ovl, content)
	if !near(pv.At(120, 100), yellow) || !near(pv.At(160, 160), blue) || !near(pv.At(20, 150), red) {
		t.Fatal("预览应是内容在媒体区、装饰与底图在上面")
	}

	// 对调但没有对调版：原图不翻转——左边的红色不透明部分正好盖住换到左边的媒体区（所以需要对调版）
	mir, _ := r.Render(tpl, nil, true, true)
	if !near(mir.Image.At(20, 150), red) {
		t.Fatalf("没有对调版底图时不应翻转原图：%v", mir.Image.At(20, 150))
	}
	tpl.BackgroundImageMirror = "bg-0000000000000002.png"
	mir, _ = r.Render(tpl, nil, true, true)
	if alphaAt(mir.Image, 20, 150) != 0 || !near(mir.Image.At(180, 150), green) {
		t.Fatal("对调的设备应使用对调版底图")
	}

	// 底图文件丢了：退回底色，不让清单生成失败
	tpl.BackgroundImage, tpl.BackgroundImageMirror = "bg-00000000000000ff.png", ""
	if out, err := r.Render(tpl, nil, false, false); err != nil || !near(out.Image.At(20, 150), black) {
		t.Fatalf("底图缺失时应退回底色：%v", err)
	}

	// 没有底图的模板：媒体区仍是整块透明
	tpl.BackgroundImage = ""
	if plain, _ := r.Render(tpl, nil, false, true); alphaAt(plain.Image, 160, 160) != 0 {
		t.Fatal("没有底图时媒体区整块透明")
	}
}

func mustDecode(t *testing.T, dir, name string) image.Image {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
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
