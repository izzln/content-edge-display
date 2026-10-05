package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/testpdf"
	"github.com/izzln/content-edge-display/internal/transcode"
)

func realPDF(t *testing.T, s *Server) {
	t.Helper()
	r, err := transcode.FindPDF()
	if err != nil {
		t.Skipf("poppler-utils 不可用：%v", err)
	}
	s.setPDFRenderer(r)
}

// PDF 上传后在后台逐页渲染；完成后在播放列表里是一项（带页数），下发给设备时展开成逐页图片，
// 与图片同样的停留时长；设备能下载页面，后台能看每页缩略图；删除时页面一并删掉。
func TestPDFUploadIsRenderedIntoPages(t *testing.T) {
	s, h := newAdminTestServer(t)
	realPDF(t, s)
	doc := testpdf.Make(testpdf.A4Portrait, testpdf.A4Landscape, testpdf.A4Portrait)

	res := parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"a.jpg", tinyPNG(t)}, upload{"季度 报告.pdf", doc}))
	if strings.Join(res.Queued, ",") != "季度 报告.pdf" || len(res.Rejected) != 0 {
		t.Fatalf("PDF 应进入后台处理队列：%+v", res)
	}
	files := waitMedia(t, h, "PDF 渲染完成", func(fs []MediaFile) bool {
		f, ok := byName(fs, "季度 报告.pdf")
		return ok && f.Status == mediaReady
	})
	if f, _ := byName(files, "季度 报告.pdf"); f.Type != "document" || f.Pages != 3 || f.Size != int64(len(doc)) {
		t.Fatalf("文档条目：%+v", f)
	}

	m := deviceManifest(t, h)
	var names []string
	for _, it := range m.Items {
		names = append(names, it.Name)
		if it.Type != "image" || it.Duration != m.Items[0].Duration || it.Duration <= 0 {
			t.Fatalf("页面应是与图片同样停留时长的图片条目：%+v", m.Items)
		}
	}
	if strings.Join(names, ",") != "a.jpg,季度 报告-p001.jpg,季度 报告-p002.jpg,季度 报告-p003.jpg" {
		t.Fatalf("清单条目 %v", names)
	}
	// 设备按清单里的 URL 下载页面
	w := do(t, h, signedRequest("GET", m.Items[2].URL, nil), http.StatusOK)
	if w.Header().Get("Content-Type") != "image/jpeg" || w.Body.Len() != int(m.Items[2].Size) {
		t.Fatalf("页面下载：%s，%d 字节（清单 %d）", w.Header().Get("Content-Type"), w.Body.Len(), m.Items[2].Size)
	}
	do(t, h, signedRequest("GET", "/media/"+testDeviceID+"/a.jpg/p001.jpg", nil), http.StatusBadRequest)
	// 后台缩略图：默认第 1 页，可指定页，越界 404
	base := "/api/v1/admin/devices/" + testDeviceID + "/media/" + "季度%20报告.pdf/thumb"
	do(t, h, adminReq("GET", base, nil), http.StatusOK)
	do(t, h, adminReq("GET", base+"?page=3", nil), http.StatusOK)
	do(t, h, adminReq("GET", base+"?page=4", nil), http.StatusNotFound)

	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/"+"季度%20报告.pdf", nil), http.StatusOK)
	if _, err := os.Stat(filepath.Join(s.deviceMediaDir(testDeviceID), manifest.PagesDir, "季度 报告.pdf")); !os.IsNotExist(err) {
		t.Fatal("删除 PDF 应一并删掉页面")
	}
	if m := deviceManifest(t, h); len(m.Items) != 1 {
		t.Fatalf("删除后清单只剩图片：%+v", m.Items)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.incomingDir(), testDeviceID)); len(entries) != 0 {
		t.Fatalf("暂存区应已清空，剩 %d 个文件", len(entries))
	}
}

