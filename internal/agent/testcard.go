package agent

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
)

// 测试卡是临时内容：清单带着到期时间（manifest.Manifest.Expires）。联系得上服务端时，到期后下一次轮询
// 自然拿到正常内容；联系不上（断网、断网重启）时设备要自己到点退回去，不能一直显示测试卡。
// 所以换成测试卡之前，把当时的正常清单另存一份（base.json），测试卡期间它引用的文件也不清理。

func (a *Agent) basePath() string { return filepath.Join(a.cfg.CacheDir, "base.json") }

func readManifest(path string) (*manifest.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// keepBase 在落盘新清单 m 之前调用：m 是测试卡、而当前是正常内容时，把当前清单另存为 base.json；
// 测试卡接着测试卡（延长测试）时保留原来的；m 是正常内容时 base.json 就没用了。
func (a *Agent) keepBase(m *manifest.Manifest) {
	if m.Expires.IsZero() {
		os.Remove(a.basePath())
		return
	}
	cur, err := os.ReadFile(a.currentPath())
	if err != nil {
		return // 开机就是测试卡，之前没有正常内容
	}
	var c manifest.Manifest
	if json.Unmarshal(cur, &c) != nil || !c.Expires.IsZero() {
		return
	}
	if err := fsutil.WriteFile(a.basePath(), cur, 0o644); err != nil {
		log.Printf("agent: %v", err)
	}
}

// testExpired 判断到期时间 until（非零）是否已过；时间以服务端为准（clock.go）。
func (a *Agent) testExpired(until time.Time) bool {
	return !until.IsZero() && !a.clock.Now().Before(until)
}

// backToBase 退回测试卡之前的正常内容：base.json 改回 current.json 并播放。
func (a *Agent) backToBase() error {
	data, err := os.ReadFile(a.basePath())
	if err != nil {
		return err
	}
	b, err := readManifest(a.basePath())
	if err != nil {
		return err
	}
	if err := a.restore(b); err != nil {
		return err
	}
	if err := fsutil.WriteFile(a.currentPath(), data, 0o644); err != nil {
		log.Printf("agent: %v", err)
	}
	os.Remove(a.basePath())
	return nil
}

// expireTest 测试卡到期而服务端还没给新内容（联系不上）时，自己退回正常内容。
func (a *Agent) expireTest() {
	if !a.testExpired(a.testUntil) {
		return
	}
	until := a.testUntil
	if err := a.backToBase(); err != nil {
		a.testUntil = time.Time{} // 没有可退回的内容：继续显示测试卡，不再反复尝试
		log.Printf("agent: test card expired at %s but no earlier content is cached (%v); keeping it until the server is reachable",
			until.In(a.localZone()).Format(time.DateTime), err)
		return
	}
	log.Printf("agent: test card expired at %s, back to the normal content", until.In(a.localZone()).Format(time.DateTime))
}

// testRemaining 返回测试卡还要显示多久；没有测试卡时 ok 为 false。
func (a *Agent) testRemaining() (d time.Duration, ok bool) {
	if a.testUntil.IsZero() {
		return 0, false
	}
	return max(0, a.testUntil.Sub(a.clock.Now())), true
}
