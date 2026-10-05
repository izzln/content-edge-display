// Package render 在服务端把显示模板/测试卡合成为位图：模板的静态部分（底图、属性、文字、底色）
// 由服务端画，媒体区留给设备端播放（见 Render 的 overlayMode）。
package render

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/store"
)

// Renderer 持有渲染用字体，并缓存缩放好的底图。
type Renderer struct {
	font  *opentype.Font
	bgDir string // 底图目录（模板的 background_image 在这里）

	mu  sync.Mutex
	bgs map[bgKey]*image.RGBA // 解码并缩放到画布大小的底图：各设备渲染同一模板时不重复解码
}

type bgKey struct {
	file string
	w, h int
}

const maxCachedBackgrounds = 8

// New 创建渲染器。fontPath 为空时退回内嵌的 Go Regular 字体
// （仅覆盖拉丁字符，中文会显示为方框——生产环境必须配置 CJK 字体）。bgDir 是底图目录。
func New(fontPath, bgDir string) (*Renderer, error) {
	data := goregular.TTF
	if fontPath != "" {
		b, err := os.ReadFile(fontPath)
		if err != nil {
			return nil, fmt.Errorf("font_path: %w", err)
		}
		data = b
	}
	f, err := parseFont(data)
	if err != nil {
		return nil, fmt.Errorf("font_path: cannot parse font: %w", err)
	}
	return &Renderer{font: f, bgDir: bgDir, bgs: map[bgKey]*image.RGBA{}}, nil
}

// parseFont 解析单个字体，或字体集合（.ttc，如 NotoSansCJK-Regular.ttc）——集合里优先取简体中文（SC）那一款。
func parseFont(data []byte) (*opentype.Font, error) {
	if f, err := opentype.Parse(data); err == nil {
		return f, nil
	}
	c, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	var first *opentype.Font
	for i := range c.NumFonts() {
		f, err := c.Font(i)
		if err != nil {
			continue
		}
		if name, _ := f.Name(nil, sfnt.NameIDFamily); strings.HasSuffix(name, " SC") {
			return f, nil
		}
		if first == nil {
			first = f
		}
	}
	if first == nil {
		return nil, errors.New("empty font collection")
	}
	return first, nil
}

// Render 按模板 + 设备属性合成一张图。
//
// mirror 为真时所有区域左右对调（属性在左还是在右，用同一个模板即可覆盖两种设备）。
//
// overlayMode 决定媒体区怎么画：
//   - true：留透明，作为叠加图交给设备端贴在视频之上（Go 的 image.RGBA 本身是预乘 alpha，
//     正是显示图层要的格式）
//   - false：填上自己的底色，得到一张整屏静态图——用于模板没有媒体区、或媒体区还没有内容的情形
//
// 底图（PNG）在媒体区之后、文字之前整张叠上去：媒体区里它透明的地方露出视频，不透明的装饰压在视频上。
func (r *Renderer) Render(tpl store.Template, attrs map[string]string, mirror, overlayMode bool) (*image.RGBA, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, tpl.W, tpl.H))
	fill(canvas, canvas.Bounds(), parseColor(tpl.Background))
	regions := make([]store.Region, len(tpl.Regions))
	for i, reg := range tpl.Regions {
		if mirror {
			reg = store.Mirrored(reg, tpl.W)
		}
		regions[i] = reg
		switch {
		case reg.Type != store.RegionMedia:
		case overlayMode:
			fill(canvas, reg.Rect(), color.RGBA{}) // 设备端的视频从这里透出来
		case reg.Bg != "":
			fill(canvas, reg.Rect(), parseColor(reg.Bg))
		}
	}
	if file := tpl.BackgroundFor(mirror); file != "" {
		// 底图丢了不让整个清单生成失败（设备会一直拿不到新内容）：退回纯底色并记下原因
		if bg, err := r.background(file, tpl.W, tpl.H); err != nil {
			log.Printf("template %s: background image skipped: %v", tpl.ID, err)
		} else {
			draw.Draw(canvas, canvas.Bounds(), bg, image.Point{}, draw.Over)
		}
	}

	for _, reg := range regions {
		if reg.Type == store.RegionMedia {
			continue
		}
		rect := reg.Rect()
		if reg.Bg != "" {
			fill(canvas, rect, parseColor(reg.Bg))
		}
		switch reg.Type {
		case store.RegionAttribute:
			text := attrs[reg.Key]
			if text == "" {
				text = "-"
			}
			if err := r.drawText(canvas, rect, text, reg.FontSize, parseColor(reg.Color), reg.Align); err != nil {
				return nil, err
			}
		case store.RegionText:
			if err := r.drawText(canvas, rect, reg.Key, reg.FontSize, parseColor(reg.Color), reg.Align); err != nil {
				return nil, err
			}
		}
	}
	return canvas, nil
}

