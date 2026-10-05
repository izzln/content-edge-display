package server

import (
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func pixelNear(img image.Image, x, y int, want color.RGBA) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	d := func(v uint32, w uint8) bool { return int(v>>8)-int(w) < 40 && int(w)-int(v>>8) < 40 }
	return d(r, want.R) && d(g, want.G) && d(b, want.B)
}

// 视频缩略图：没有 ffmpeg 时 404（后台显示 ▶）；有 ffmpeg 时抽一帧，同一个视频只抽一次。
func TestVideoThumbnail(t *testing.T) {
	s, h := newAdminTestServer(t)
	dir := s.deviceMediaDir(testDeviceID)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("fake video"), 0o644)
	thumb := "/api/v1/admin/devices/" + testDeviceID + "/media/clip.mp4/thumb"

	do(t, h, adminReq("GET", thumb, nil), http.StatusNotFound)
	enc := &fakeEncoder{}
	s.setEncoder(enc)
	for range 2 {
		if w := do(t, h, adminReq("GET", thumb, nil), http.StatusOK); w.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatal("视频缩略图应是 JPEG")
		}
	}
	if enc.frames != 1 {
		t.Fatalf("同一个视频只应抽一次帧，实际 %d 次", enc.frames)
	}
}

// 效果预览：带设备时把播放列表里的一项铺进媒体区（默认第一项，可用 item 指定），模板盖在上面。
func TestPreviewShowsContent(t *testing.T) {
	s, h := newAdminTestServer(t)
	s.setEncoder(&fakeEncoder{})
	dir := s.deviceMediaDir(testDeviceID)
	os.MkdirAll(dir, 0o755)
	yellow := image.NewRGBA(image.Rect(0, 0, 160, 100))
	for i := 0; i < len(yellow.Pix); i += 4 {
		copy(yellow.Pix[i:], []byte{0xFF, 0xFF, 0, 0xFF})
	}
	f, _ := os.Create(filepath.Join(dir, "a.png"))
	png.Encode(f, yellow)
	f.Close()
	os.WriteFile(filepath.Join(dir, "b.mp4"), []byte("fake video"), 0o644)
	tplID := state(s).Global.TemplateID // 默认模板：右半边是媒体区

	get := func(q string) image.Image {
		t.Helper()
		w := do(t, h, adminReq("GET", "/api/v1/admin/templates/"+tplID+"/preview"+q, nil), http.StatusOK)
		img, err := png.Decode(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	if img := get("?device=" + testDeviceID); !pixelNear(img, 1080, 450, color.RGBA{0xFF, 0xFF, 0, 0xFF}) {
		t.Fatalf("媒体区应显示第一项（黄色图片）：%v", img.At(1080, 450))
	}
	if img := get("?device=" + testDeviceID + "&item=b.mp4"); !pixelNear(img, 1080, 450, color.RGBA{0, 0xC0, 0, 0xFF}) {
		t.Fatalf("指定视频时媒体区应显示抽出的帧（绿色）：%v", img.At(1080, 450))
	}
	if img := get(""); pixelNear(img, 1080, 450, color.RGBA{0xFF, 0xFF, 0, 0xFF}) {
		t.Fatal("不带设备的预览只是版式，媒体区不应有内容")
	}
}
