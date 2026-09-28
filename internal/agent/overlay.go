package agent

import (
	"fmt"
	"image"
	"image/png"
	"os"

	"github.com/izzln/content-edge-display/internal/player"
)

// overlaySuffix 是叠加层原始像素文件的后缀（与其 PNG 同目录、同生命周期）。
const overlaySuffix = ".bgra"

// prepareOverlay 把服务端下发的叠加 PNG 解码成 BGRA 原始像素并落盘：
// mpv 的 overlay-add 只吃原始像素（预乘 alpha 的 bgra），不认 PNG。
//
// 解码一次落盘复用：mpv 每次重启都要重贴叠加层，而 PNG 内容不变时文件名也不变。
func prepareOverlay(pngPath string) (*player.Overlay, error) {
	f, err := os.Open(pngPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", pngPath, err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("overlay %s: 空图像", pngPath)
	}

	raw := pngPath + overlaySuffix
	if fi, err := os.Stat(raw); err == nil && fi.Size() == int64(w*h*4) {
		return &player.Overlay{Path: raw, W: w, H: h}, nil // 已解码过
	}

	buf := make([]byte, w*h*4)
	i := 0
	if src, ok := img.(*image.RGBA); ok {
		// image.RGBA 已是预乘 alpha，只需把 RGBA 换成 BGRA 的字节序。
		for y := 0; y < h; y++ {
			row := src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):][:w*4]
			for x := 0; x < w*4; x += 4 {
				buf[i], buf[i+1], buf[i+2], buf[i+3] = row[x+2], row[x+1], row[x], row[x+3]
				i += 4
			}
		}
	} else {
		// 其它图像类型走通用路径：Color.RGBA() 返回的也是预乘值。
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, a := img.At(x, y).RGBA()
				buf[i], buf[i+1], buf[i+2], buf[i+3] = byte(bl>>8), byte(g>>8), byte(r>>8), byte(a>>8)
				i += 4
			}
		}
	}

	tmp := raw + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, raw); err != nil {
		return nil, err
	}
	return &player.Overlay{Path: raw, W: w, H: h}, nil
}