// background 返回铺满 w×h 画布的底图：等比缩放到刚好盖住画布，居中裁掉多出来的一边。
func (r *Renderer) background(file string, w, h int) (*image.RGBA, error) {
	key := bgKey{file, w, h}
	r.mu.Lock()
	img := r.bgs[key]
	r.mu.Unlock()
	if img != nil {
		return img, nil
	}
	f, err := os.Open(filepath.Join(r.bgDir, file))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", file, err)
	}
	img = CoverImage(src, w, h)
	r.mu.Lock()
	if len(r.bgs) >= maxCachedBackgrounds {
		clear(r.bgs)
	}
	r.bgs[key] = img
	r.mu.Unlock()
	return img, nil
}

// cover 把 src 等比缩放到刚好盖住 dst 里的 rect，居中裁掉多出来的一边（设备端放图片、视频也是这样）。
func cover(dst *image.RGBA, rect image.Rectangle, src image.Image) {
	sb := src.Bounds()
	scale := math.Max(float64(rect.Dx())/float64(sb.Dx()), float64(rect.Dy())/float64(sb.Dy()))
	cw, ch := min(sb.Dx(), int(math.Round(float64(rect.Dx())/scale))), min(sb.Dy(), int(math.Round(float64(rect.Dy())/scale)))
	crop := image.Rect(0, 0, cw, ch).Add(sb.Min).Add(image.Pt((sb.Dx()-cw)/2, (sb.Dy()-ch)/2))
	xdraw.CatmullRom.Scale(dst, rect, src, crop, xdraw.Src, nil)
}

// CoverImage 返回铺满 w×h 的 src（保留透明通道）。
func CoverImage(src image.Image, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	cover(img, img.Bounds(), src)
	return img
}

// TransparentShare 返回 img 在 rect 里透明（alpha 低于一半）像素的比例。
func TransparentShare(img *image.RGBA, rect image.Rectangle) float64 {
	rect = rect.Intersect(img.Bounds())
	if rect.Empty() {
		return 1
	}
	n := 0
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		row := img.Pix[img.PixOffset(rect.Min.X, y):img.PixOffset(rect.Max.X, y)]
		for i := 3; i < len(row); i += 4 {
			if row[i] < 128 {
				n++
			}
		}
	}
	return float64(n) / float64(rect.Dx()*rect.Dy())
}

// Preview 把播放内容的一帧 content 按设备的方式铺进媒体区 media，再盖上叠加模式渲染出的模板 overlay，
// 得到与设备上一致的画面（含压在媒体区上的底图装饰）。
func Preview(overlay *image.RGBA, media image.Rectangle, content image.Image) *image.RGBA {
	out := image.NewRGBA(overlay.Bounds())
	fill(out, out.Bounds(), color.RGBA{A: 0xFF})
	cover(out, media, content)
	draw.Draw(out, out.Bounds(), overlay, image.Point{}, draw.Over)
	return out
}

// RenderGuide 生成底图设计参考图（画布原尺寸）：浅灰底，媒体区是红块并写明位置——底图在这块要透明，视频/图片
// 从透明处露出，装饰可以压进来；文字/属性区域描边并标注（区域没设底色时透明，透出底图）。mirror 出对调版。
func (r *Renderer) RenderGuide(tpl store.Template, mirror bool) (*image.RGBA, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, tpl.W, tpl.H))
	fill(canvas, canvas.Bounds(), color.RGBA{0xE5, 0xE5, 0xEA, 0xFF})
	for _, reg := range tpl.Regions {
		if mirror {
			reg = store.Mirrored(reg, tpl.W)
		}
		rect := reg.Rect()
		var lines []string
		var c color.RGBA
		switch reg.Type {
		case store.RegionMedia:
			c = color.RGBA{0xFF, 0x3B, 0x30, 0xFF}
			fill(canvas, rect, c)
			lines = []string{"媒体区：底图在这里要透明", fmt.Sprintf("x=%d y=%d  %d×%d", reg.X, reg.Y, reg.W, reg.H),
				"视频/图片从透明处露出", "装饰可以压进来", fmt.Sprintf("画布 %d×%d", tpl.W, tpl.H)}
			c = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
		case store.RegionAttribute:
			c = color.RGBA{0x00, 0x7A, 0xFF, 0xFF}
			lines = []string{"属性 " + reg.Key, fmt.Sprintf("x=%d y=%d  %d×%d", reg.X, reg.Y, reg.W, reg.H)}
		default:
			c = color.RGBA{0x34, 0xC7, 0x59, 0xFF}
			lines = []string{"文字：" + reg.Key, fmt.Sprintf("x=%d y=%d  %d×%d", reg.X, reg.Y, reg.W, reg.H)}
		}
		if reg.Type != store.RegionMedia {
			stroke(canvas, rect, max(2, tpl.W/360), c)
		}
		size := max(12, min(rect.Dx()/12, rect.Dy()/(2*len(lines)), 40))
		top := rect.Min.Y + (rect.Dy()-len(lines)*size*3/2)/2
		for i, l := range lines {
			row := image.Rect(rect.Min.X, top+i*size*3/2, rect.Max.X, top+(i+1)*size*3/2)
			if err := r.drawText(canvas, row, l, size, c, "center"); err != nil {
				return nil, err
			}
		}
	}
	return canvas, nil
}

