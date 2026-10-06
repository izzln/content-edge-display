package server

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/testutil"
)

// bgPNG 造一张 w×h 的底图：opaqueFrac 指左边多少比例不透明（填 c），其余透明。
// 默认模板的媒体区在右半边，所以 0.5 = 媒体区全透明；对调版的媒体区在左半边，要反过来。
func bgPNG(t *testing.T, w, h int, opaqueFrac float64, c color.RGBA, mirrorSide bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	cut := int(float64(w) * opaqueFrac)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x < cut) != mirrorSide {
				img.Set(x, y, c)
			}
		}
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

	red, green := color.RGBA{0xFF, 0, 0, 0xFF}, color.RGBA{0, 0xFF, 0, 0xFF}
	if w := uploadBackground(t, h, tplID, true, bgPNG(t, 32, 20, 0.5, green, true)); w.Code != http.StatusBadRequest {
		t.Fatalf("没有常规底图时不能传对调版：%d %s", w.Code, w.Body.String())
	}
	var jpg bytes.Buffer
	jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 32, 20)), nil)
	for name, data := range map[string][]byte{"非图片": []byte("not an image"), "JPG": jpg.Bytes(),
		"媒体区完全不透明": bgPNG(t, 32, 20, 1, red, false)} {
		if w := uploadBackground(t, h, tplID, false, data); w.Code != http.StatusBadRequest {
			t.Fatalf("%s应被拒：%d %s", name, w.Code, w.Body.String())
		}
	}
	// 媒体区只有约 25% 透明：照收，但返回比例供后台提示
	if w := uploadBackground(t, h, tplID, false, bgPNG(t, 32, 20, 0.875, red, false)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"media_transparent":0.2`) {
		t.Fatalf("透明不足一半应照收并返回比例：%d %s", w.Code, w.Body.String())
	}
	w := uploadBackground(t, h, tplID, false, bgPNG(t, 32, 20, 0.5, red, false))
	file := state(s).Templates[tplID].BackgroundImage
	if w.Code != http.StatusOK || !strings.HasPrefix(file, "bg-") {
		t.Fatalf("底图应上传成功并写进模板：%d %s", w.Code, w.Body.String())
	}
	if after := deviceManifest(t, h).Version; after == before {
		t.Fatal("换了底图，设备清单版本应变化")
	}
	if w := do(t, h, adminReq("GET", "/api/v1/admin/backgrounds/"+file, nil), http.StatusOK); w.Body.Len() == 0 {
		t.Fatal("后台应能取回底图原图")
	}
	if w := uploadBackground(t, h, tplID, true, bgPNG(t, 32, 20, 0.5, green, true)); w.Code != http.StatusOK {
		t.Fatalf("对调版底图应上传成功：%d %s", w.Code, w.Body.String())
	}

	gw := do(t, h, adminReq("GET", "/api/v1/admin/templates/"+tplID+"/guide?mirror=1", nil), http.StatusOK)
	guide, err := png.Decode(gw.Body)
	if tpl := state(s).Templates[tplID]; err != nil || guide.Bounds().Dx() != tpl.W || guide.Bounds().Dy() != tpl.H {
		t.Fatalf("参考图应是画布尺寸的 PNG：%v", err)
	}

	// 底图只由底图接口改：编辑模板（含 JSON 编辑）时忽略请求里的底图字段，保留原值
	tpl := state(s).Templates[tplID]
	tpl.BackgroundImage, tpl.Name = "bg-00000000000000ff.png", "改了名"
	do(t, h, adminReq("PUT", "/api/v1/admin/templates/"+tplID, tpl), http.StatusOK)
	if got := state(s).Templates[tplID]; got.BackgroundImage != file || got.Name != "改了名" {
		t.Fatalf("编辑模板应保留底图：%+v", got)
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
	testutil.WaitFor(t, 3*time.Second, "没有模板引用的底图被维护协程回收", func() bool { return len(files()) == 0 })
}
