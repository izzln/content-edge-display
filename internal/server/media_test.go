package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
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
)

// ---- 测试素材 ----

// box 拼一个 ISO-BMFF 盒子。
func box(typ string, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	return append(append(out, typ...), body...)
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// mp4Fixture 造一个只有盒子骨架的 MP4：编码 fourcc 是我们唯一关心的信息。
// 解析逻辑另外在真实的 ffmpeg 产物上验证过（libx264→avc1、libx265→hev1、.mov→avc1）。
func mp4Fixture(fourcc string) []byte {
	sampleEntry := box(fourcc, make([]byte, 70)) // 其余字段与本测试无关
	stsd := box("stsd", u32(0), u32(1), sampleEntry)
	moov := box("moov", box("trak", box("mdia", box("minf", box("stbl", stsd)))))
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso2avc1mp41"))
	mdat := box("mdat", bytes.Repeat([]byte{0}, 64))
	return append(append(ftyp, moov...), mdat...)
}

// pngHeaderFixture 造一个只有 IHDR 的 PNG：image.DecodeConfig 只读文件头，
// 用它声明超大尺寸，不必真的生成一张几千万像素的图。
func pngHeaderFixture(w, h int) []byte {
	ihdr := append(u32(uint32(w)), u32(uint32(h))...)
	ihdr = append(ihdr, 8, 0, 0, 0, 0) // 8bit 灰度
	chunk := append(u32(uint32(len(ihdr))), []byte("IHDR")...)
	chunk = append(chunk, ihdr...)
	chunk = append(chunk, u32(crc32.ChecksumIEEE(chunk[4:]))...)
	return append([]byte("\x89PNG\r\n\x1a\n"), chunk...)
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.SetRGBA(0, 0, color.RGBA{0xFF, 0, 0, 0xFF})
	return encodePNG(t, img)
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type upload struct {
	name string
	data []byte
}

// uploadMedia 往设备的播放列表里上传若干文件（一次 multipart 请求，多个文件字段）。
func uploadMedia(t *testing.T, h http.Handler, deviceID string, files ...upload) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		fw, err := mw.CreateFormFile("files", f.name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(f.data)
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/api/v1/admin/devices/"+deviceID+"/media", &buf)
	r.Header.Set("X-Admin-Token", adminToken)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type uploadResult struct {
	Accepted    []string `json:"accepted"`
	Transcoding []string `json:"transcoding"`
	Rejected    []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"rejected"`
	Playlist []MediaFile `json:"playlist"`
}

func parseUpload(t *testing.T, w *httptest.ResponseRecorder) uploadResult {
	t.Helper()
	var res uploadResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析上传响应失败：%v: %s", err, w.Body.String())
	}
	return res
}

// ---- 编码识别 ----

func TestVideoCodecGate(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		wantErr string // 空表示应当放行
	}{
		{"h264-avc1", mp4Fixture("avc1"), ""},
		{"h264-avc3", mp4Fixture("avc3"), ""},
		{"h265-hvc1", mp4Fixture("hvc1"), "H.265"},
		{"h265-hev1", mp4Fixture("hev1"), "H.265"},
		{"av1", mp4Fixture("av01"), "AV1"},
		{"no-video-track", mp4Fixture("mp4a"), "没有找到视频轨道"},
		{"not-a-mp4", []byte("这不是 MP4 文件，只是一段文字而已"), "不是有效的 MP4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkVideoPlayable(bytes.NewReader(c.data), int64(len(c.data)))
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("应当放行，却被拒：%v", err)
			case c.wantErr != "" && err == nil:
				t.Fatal("应当拒收，却放行了")
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("拒收原因应包含 %q，得到：%v", c.wantErr, err)
			}
		})
	}
	// H.265 的提示必须可操作：告诉运营方怎么办，而不只是说不行
	err := checkVideoPlayable(bytes.NewReader(mp4Fixture("hvc1")), int64(len(mp4Fixture("hvc1"))))
	if !strings.Contains(err.Error(), "H.264") || !strings.Contains(err.Error(), "ffmpeg") {
		t.Fatalf("拒收提示应告诉运营方怎么重新编码，得到：%v", err)
	}
}

// ---- 上传校验 ----

func TestDeviceMediaUploadValidation(t *testing.T) {
	_, h := newAdminTestServer(t)
	dev := "/api/v1/admin/devices/" + testDeviceID + "/media"

	res := parseUpload(t, uploadMedia(t, h, testDeviceID,
		upload{"promo.jpg", tinyPNG(t)}, // 后缀是 jpg 但内容是 PNG：解码器认内容，应当通过
		upload{"clip.mp4", mp4Fixture("avc1")},
		upload{"hevc.mp4", mp4Fixture("hvc1")},
		upload{"huge.png", pngHeaderFixture(8000, 7000)},    // 5600 万像素，超过服务端上限
		upload{"truncated.png", pngHeaderFixture(800, 600)}, // 只有文件头，内容被截断
		upload{"notes.txt", []byte("hello")},
		upload{"broken.png", []byte("not a png at all")},
	))
	if len(res.Accepted) != 2 {
		t.Fatalf("应当只收下 2 个文件，得到 %v（拒收 %+v）", res.Accepted, res.Rejected)
	}
	reasons := map[string]string{}
	for _, r := range res.Rejected {
		reasons[r.Name] = r.Reason
	}
	for name, want := range map[string]string{
		"hevc.mp4":      "H.264",
		"huge.png":      "像素过大",
		"notes.txt":     "不支持的文件类型",
		"broken.png":    "无法解码",
		"truncated.png": "无法解码",
	} {
		if !strings.Contains(reasons[name], want) {
			t.Errorf("%s 的拒收原因应包含 %q，得到 %q", name, want, reasons[name])
		}
	}
	// 被拒的文件不能留在目录里
	w := do(t, h, adminReq("GET", dev, nil), http.StatusOK)
	for _, bad := range []string{"hevc.mp4", "huge.png", "notes.txt", "broken.png", "truncated.png"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Errorf("被拒的 %s 不该出现在播放列表里：%s", bad, w.Body.String())
		}
	}

	// 同名再传一次 → 自动改名，不覆盖原文件
	res = parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"promo.jpg", tinyPNG(t)}))
	if len(res.Accepted) != 1 || res.Accepted[0] != "promo-2.jpg" {
		t.Fatalf("同名文件应自动改名，得到 %v", res.Accepted)
	}
	if len(res.Playlist) != 3 {
		t.Fatalf("播放列表应有 3 个文件，得到 %+v", res.Playlist)
	}

	// 未知设备
	if w := uploadMedia(t, h, "nope", upload{"a.jpg", tinyPNG(t)}); w.Code != http.StatusNotFound {
		t.Fatalf("未知设备应 404，得到 %d", w.Code)
	}
}

func TestDeviceMediaOversizeRejected(t *testing.T) {
	_, h := newAdminTestServer(t)
	// 超过 20MB 的“图片”：超限判断在解码之前，所以内容无需是真图
	res := parseUpload(t, uploadMedia(t, h, testDeviceID,
		upload{"big.jpg", bytes.Repeat([]byte{0x41}, maxImageUploadBytes+1)}))
	if len(res.Accepted) != 0 || len(res.Rejected) != 1 ||
		!strings.Contains(res.Rejected[0].Reason, fmt.Sprintf("%dMB", maxImageUploadBytes>>20)) {
		t.Fatalf("超限图片应被拒并说明上限：%+v", res)
	}
}

// ---- 播放列表顺序与增删 ----

func TestDeviceMediaReorderAndDelete(t *testing.T) {
	_, h := newAdminTestServer(t)
	base := "/api/v1/admin/devices/" + testDeviceID + "/media"

	parseUpload(t, uploadMedia(t, h, testDeviceID,
		upload{"a.jpg", tinyPNG(t)}, upload{"b.jpg", tinyPNG(t)}, upload{"c.mp4", mp4Fixture("avc1")}))

	names := func(w *httptest.ResponseRecorder) []string {
		var list []MediaFile
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(list))
		for _, f := range list {
			out = append(out, f.Name)
		}
		return out
	}

	// 上传顺序即初始播放顺序
	got := names(do(t, h, adminReq("GET", base, nil), http.StatusOK))
	if strings.Join(got, ",") != "a.jpg,b.jpg,c.mp4" {
		t.Fatalf("初始顺序应为上传顺序，得到 %v", got)
	}

	// 拖拽排序
	got = names(do(t, h, adminReq("PUT", base, []string{"c.mp4", "a.jpg", "b.jpg"}), http.StatusOK))
	if strings.Join(got, ",") != "c.mp4,a.jpg,b.jpg" {
		t.Fatalf("排序未生效，得到 %v", got)
	}
	// 顺序在设备清单里也要体现
	m := deviceManifest(t, h)
	if len(m.Items) != 3 || m.Items[0].Name != "c.mp4" {
		t.Fatalf("清单顺序应跟随播放列表：%+v", m.Items)
	}
	if m.Items[0].Duration != 0 {
		t.Fatalf("视频不该有停留时长（播完即切），得到 %d", m.Items[0].Duration)
	}
	if m.Items[1].Duration <= 0 {
		t.Fatalf("图片应带上模板里的停留时长，得到 %d", m.Items[1].Duration)
	}

	// 非法排序：不存在的文件 / 重复项
	do(t, h, adminReq("PUT", base, []string{"nope.jpg"}), http.StatusBadRequest)
	do(t, h, adminReq("PUT", base, []string{"a.jpg", "a.jpg"}), http.StatusBadRequest)

	// 删除
	got = names(do(t, h, adminReq("DELETE", base+"/a.jpg", nil), http.StatusOK))
	if strings.Join(got, ",") != "c.mp4,b.jpg" {
		t.Fatalf("删除后列表错误，得到 %v", got)
	}
	// 设备不再拿到被删的文件
	if m := deviceManifest(t, h); len(m.Items) != 2 {
		t.Fatalf("删除后清单应只剩 2 项：%+v", m.Items)
	}
}

// ---- 清单 layout ----

// 模板有媒体区且媒体区有内容时：清单条目是媒体文件本身（视频不转码），
// 模板的静态部分作为“媒体区挖空”的叠加图随 layout 下发。
func TestManifestLayoutAndMirror(t *testing.T) {
	s, h := newAdminTestServer(t)
	tplID := globalTemplateID(t, s)

	// 媒体区还没内容 → 整屏图，没有 layout（不会黑屏）
	if m := deviceManifest(t, h); m.Layout != nil || !strings.HasPrefix(m.Items[0].Name, "tpl_") {
		t.Fatalf("媒体区空时应回落整屏图：%+v", m)
	}

	parseUpload(t, uploadMedia(t, h, testDeviceID, upload{"clip.mp4", mp4Fixture("avc1")}))
	m := deviceManifest(t, h)
	if m.Layout == nil {
		t.Fatal("媒体区有内容时必须下发 layout，否则视频会铺满整屏、盖掉属性")
	}
	if len(m.Items) != 1 || m.Items[0].Name != "clip.mp4" || m.Items[0].Type != "video" {
		t.Fatalf("清单条目应是媒体文件本身：%+v", m.Items)
	}
	if got, want := m.Layout.Media, (struct{ X, Y, W, H int }{720, 0, 720, 900}); got.X != want.X ||
		got.Y != want.Y || got.W != want.W || got.H != want.H {
		t.Fatalf("媒体区矩形 = %+v，期望右半屏 %+v", got, want)
	}
	if m.Layout.CanvasW != canvasW || m.Layout.CanvasH != canvasH {
		t.Fatalf("画布尺寸错误：%dx%d", m.Layout.CanvasW, m.Layout.CanvasH)
	}
	if !strings.HasPrefix(m.Layout.Overlay.Name, "ovl_") || m.Layout.Overlay.Size == 0 {
		t.Fatalf("叠加图条目不完整：%+v", m.Layout.Overlay)
	}
	// 设备能下载叠加图
	w := do(t, h, signedRequest("GET", m.Layout.Overlay.URL, nil), http.StatusOK)
	if int64(w.Body.Len()) != m.Layout.Overlay.Size || !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("叠加图下载异常：%d 字节", w.Body.Len())
	}

	// 左右对调：一个模板覆盖两种设备
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/display",
		map[string]any{"mode": "global", "mirror": true}), http.StatusOK)
	flipped := deviceManifest(t, h)
	if flipped.Layout == nil || flipped.Layout.Media.X != 0 || flipped.Layout.Media.W != 720 {
		t.Fatalf("对调后媒体区应在左半屏：%+v", flipped.Layout)
	}
	if flipped.Version == m.Version {
		t.Fatal("对调必须改变清单版本号，否则设备收到 304 不会应用")
	}
	if flipped.Layout.Overlay.SHA256 == m.Layout.Overlay.SHA256 {
		t.Fatal("对调后叠加图内容应当不同")
	}

	// 属性变化只换叠加图，媒体文件不变（视频不用重新下载，更不用重新编码）
	do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes",
		map[string]string{"room": "302"}), http.StatusOK)
	withAttr := deviceManifest(t, h)
	if withAttr.Layout.Overlay.SHA256 == flipped.Layout.Overlay.SHA256 {
		t.Fatal("属性变化应当改变叠加图")
	}
	if withAttr.Items[0].SHA256 != flipped.Items[0].SHA256 {
		t.Fatal("属性变化不该影响媒体文件")
	}

	// 模板没有媒体区时回到整屏图
	noMedia := splitTemplate()
	do(t, h, adminReq("POST", "/api/v1/admin/templates", noMedia), http.StatusOK)
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]string{"template_id": "split"}), http.StatusOK)
	if m := deviceManifest(t, h); m.Layout != nil || len(m.Items) != 1 ||
		!strings.HasPrefix(m.Items[0].Name, "tpl_") {
		t.Fatalf("无媒体区的模板应渲染成整屏图：%+v", m)
	}
	_ = tplID
}

// 渲染目录不能无限堆积：切换模板/属性后旧的渲染文件要清掉。
func TestRenderedFilesPruned(t *testing.T) {
	s, h := newAdminTestServer(t)
	for _, room := range []string{"301", "302", "303"} {
		do(t, h, adminReq("PUT", "/api/v1/admin/devices/"+testDeviceID+"/attributes",
			map[string]string{"room": room}), http.StatusOK)
		deviceManifest(t, h)
	}
	m := deviceManifest(t, h)
	entries, err := os.ReadDir(filepath.Join(s.renderedDir(), testDeviceID))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != m.Items[0].Name {
		t.Fatalf("渲染目录应只留当前清单用到的文件，得到 %v", entries)
	}
}

// ---- 模板保护 ----

func TestLastTemplateProtectedAndGlobalRepoints(t *testing.T) {
	s, h := newAdminTestServer(t)
	seeded := globalTemplateID(t, s)

	// 只剩一个模板时不能删
	w := do(t, h, adminReq("DELETE", "/api/v1/admin/templates/"+seeded, nil), http.StatusConflict)
	if !strings.Contains(w.Body.String(), "最后一个模板") {
		t.Fatalf("拒绝原因应说明是最后一个模板：%s", w.Body.String())
	}

	// 再建一个，然后删掉当前的全局模板 → 全局自动指向剩下的那个
	do(t, h, adminReq("POST", "/api/v1/admin/templates", mediaTemplate("second")), http.StatusOK)
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/"+seeded, nil), http.StatusOK)
	if got := s.store.Global().TemplateID; got != "second" {
		t.Fatalf("删掉全局模板后应自动改指向剩下的模板，得到 %q", got)
	}
	// 设备照样有内容可显示
	if m := deviceManifest(t, h); len(m.Items) == 0 {
		t.Fatal("删除模板后设备不应没有内容")
	}

	// 现在 second 是唯一的模板，同样不能删
	do(t, h, adminReq("DELETE", "/api/v1/admin/templates/second", nil), http.StatusConflict)
}

// 新建模板不必自带 ID：ID 由服务端生成，后台不暴露给使用者。
func TestTemplateIDGeneratedByServer(t *testing.T) {
	_, h := newAdminTestServer(t)
	body := mediaTemplate("")
	delete(body, "id")
	w := do(t, h, adminReq("POST", "/api/v1/admin/templates", body), http.StatusOK)
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if !strings.HasPrefix(created.ID, "tpl-") {
		t.Fatalf("服务端应自动生成模板 ID，得到 %q", created.ID)
	}
}
