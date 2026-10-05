package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pngBytes(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func uploadBackground(t *testing.T, h http.Handler, tplID string, mirror bool, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "bg.png")
	fw.Write(data)
	mw.Close()
	path := "/api/v1/admin/templates/" + tplID + "/background"
	if mirror {
		path += "?mirror=1"
	}
	r := httptest.NewRequest("POST", path, &buf)
	r.Header.Set("X-Admin-Token", adminToken)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// 底图：上传即生效（清单版本跟着变），先有常规版才能传对调版；参考图按画布尺寸出；
// 不再被引用的文件回收；JSON 里引用不存在的底图被拒。
func TestTemplateBackground(t *testing.T) {
	s, h := newAdminTestServer(t)
	tplID := state(s).Global.TemplateID
	before := deviceManifest(t, h).Version

	if w := uploadBackground(t, h, tplID, true, pngBytes(t, 32, 20, color.RGBA{0, 0xFF, 0, 0xFF})); w.Code != http.StatusBadRequest {
		t.Fatalf("没有常规底图时不能传对调版：%d %s", w.Code, w.Body.String())
	}
	if w := uploadBackground(t, h, tplID, false, []byte("not an image")); w.Code != http.StatusBadRequest {
		t.Fatalf("非图片应被拒：%d", w.Code)
	}
	w := uploadBackground(t, h, tplID, false, pngBytes(t, 32, 20, color.RGBA{0xFF, 0, 0, 0xFF}))
	var got struct {
		File string `json:"file"`
		W, H int
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || !strings.HasPrefix(got.File, "bg-") || got.W != 32 || state(s).Templates[tplID].BackgroundImage != got.File {
		t.Fatalf("底图应上传成功并写进模板：%d %s", w.Code, w.Body.String())
	}
	if after := deviceManifest(t, h).Version; after == before {
		t.Fatal("换了底图，设备清单版本应变化")
	}
	if w := do(t, h, adminReq("GET", "/api/v1/admin/backgrounds/"+got.File, nil), http.StatusOK); w.Body.Len() == 0 {
		t.Fatal("后台应能取回底图原图")
	}
	if w := uploadBackground(t, h, tplID, true, pngBytes(t, 32, 20, color.RGBA{0, 0xFF, 0, 0xFF})); w.Code != http.StatusOK {
		t.Fatalf("对调版底图应上传成功：%d %s", w.Code, w.Body.String())
	}

	gw := do(t, h, adminReq("GET", "/api/v1/admin/templates/"+tplID+"/guide?mirror=1", nil), http.StatusOK)
	guide, err := png.Decode(gw.Body)
	if tpl := state(s).Templates[tplID]; err != nil || guide.Bounds().Dx() != tpl.W || guide.Bounds().Dy() != tpl.H {
		t.Fatalf("参考图应是画布尺寸的 PNG：%v", err)
	}

	// JSON 编辑里引用不存在的底图：拒绝
	tpl := state(s).Templates[tplID]
	tpl.BackgroundImage = "bg-00000000000000ff.png"
	if w := do2(t, h, adminReq("PUT", "/api/v1/admin/templates/"+tplID, tpl)); w.Code != http.StatusBadRequest {
		t.Fatalf("引用不存在的底图应被拒：%d", w.Code)
	}

	// 去掉常规底图：对调版一并去掉；文件过了宽限期后被回收
	files := func() []string {
		e, _ := os.ReadDir(s.backgroundsDir())
		var out []string
		for _, f := range e {
			out = append(out, f.Name())
		}
		return out
	}
	for _, f := range files() {
		old := time.Now().Add(-time.Hour)
		os.Chtimes(filepath.Join(s.backgroundsDir(), f), old, old)
	}
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/"+tplID+"/background", nil), http.StatusNoContent)
	if tpl := state(s).Templates[tplID]; tpl.BackgroundImage != "" || tpl.BackgroundImageMirror != "" {
		t.Fatalf("去掉常规底图时对调版应一并去掉：%+v", tpl)
	}
	if left := files(); len(left) != 0 {
		t.Fatalf("没有模板引用的底图应被回收：%v", left)
	}
}
