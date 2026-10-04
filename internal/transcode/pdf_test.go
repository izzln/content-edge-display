package transcode

import (
	"context"
	"errors"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/izzln/content-edge-display/internal/testpdf"
)

// needPDF 在没装 poppler-utils 的环境里跳过。
func needPDF(t *testing.T) *PDFRenderer {
	t.Helper()
	r, err := FindPDF()
	if err != nil {
		t.Skipf("poppler-utils 不可用，跳过 PDF 测试：%v", err)
	}
	return r
}

// 逐页渲染成 1440 宽的 JPEG，横竖混排的页面各自保持比例，每页回调一次进度。
func TestPDFRenderPages(t *testing.T) {
	r := needPDF(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.pdf")
	os.WriteFile(src, testpdf.Make(testpdf.A4Portrait, testpdf.A4Landscape, testpdf.A4Portrait), 0o644)

	n, err := r.Pages(context.Background(), src)
	if err != nil || n != 3 {
		t.Fatalf("页数 %d，错误 %v", n, err)
	}
	out := t.TempDir()
	var done []int
	if err := r.Render(context.Background(), src, out, n, func(d int) { done = append(done, d) }); err != nil {
		t.Fatal(err)
	}
	if len(done) != 3 || done[2] != 3 {
		t.Fatalf("进度回调 %v", done)
	}
	for i, want := range [][2]int{{1440, 2037}, {1440, 1018}, {1440, 2037}} {
		f, err := os.Open(filepath.Join(out, PageName(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		// 高度按比例取整，允许差一个像素
		if cfg.Width != want[0] || cfg.Height < want[1]-1 || cfg.Height > want[1]+1 {
			t.Errorf("第 %d 页 %dx%d，期望约 %dx%d", i+1, cfg.Width, cfg.Height, want[0], want[1])
		}
	}
}

func TestPDFBrokenAndEncrypted(t *testing.T) {
	r := needPDF(t)
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.pdf")
	os.WriteFile(broken, []byte("%PDF-1.4 not really"), 0o644)
	if _, err := r.Pages(context.Background(), broken); err == nil {
		t.Fatal("损坏的 PDF 应报错")
	}

	if _, err := exec.LookPath("qpdf"); err != nil {
		t.Skip("没有 qpdf，造不了加密 PDF")
	}
	plain, locked := filepath.Join(dir, "plain.pdf"), filepath.Join(dir, "locked.pdf")
	os.WriteFile(plain, testpdf.Make(testpdf.A4Portrait), 0o644)
	if out, err := exec.Command("qpdf", "--encrypt", "secret", "owner", "256", "--", plain, locked).CombinedOutput(); err != nil {
		t.Skipf("qpdf 加密失败：%v %s", err, out)
	}
	if _, err := r.Pages(context.Background(), locked); !errors.Is(err, ErrPDFEncrypted) {
		t.Fatalf("加密 PDF 应报 ErrPDFEncrypted，得到 %v", err)
	}
}
