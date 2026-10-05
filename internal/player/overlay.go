package player

import (
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
)

// rasterize 把服务端下发的叠加 PNG 变成播放进程能直接推给显示图层的原始像素，并缓存在 PNG 旁边。
//
// 两件事必须在这里做：
//
//  1. 格式。上层图层吃原始像素（预乘 alpha 的 BGRA，即 DRM 的 ARGB8888），播放进程不必再解 PNG。
//
//  2. 尺寸。上层图层是整屏的，坐标是 **显示屏实际输出分辨率** 下的像素，
//     而叠加图是按模板画布尺寸渲染的。显示屏真实输出不一定等于画布——
//     面板 EDID 报的是 1920×1080、内核没吃下 video= 参数、换了块屏，都会导致两者不一致。
//     不缩放的话叠加图会贴在左上角那一块，画面整体错位。所以这里按实际输出尺寸缩放，
//     媒体区（画布坐标，下层图层的位置）也按同一比例换算成输出坐标（返回值 hole），两者始终对齐。
//
// 文件名带上目标尺寸：分辨率变了会生成新文件，旧尺寸的顺手删掉。
func rasterize(pngPath string, w, h int, media manifest.Rect) (raw string, hole manifest.Rect, err error) {
	if w <= 0 || h <= 0 {
		return "", hole, fmt.Errorf("overlay: invalid target size %dx%d", w, h)
	}
	f, err := os.Open(pngPath)
	if err != nil {
		return "", hole, err
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return "", hole, fmt.Errorf("decode %s: %w", pngPath, err)
	}
	sx, sy := float64(w)/float64(cfg.Width), float64(h)/float64(cfg.Height)
	scale := func(v int, s float64) int { return int(math.Round(float64(v) * s)) }
	hole = manifest.Rect{X: scale(media.X, sx), Y: scale(media.Y, sy), W: scale(media.W, sx), H: scale(media.H, sy)}

	raw = fmt.Sprintf("%s.%dx%d.bgra", pngPath, w, h)
	if fi, err := os.Stat(raw); err == nil && fi.Size() == int64(w)*int64(h)*4 {
		return raw, hole, nil // 已经转换过
	}
	if _, err := f.Seek(0, 0); err != nil {
		return "", hole, err
	}
	src, err := png.Decode(f)
	if err != nil {
		return "", hole, fmt.Errorf("decode %s: %w", pngPath, err)
	}

	// 统一先落到目标尺寸的 RGBA（Go 的 image.RGBA 就是预乘 alpha，正合显示图层的要求），
	// 再换成 BGRA 的字节序。
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if src.Bounds().Dx() == w && src.Bounds().Dy() == h {
		xdraw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, xdraw.Src)
	} else {
		// 叠加图是整屏构图，必须整体拉伸到输出分辨率（不能保持比例留边，
		// 否则属性区和媒体区的分界线会跟视频对不上）。
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	}

	for i := 0; i < len(dst.Pix); i += 4 { // RGBA → BGRA，原地交换
		dst.Pix[i], dst.Pix[i+2] = dst.Pix[i+2], dst.Pix[i]
	}
	if err := fsutil.WriteFile(raw, dst.Pix, 0o644); err != nil {
		return "", hole, err
	}
	stale, _ := filepath.Glob(pngPath + ".*.bgra") // 别的输出分辨率留下的
	for _, p := range stale {
		if p != raw {
			os.Remove(p)
		}
	}
	return raw, hole, nil
}
