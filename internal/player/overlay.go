package player

import (
	"fmt"
	"image"
	"image/png"
	"os"

	xdraw "golang.org/x/image/draw"
)

// rasterize 把服务端下发的叠加 PNG 变成 mpv 能直接贴的原始像素，并缓存在 PNG 旁边。
//
// 两件事必须在这里做：
//
//  1. 格式。mpv 的 overlay-add 只吃原始像素（预乘 alpha 的 bgra），不认 PNG。
//
//  2. 尺寸。overlay-add 的坐标是 **mpv 实际输出分辨率**（osd-dimensions）下的像素，
//     而叠加图是按模板画布尺寸（1440×900）渲染的。显示屏真实输出不一定等于画布——
//     面板 EDID 报的是 1920×1080、内核没吃下 video= 参数、换了块屏，都会导致两者不一致。
//     不缩放的话叠加图会贴在左上角那一块，画面整体错位。所以这里按实际输出尺寸缩放，
//     与 --video-margin-ratio-*（本来就是比例）对齐。
//
// 文件名带上目标尺寸：分辨率变了会生成新文件，不会用到旧的。
func rasterize(pngPath string, w, h int) (string, error) {
	if w <= 0 || h <= 0 {
		return "", fmt.Errorf("overlay: invalid target size %dx%d", w, h)
	}
	raw := fmt.Sprintf("%s.%dx%d.bgra", pngPath, w, h)
	if fi, err := os.Stat(raw); err == nil && fi.Size() == int64(w)*int64(h)*4 {
		return raw, nil // 已经转换过
	}

	f, err := os.Open(pngPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return "", fmt.Errorf("decode %s: %w", pngPath, err)
	}

	// 统一先落到目标尺寸的 RGBA（Go 的 image.RGBA 就是预乘 alpha，正合 mpv 的要求），
	// 再换成 BGRA 的字节序。
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if src.Bounds().Dx() == w && src.Bounds().Dy() == h {
		xdraw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, xdraw.Src)
	} else {
		// 叠加图是整屏构图，必须整体拉伸到输出分辨率（不能保持比例留边，
		// 否则属性区和媒体区的分界线会跟视频对不上）。
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	}

	buf := make([]byte, w*h*4)
	for i := 0; i < len(buf); i += 4 {
		buf[i], buf[i+1], buf[i+2], buf[i+3] = dst.Pix[i+2], dst.Pix[i+1], dst.Pix[i], dst.Pix[i+3]
	}
	tmp := raw + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, raw); err != nil {
		return "", err
	}
	return raw, nil
}