// RenderTestCard 生成现场定位用的整屏测试卡：纯色底 + 大号“测试” + 设备编号与属性。
// until 按它自带的时区显示，由调用方转换成运营方配置的时区（不能用服务器操作系统的时区）。
func (r *Renderer) RenderTestCard(deviceID string, attrs map[string]string, until time.Time) (*image.RGBA, error) {
	w, h := manifest.CanvasW, manifest.CanvasH
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(canvas, canvas.Bounds(), color.RGBA{0x00, 0x66, 0xCC, 0xFF})

	type line struct {
		text string
		size int
	}
	lines := []line{{"测 试", h / 4}, {deviceID, h / 12}}
	if len(attrs) > 0 {
		var kv []string
		for k, v := range attrs {
			kv = append(kv, k+"="+v)
		}
		sort.Strings(kv) // map 遍历无序，排序保证同一输入渲染结果字节级一致（版本号稳定）
		lines = append(lines, line{strings.Join(kv, "  "), h / 18})
	}
	lines = append(lines, line{"至 " + until.Format("15:04:05"), h / 20})

	total := 0
	for _, l := range lines {
		total += l.size * 3 / 2
	}
	y := (h - total) / 2
	white := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	for _, l := range lines {
		rect := image.Rect(0, y, w, y+l.size*3/2)
		if err := r.drawText(canvas, rect, l.text, l.size, white, "center"); err != nil {
			return nil, err
		}
		y += l.size * 3 / 2
	}
	return canvas, nil
}

// drawText 在 rect 内绘制单行文字（水平按 align，垂直居中）。
func (r *Renderer) drawText(dst *image.RGBA, rect image.Rectangle, text string, size int, c color.Color, align string) error {
	face, err := opentype.NewFace(r.font, &opentype.FaceOptions{
		Size: float64(size), DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		return err
	}
	defer face.Close()

	d := &font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: face}
	width := d.MeasureString(text).Ceil()
	var x int
	switch align {
	case "left":
		x = rect.Min.X + size/4
	case "right":
		x = rect.Max.X - width - size/4
	default:
		x = rect.Min.X + (rect.Dx()-width)/2
	}
	metrics := face.Metrics()
	baseline := rect.Min.Y + (rect.Dy()+metrics.Ascent.Ceil()-metrics.Descent.Ceil())/2
	d.Dot = fixed.P(x, baseline)
	d.DrawString(text)
	return nil
}

func fill(dst *image.RGBA, rect image.Rectangle, c color.Color) {
	draw.Draw(dst, rect, image.NewUniform(c), image.Point{}, draw.Src)
}

// stroke 画 rect 的边框（向内 t 像素）。
func stroke(dst *image.RGBA, rect image.Rectangle, t int, c color.Color) {
	fill(dst, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+t), c)
	fill(dst, image.Rect(rect.Min.X, rect.Max.Y-t, rect.Max.X, rect.Max.Y), c)
	fill(dst, image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+t, rect.Max.Y), c)
	fill(dst, image.Rect(rect.Max.X-t, rect.Min.Y, rect.Max.X, rect.Max.Y), c)
}

// parseColor 解析 "#RRGGBB"（store 已校验格式，异常时退回黑色）。
func parseColor(s string) color.RGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		return color.RGBA{A: 0xFF}
	}
	return color.RGBA{r, g, b, 0xFF}
}
