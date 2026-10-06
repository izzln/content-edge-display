package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/player"
)

// 测试卡到期后设备自己退回正常内容，不依赖联系得上服务端：运行中断网、断网重启两种情况；
// 测试卡期间正常内容的文件不被清掉；测试卡接着测试卡（延长）时退回的仍是最初的正常内容。
func TestTestCardExpiresOffline(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	os.WriteFile(filepath.Join(e.devDir, "a.jpg"), []byte("normal"), 0o644)
	poll := func() {
		t.Helper()
		if err := e.a.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	normal := func(p *player.Null) bool { return strings.HasSuffix(nowPlaying(p), "_a.jpg") }
	testCard := func(p *player.Null) bool { return strings.Contains(filepath.Base(nowPlaying(p)), "test_") }
	startTest := func(s int) {
		t.Helper()
		if w := e.admin(t, "POST", "/api/v1/admin/devices/"+e.a.identity.DeviceID+"/test", `{"duration_s":`+strconv.Itoa(s)+`}`); w.Code != 204 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	later := func(a *Agent, d time.Duration) { a.clock.local = func() time.Time { return time.Now().Add(d) } }

	poll()
	if !normal(e.p) {
		t.Fatalf("应先播正常内容：%q", nowPlaying(e.p))
	}
	startTest(60)
	poll()
	if !testCard(e.p) || e.a.testUntil.IsZero() {
		t.Fatalf("应显示测试卡并记下到期时间：%q %v", nowPlaying(e.p), e.a.testUntil)
	}
	if d, ok := e.a.testRemaining(); !ok || d <= 0 || d > time.Minute+time.Second { // 服务端时间按 Date 头学到，精确到秒
		t.Fatalf("剩余时间不对：%s %v", d, ok)
	}
	// 延长测试：退回的仍应是最初的正常内容，正常内容的文件也还在
	startTest(120)
	poll()
	if !testCard(e.p) {
		t.Fatal("延长后仍是测试卡")
	}

	// 1) 运行中断网：没到期不动，到期自己退回
	e.a.expireTest()
	if !testCard(e.p) {
		t.Fatal("没到期不应退回")
	}
	later(e.a, 3*time.Minute)
	e.a.expireTest()
	if !normal(e.p) || !e.a.testUntil.IsZero() {
		t.Fatalf("到期后应自己退回正常内容：%q", nowPlaying(e.p))
	}
	if _, err := os.Stat(e.a.basePath()); !os.IsNotExist(err) {
		t.Fatal("退回后 base.json 应删掉")
	}
	if m, _ := readManifest(e.a.currentPath()); m == nil || !m.Expires.IsZero() {
		t.Fatal("退回后 current.json 应是正常内容（重启也不再回到测试卡）")
	}

	// 2) 断网重启：记着的测试卡已过期 → 开机直接放正常内容；没过期 → 继续显示测试卡
	later(e.a, 0)
	e.a.manifestVer = ""
	startTest(60)
	poll()
	if !testCard(e.p) {
		t.Fatal("应再次显示测试卡")
	}
	p2 := player.NewNull()
	a2 := New(e.a.cfg, p2)
	if err := a2.loadCurrent(); err != nil || !testCard(p2) {
		t.Fatalf("没到期时重启应继续显示测试卡：%v %q", err, nowPlaying(p2))
	}
	p3 := player.NewNull()
	a3 := New(e.a.cfg, p3)
	later(a3, 3*time.Minute)
	if err := a3.loadCurrent(); err != nil || !normal(p3) {
		t.Fatalf("测试卡已过期，重启后应放正常内容：%v %q", err, nowPlaying(p3))
	}

	// 3) 联系得上服务端时照旧由服务端结束测试：到期后轮询拿到正常内容，base.json 删掉
	e.admin(t, "POST", "/api/v1/admin/devices/"+e.a.identity.DeviceID+"/test", `{"duration_s":60}`)
	poll()
	e.admin(t, "POST", "/api/v1/admin/devices/"+e.a.identity.DeviceID+"/test", `{"duration_s":0}`)
	poll()
	if !normal(e.p) || !e.a.testUntil.IsZero() {
		t.Fatalf("取消测试后应回到正常内容：%q", nowPlaying(e.p))
	}
	if _, err := os.Stat(e.a.basePath()); !os.IsNotExist(err) {
		t.Fatal("回到正常内容后 base.json 应删掉")
	}
}