// 坏文件、加密、页数超限：任务失败，原因用运营方看得懂的话显示在列表里，可以删掉。
func TestPDFFailuresAreExplained(t *testing.T) {
	s, h := newAdminTestServer(t)
	realPDF(t, s)
	pages := make([]testpdf.Page, maxPDFPages+1)
	for i := range pages {
		pages[i] = testpdf.A4Landscape
	}
	uploads := []upload{{"broken.pdf", []byte("%PDF-1.4 not really")}, {"huge.pdf", testpdf.Make(pages...)}}
	want := map[string]string{"broken.pdf": "无法解析", "huge.pdf": "超过上限"}
	if _, err := exec.LookPath("qpdf"); err == nil {
		dir := t.TempDir()
		plain, locked := filepath.Join(dir, "p.pdf"), filepath.Join(dir, "l.pdf")
		os.WriteFile(plain, testpdf.Make(testpdf.A4Portrait), 0o644)
		if exec.Command("qpdf", "--encrypt", "pw", "owner", "256", "--", plain, locked).Run() == nil {
			data, _ := os.ReadFile(locked)
			uploads = append(uploads, upload{"locked.pdf", data})
			want["locked.pdf"] = "密码保护"
		}
	}
	parseUpload(t, uploadMedia(t, h, testDeviceID, uploads...))
	files := waitMedia(t, h, "全部失败", func(fs []MediaFile) bool {
		for name := range want {
			if f, ok := byName(fs, name); !ok || f.Status != jobFailed {
				return false
			}
		}
		return true
	})
	for name, w := range want {
		if f, _ := byName(files, name); !strings.Contains(f.Error, w) || f.Type != "document" {
			t.Errorf("%s：%+v，原因应包含 %q", name, f, w)
		}
	}
	for _, it := range deviceManifest(t, h).Items {
		if strings.Contains(it.URL, ".pdf") {
			t.Fatalf("失败的 PDF 不能下发：%+v", it)
		}
	}
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/broken.pdf", nil), http.StatusOK)
	if _, ok := byName(listMedia(t, h), "broken.pdf"); ok {
		t.Fatal("删除后失败的任务应从列表消失")
	}
}

// blockingPDF 渲染到一半卡住，直到被取消（模拟运营方在渲染中删除）。
type blockingPDF struct{ started chan struct{} }

func (b *blockingPDF) Version() string                            { return "blocking" }
func (b *blockingPDF) Pages(context.Context, string) (int, error) { return 2, nil }
func (b *blockingPDF) Render(ctx context.Context, _, dir string, _ int, onPage func(int)) error {
	os.WriteFile(filepath.Join(dir, "p001.jpg"), []byte("x"), 0o644)
	onPage(1)
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestDeleteWhileRenderingPDFCancels(t *testing.T) {
	s, h := newAdminTestServer(t)
	r := &blockingPDF{started: make(chan struct{})}
	s.setPDFRenderer(r)
	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"doc.pdf", testpdf.Make(testpdf.A4Portrait)}))
	<-r.started
	if f, _ := byName(listMedia(t, h), "doc.pdf"); f.Status != jobRunning || f.Pages != 2 || f.Progress != 50 {
		t.Fatalf("渲染中应显示页数与进度：%+v", f)
	}
	do(t, h, adminReq("DELETE", "/api/v1/admin/devices/"+testDeviceID+"/media/doc.pdf", nil), http.StatusOK)
	time.Sleep(100 * time.Millisecond) // 给工作协程收尾的时间
	dir := s.deviceMediaDir(testDeviceID)
	if entries, _ := os.ReadDir(filepath.Join(dir, manifest.PagesDir)); len(entries) != 0 {
		t.Fatalf("被取消的渲染不应留下页面：%d 项", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "doc.pdf")); !os.IsNotExist(err) {
		t.Fatal("被取消的渲染不应留下 PDF")
	}
}

// 没装 poppler-utils：PDF 当场拒收并说明原因，后台信息里也能看到。
func TestPDFRejectedWithoutPoppler(t *testing.T) {
	_, h := newAdminTestServer(t)
	res := parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"doc.pdf", testpdf.Make(testpdf.A4Portrait)}))
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Reason, "poppler-utils") {
		t.Fatalf("应拒收并提示安装 poppler-utils：%+v", res)
	}
	var info struct {
		PDF      bool   `json:"pdf"`
		PDFError string `json:"pdf_error"`
	}
	json.Unmarshal(do(t, h, adminReq("GET", "/api/v1/admin/info", nil), http.StatusOK).Body.Bytes(), &info)
	if info.PDF || info.PDFError == "" {
		t.Fatalf("后台信息应报告 PDF 不可用及原因：%+v", info)
	}
}
