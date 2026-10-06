package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/server"
)

// 分时段亮度：后台设好 → 设备按服务端时区判断此刻所在时段、设播放器亮度；计划存盘，断网重启后照旧；
// 清空后恢复 100%。
func TestBrightnessFollowsServer(t *testing.T) {
	e := newEnv(t, true, func(c *server.Config) { c.Timezone = "Asia/Shanghai" })
	ctx := context.Background()
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	// 设备时间以服务端为准（clock.go），时段按此刻的东八区时间前后各 2 小时取，避开分钟边界
	now := time.Now().In(shanghai)
	hm := func(t time.Time) string { return t.Format("15:04") }
	set := func(periods []map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"periods": periods})
		if w := e.admin(t, "PUT", "/api/v1/admin/brightness", string(b)); w.Code != 204 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	step := func() {
		t.Helper()
		if err := e.a.step(ctx); err != nil {
			t.Fatal(err)
		}
	}

	step()
	if e.p.Brightness() != 100 {
		t.Fatalf("没设置时应是 100%%：%d", e.p.Brightness())
	}
	set([]map[string]any{
		{"start": hm(now.Add(-2 * time.Hour)), "end": hm(now.Add(2 * time.Hour)), "percent": 30, "hide_media": true},
		{"start": hm(now.Add(3 * time.Hour)), "end": hm(now.Add(4 * time.Hour)), "percent": 70},
	})
	step()
	if e.p.Brightness() != 30 || e.p.Media() {
		t.Fatalf("此刻在 30%%、停播媒体区的时段里：%d %v", e.p.Brightness(), e.p.Media())
	}
	if _, err := os.Stat(e.a.brightnessPath()); err != nil {
		t.Fatal("亮度计划应存盘")
	}

	// 断网重启：新代理还没联系服务端，凭存盘的计划照样调暗
	p2 := player.NewNull()
	a2 := New(e.a.cfg, p2)
	a2.applyBrightness()
	if p2.Brightness() != 30 || p2.Media() {
		t.Fatalf("重启后凭存下的计划应照样调暗、停播媒体区：%d %v", p2.Brightness(), p2.Media())
	}

	// 时段不含此刻 → 100%；清空 → 100%
	set([]map[string]any{{"start": hm(now.Add(3 * time.Hour)), "end": hm(now.Add(4 * time.Hour)), "percent": 70}})
	step()
	if e.p.Brightness() != 100 || !e.p.Media() {
		t.Fatalf("此刻不在任何时段里应是 100%%、正常播放：%d %v", e.p.Brightness(), e.p.Media())
	}
	set([]map[string]any{{"start": hm(now.Add(-2 * time.Hour)), "end": hm(now.Add(2 * time.Hour)), "percent": 50}})
	step()
	set(nil)
	step()
	if e.p.Brightness() != 100 {
		t.Fatalf("清空后应恢复 100%%：%d", e.p.Brightness())
	}
}
