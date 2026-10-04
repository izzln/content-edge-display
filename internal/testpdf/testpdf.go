// Package testpdf 现造测试用的 PDF（只给测试用）：每页一个指定尺寸、整页填一种颜色。
package testpdf

import (
	"bytes"
	"fmt"
)

// Page 是一页的尺寸（PDF 点，1/72 英寸）与填充色（0~1）。
type Page struct {
	W, H    int
	R, G, B float64
}

// A4 竖版与横版。
var (
	A4Portrait  = Page{W: 595, H: 842, R: 1}
	A4Landscape = Page{W: 842, H: 595, G: 1}
)

// Make 生成一个包含 pages 的合法 PDF。
func Make(pages ...Page) []byte {
	var buf bytes.Buffer
	var offsets []int
	obj := func(body string) int {
		offsets = append(offsets, buf.Len())
		n := len(offsets)
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", n, body)
		return n
	}
	buf.WriteString("%PDF-1.4\n")
	// 对象编号：1 目录，2 页树，之后每页两个对象（页面、内容流）
	kids := ""
	for i := range pages {
		kids += fmt.Sprintf("%d 0 R ", 3+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(pages)))
	for i, p := range pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Contents %d 0 R >>", p.W, p.H, 4+2*i))
		stream := fmt.Sprintf("%.2f %.2f %.2f rg 0 0 %d %d re f", p.R, p.G, p.B, p.W, p.H)
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return buf.Bytes()
}
