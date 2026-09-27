package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
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

// 图片停留时长应能在管理后台改并立即对设备生效——
// 包括让清单版本号变化，否则设备一直 304，新设置到不了现场。
func TestGlobalImageDurationReachesDevices(t *testing.T) {
	s, mediaRoot := newTestServer(t)
	s.cfg.AdminToken = adminToken
	h := s.Handler()
	devDir := filepath.Join(mediaRoot, testDeviceID)
	os.MkdirAll(devDir, 0o755)
	os.WriteFile(filepath.Join(devDir, "a.jpg"), []byte("img"), 0o644)

	before := deviceManifest(t, h)
	if before.Items[0].Duration != s.cfg.ImageDurationS {
		t.Fatalf("默认应取 server.json 的值 %d，得到 %d", s.cfg.ImageDurationS, before.Items[0].Duration)
	}

	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]any{"image_duration_s": 25}), http.StatusOK)

	after := deviceManifest(t, h)
	if after.Items[0].Duration != 25 {
		t.Fatalf("后台设置未生效：duration = %d", after.Items[0].Duration)
	}
	if after.Version == before.Version {
		t.Fatal("只改时长也必须改变清单版本号，否则设备收到 304 永远不会更新")
	}
	if after.Items[0].SHA256 != before.Items[0].SHA256 {
		t.Fatal("文件没变，校验和不该变（设备不应因此重新下载）")
	}

	// 非法值被拒绝
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]any{"image_duration_s": 99999}), http.StatusBadRequest)

	// 归零 → 回落到 server.json 的值
	do(t, h, adminReq("PUT", "/api/v1/admin/global", map[string]any{"image_duration_s": 0}), http.StatusOK)
	if back := deviceManifest(t, h); back.Items[0].Duration != s.cfg.ImageDurationS {
		t.Fatalf("清零后应回落到配置值，得到 %d", back.Items[0].Duration)
	}
}
