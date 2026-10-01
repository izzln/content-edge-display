package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izzln/content-edge-display/internal/store"
)

// 相对路径必须按配置文件所在目录解析，而不是进程工作目录：
// systemd 启动时工作目录是 /，按 CWD 解析会让 "data" 落到 /data。
func TestLoadConfigResolvesRelativePathsAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.json")
	body := `{"media_root":"media","data_dir":"data","font_path":"fonts/cjk.ttc","enroll_token":"tok"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// 故意在另一个目录下加载，证明结果与工作目录无关
	t.Chdir(t.TempDir())

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"media_root", cfg.MediaRoot, filepath.Join(dir, "media")},
		{"data_dir", cfg.DataDir, filepath.Join(dir, "data")},
		{"font_path", cfg.FontPath, filepath.Join(dir, "fonts/cjk.ttc")},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q，期望 %q", c.name, c.got, c.want)
		}
	}
}

func TestLoadConfigKeepsAbsolutePathsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.json")
	// media_root 绝对路径；data_dir/font_path 留空
	body := `{"media_root":"/srv/media","enroll_token":"tok"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MediaRoot != "/srv/media" {
		t.Errorf("绝对路径应原样保留，得到 %q", cfg.MediaRoot)
	}
	if want := filepath.Join(dir, "data"); cfg.DataDir != want {
		t.Errorf("默认 data_dir = %q，期望 %q", cfg.DataDir, want)
	}
	if cfg.FontPath != "" {
		t.Errorf("未配置字体时应保持空，得到 %q", cfg.FontPath)
	}
}

// 图片停留时长是版式的一部分，跟着模板走：在后台改模板就要立即对设备生效——
// 包括让清单版本号变化，否则设备一直 304，新设置到不了现场。
func TestTemplateImageDurationReachesDevices(t *testing.T) {
	s, mediaRoot := newTestServer(t)
	s.cfg.AdminToken = adminToken
	h := s.Handler()
	devDir := filepath.Join(mediaRoot, testDeviceID)
	os.MkdirAll(devDir, 0o755)
	os.WriteFile(filepath.Join(devDir, "a.jpg"), []byte("img"), 0o644)

	tplID := globalTemplateID(t, s)
	before := deviceManifest(t, h)
	if len(before.Items) != 1 || before.Items[0].Name != "a.jpg" {
		t.Fatalf("媒体区有内容时清单应是媒体文件本身：%+v", before.Items)
	}
	if before.Items[0].Duration != store.DefaultImageDurationS {
		t.Fatalf("默认停留时长应为 %d，得到 %d", store.DefaultImageDurationS, before.Items[0].Duration)
	}

	tpl, _ := s.store.Template(tplID)
	tpl.ImageDurationS = 25
	do(t, h, adminReq("PUT", "/api/v1/admin/templates/"+tplID, tpl), http.StatusOK)

	after := deviceManifest(t, h)
	if after.Items[0].Duration != 25 {
		t.Fatalf("模板里的设置未生效：duration = %d", after.Items[0].Duration)
	}
	if after.Version == before.Version {
		t.Fatal("只改时长也必须改变清单版本号，否则设备收到 304 永远不会更新")
	}
	if after.Items[0].SHA256 != before.Items[0].SHA256 {
		t.Fatal("文件没变，校验和不该变（设备不应因此重新下载）")
	}

	// 非法值被拒绝
	tpl.ImageDurationS = 99999
	do(t, h, adminReq("PUT", "/api/v1/admin/templates/"+tplID, tpl), http.StatusBadRequest)

	// 归零 → 回落到默认值
	tpl.ImageDurationS = 0
	do(t, h, adminReq("PUT", "/api/v1/admin/templates/"+tplID, tpl), http.StatusOK)
	if back := deviceManifest(t, h); back.Items[0].Duration != store.DefaultImageDurationS {
		t.Fatalf("清零后应回落到默认值，得到 %d", back.Items[0].Duration)
	}
}

// 仓库是公开的，配置样例里的占位口令人人可见，带着它启动等于没有口令。
func TestLoadConfigRejectsPlaceholderAndMissingTokens(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "server.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, body, wantIn string
	}{
		{"占位 admin_token", `{"admin_token":"change-me","enroll_token":"G2o4MrHY"}`, "admin_token"},
		{"占位 enroll_token", `{"admin_token":"6DOTtuXB","enroll_token":"change-me"}`, "enroll_token"},
		{"缺少 enroll_token", `{"admin_token":"6DOTtuXB"}`, "enroll_token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, c.body))
			if err == nil {
				t.Fatal("应当拒绝启动")
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("错误信息应点明是哪个字段，得到：%v", err)
			}
		})
	}

	// 正常口令可以加载
	if _, err := LoadConfig(write(t, `{"admin_token":"6DOTtuXB","enroll_token":"G2o4MrHY"}`)); err != nil {
		t.Fatalf("正常配置不应报错：%v", err)
	}
}

func TestLoadConfigDefaultListenPort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(p, []byte(`{"admin_token":"6DOTtuXB","enroll_token":"G2o4MrHY"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9000" {
		t.Fatalf("默认监听端口应为 :9000，得到 %q", cfg.Listen)
	}
}
