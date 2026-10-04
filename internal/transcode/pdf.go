package transcode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// PageWidth 是 PDF 页面渲染出的宽度（像素，高度按比例）：与画布同宽，设备照常按 cover 裁剪缩放。
const PageWidth = manifest.CanvasW

// ErrPDFEncrypted 表示 PDF 有密码保护，渲染不了。
var ErrPDFEncrypted = errors.New("PDF is password protected")

// PDFRenderer 用 poppler-utils（pdfinfo 取页数、pdftoppm 渲染）把 PDF 逐页渲染成 JPEG。
type PDFRenderer struct {
	info, toppm string
	version     string
}

// FindPDF 在 PATH 里定位并试运行 pdfinfo 与 pdftoppm。不可用时返回的错误说明了具体原因
// （与 ffmpeg 一样，"装了却找不到"多半是服务以 display 用户运行、PATH 不同）。
func FindPDF() (*PDFRenderer, error) {
	r := &PDFRenderer{}
	var err error
	if r.info, err = lookTool("pdfinfo"); err != nil {
		return nil, err
	}
	if r.toppm, err = lookTool("pdftoppm"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// -v 打印版本后以非零码退出（老版本）或零退出（新版本），只看有没有输出
	out, _ := exec.CommandContext(ctx, r.toppm, "-v").CombinedOutput()
	if r.version = firstLine(out); !strings.Contains(r.version, "pdftoppm") {
		return nil, fmt.Errorf("found %s but it fails to run as user %s: %s", r.toppm, currentUser(), r.version)
	}
	return r, nil
}

// Version 返回 pdftoppm 的版本行（启动日志用）。
func (r *PDFRenderer) Version() string { return r.version }

var pagesLine = regexp.MustCompile(`(?m)^Pages:\s+(\d+)`)

// Pages 返回 PDF 的页数。
func (r *PDFRenderer) Pages(ctx context.Context, src string) (int, error) {
	out, err := exec.CommandContext(ctx, r.info, src).CombinedOutput()
	if strings.Contains(string(out), "Incorrect password") {
		return 0, ErrPDFEncrypted
	}
	if err != nil {
		return 0, fmt.Errorf("pdfinfo: %v: %s", err, firstLine(out))
	}
	m := pagesLine.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("pdfinfo reported no page count: %s", firstLine(out))
	}
	return strconv.Atoi(string(m[1]))
}

// pageStem 是第 n 页（从 1 起）渲染产物去掉 .jpg 的名字（pdftoppm 自己加后缀）。
func pageStem(n int) string { return fmt.Sprintf("p%03d", n) }

// PageName 是第 n 页（从 1 起）渲染产物的文件名。
func PageName(n int) string { return pageStem(n) + ".jpg" }

// Render 把 src 的前 pages 页逐页渲染到 dir（p001.jpg …），每完成一页回调一次（可为 nil）。
// 逐页调用 pdftoppm 而不是一次渲染全部，是为了能报进度、能随时取消。
func (r *PDFRenderer) Render(ctx context.Context, src, dir string, pages int, onPage func(done int)) error {
	for n := 1; n <= pages; n++ {
		page := strconv.Itoa(n)
		prefix := filepath.Join(dir, pageStem(n))
		out, err := exec.CommandContext(ctx, r.toppm, "-f", page, "-l", page, "-singlefile",
			"-jpeg", "-jpegopt", "quality=90", "-scale-to-x", strconv.Itoa(PageWidth), "-scale-to-y", "-1",
			src, prefix).CombinedOutput()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("pdftoppm page %d: %v: %s", n, err, firstLine(out))
		}
		if onPage != nil {
			onPage(n)
		}
	}
	return nil
}
