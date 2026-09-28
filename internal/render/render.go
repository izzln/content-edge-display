// Package render 在服务端把显示模板/测试卡合成为 1440×900 位图，
// 使渲染结果作为普通图片走既有 manifest→下载→播放管线，设备端零改动。
package render

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/izzln/content-edge-display/internal/store"
)

// Renderer 持有字体与上传目录。
type Renderer struct {
	font       *opentype.Font
	uploadsDir string
}

// New 创建渲染器。fontPath 为空时退回内嵌的 Go Regular 字体
// （仅覆盖拉丁字符，中文会显示为方框——生产环境必须配置 CJK 字体）。
func New(fontPath, uploadsDir string) (*Renderer, error) {
	data := goregular.TTF
	if fontPath != "" {
		b, err := os.ReadFile(fontPath)
		if err != nil {
			return nil, fmt.Errorf("font_path: %w", err)
		}
		data = b
	}
	f, err := opentype.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("font_path: 解析字体失败: %w", err)
	}
	return &Renderer{font: f, uploadsDir: uploadsDir}, nil
}

// Rendered 是一次模板渲染的产物。
type Rendered struct {
	Image       *image.RGBA
	MediaRegion image.Rectangle // 媒体区在画布上的位置；HasMedia 为 false 时无意义
	HasMedia    bool
}

// Render 按模板 + 设备属性 + 区域绑定合成一张图。
//
// mirror 为真时所有区域左右对调（属性在左还是在右，用同一个模板即可覆盖两种设备）。
//
// overlayMode 决定媒体区怎么画：
//   - true：留全透明，作为叠加图交给设备端贴在视频之上（Go 的 image.RGBA 本身是预乘 alpha，
//     正是 mpv overlay-add 需要的格式）
//   - false：填上自己的底色，得到一张整屏静态图——用于模板没有媒体区、或媒体区还没有内容的情形
func (r *Renderer) Render(tpl store.Template, attrs, bindings map[string]string, mirror, overlayMode bool) (*Rendered, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, tpl.W, tpl.H))
	out := &Rendered{Image: canvas}
	fill(canvas, canvas.Bounds(), parseColor(tpl.Background))

	for _, reg := range tpl.Regions {
		if mirror {
			reg = store.Mirrored(reg, tpl.W)
		}
		rect := image.Rect(reg.X, reg.Y, reg.X+reg.W, reg.Y+reg.H)

		if reg.Type == store.RegionMedia {
			out.MediaRegion, out.HasMedia = rect, true
			if overlayMode {
				// 挖洞：透明黑，设备端的视频从这里透出来
				draw.Draw(canvas, rect, image.NewUniform(color.RGBA{}), image.Point{}, draw.Src)
			} else if reg.Bg != "" {
				fill(canvas, rect, parseColor(reg.Bg))
			}
			continue
		}

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
		case store.RegionImage:
			name := bindings[reg.ID]
			if name == "" {
				continue // 未绑定内容的图片区域留区域底色
			}
			img, err := r.loadUpload(name)
			if err != nil {
				return nil, fmt.Errorf("region %q: %w", reg.ID, err)
			}
			drawCover(canvas, rect, img)
		}
	}
	return out, nil
}

// RenderTestCard 生成现场定位用的测试卡：纯色底 + 大号“测试” + 设备信息。
func (r *Renderer) RenderTestCard(w, h int, deviceID, deviceName string, attrs map[string]string, until time.Time) (*image.RGBA, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(canvas, canvas.Bounds(), color.RGBA{0x00, 0x66, 0xCC, 0xFF})

	lines := []struct {
		text string
		size int
	}{
		{"测 试", h / 4},
		{deviceName + "  (" + deviceID + ")", h / 12},
	}
	var attrLine []string
	for k, v := range attrs {
		attrLine = append(attrLine, k+"="+v)
	}
	if len(attrLine) > 0 {
		// map 遍历无序，排序保证同一输入渲染结果字节级一致（版本号稳定）。
		sort.Strings(attrLine)
		lines = append(lines, struct {
			text string
			size int
		}{strings.Join(attrLine, "  "), h / 18})
	}
	lines = append(lines, struct {
		text string
		size int
	}{"至 " + until.Local().Format("15:04:05"), h / 20})

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

// EncodePNG 把渲染结果写为 PNG。
func EncodePNG(w io.Writer, img image.Image) error {
	return png.Encode(w, img)
}

func (r *Renderer) loadUpload(name string) (image.Image, error) {
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("非法文件名 %q", name)
	}
	f, err := os.Open(filepath.Join(r.uploadsDir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return png.Decode(f)
	case ".jpg", ".jpeg":
		return jpeg.Decode(f)
	default:
		return nil, fmt.Errorf("不支持的图片格式 %q", name)
	}
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

// drawCover 让图片撑满 rect：按较大的那个缩放比取源图中央的一块，超出的部分裁掉，
// 不留黑边（等同 CSS 的 object-fit: cover）。设备端播放媒体区内容时用的 --panscan=1 是同样的效果。
func drawCover(dst *image.RGBA, rect image.Rectangle, src image.Image) {
	sb := src.Bounds()
	if sb.Dx() == 0 || sb.Dy() == 0 || rect.Empty() {
		return
	}
	// 需要用到的源图尺寸：以 rect 的宽高比为准，从源图中央裁一块
	srcW, srcH := sb.Dx(), sb.Dy()
	wantW, wantH := srcW, srcH
	if srcW*rect.Dy() > rect.Dx()*srcH {
		wantW = srcH * rect.Dx() / rect.Dy() // 源图更宽：按高撑满，左右各裁掉一部分
	} else {
		wantH = srcW * rect.Dy() / rect.Dx() // 源图更高：按宽撑满，上下各裁掉一部分
	}
	sx := sb.Min.X + (srcW-wantW)/2
	sy := sb.Min.Y + (srcH-wantH)/2
	crop := image.Rect(sx, sy, sx+wantW, sy+wantH)
	xdraw.CatmullRom.Scale(dst, rect, src, crop, draw.Over, nil)
}

func fill(dst *image.RGBA, rect image.Rectangle, c color.Color) {
	draw.Draw(dst, rect, image.NewUniform(c), image.Point{}, draw.Src)
}

// parseColor 解析 "#RRGGBB"（store 已校验格式，异常时退回黑色）。
func parseColor(s string) color.RGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		return color.RGBA{A: 0xFF}
	}
	return color.RGBA{r, g, b, 0xFF}
}
