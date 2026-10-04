package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// Config 是服务端配置（server.json）。
// 设备不在这里配置：一律由设备凭 enroll_token 自注册，记录在 data_dir/state.json。
// 图片停留时长也不在这里配置：它是版式的一部分，跟着模板走（管理后台里改）。
type Config struct {
	Listen          string `json:"listen"`           // HTTPS：设备与管理后台，默认 :9001
	BootstrapListen string `json:"bootstrap_listen"` // HTTP：只提供一键装机入口，其余跳转 HTTPS，默认 :9000
	MediaRoot       string `json:"media_root"`       // 各设备的播放内容，默认 data/media
	DataDir         string `json:"data_dir"`         // state.json、cache.json、rendered/、packages/、deps/、incoming/、tls/，默认 data
	FontPath        string `json:"font_path"`        // 模板渲染字体（中文需 CJK 字体）
	AdminToken      string `json:"admin_token"`      // 管理后台口令
	EnrollToken     string `json:"enroll_token"`     // 设备注册口令（一键装机时输入）
	Timezone        string `json:"timezone"`         // 时段计划时区，默认系统时区
	// FFmpegPath 指定 ffmpeg；留空在服务进程的 PATH 里找。不可用时不能上传视频（后台会提示原因）。
	FFmpegPath string `json:"ffmpeg_path"`
	// 设备的轮询与心跳间隔：在这里统一规定，设备从响应头学到后照办（见 manifest.HeaderPollInterval）。
	PollIntervalS      int `json:"poll_interval_s"`      // 默认 10
	HeartbeatIntervalS int `json:"heartbeat_interval_s"` // 默认 60
}

// placeholderToken 是配置样例里的占位口令。仓库是公开的，样例值人人可见，
// 带着它启动等于没有口令，所以直接拒绝启动。
const placeholderToken = "change-me"

// LoadConfig 读取配置文件、填充默认值并校验。
//
// 配置里的相对路径一律相对**配置文件所在目录**解析，而不是进程的工作目录：
// systemd 启动服务时工作目录是 /，按工作目录解析会让 "data" 悄悄落到 /data。
// 这样也支持把 display-server、server.json、data/、fonts/ 放在同一个目录里整体搬走。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Config{MediaRoot: "data/media", DataDir: "data"}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	cfg.MediaRoot, cfg.DataDir, cfg.FontPath = resolve(cfg.MediaRoot), resolve(cfg.DataDir), resolve(cfg.FontPath)
	// ffmpeg_path 只写程序名（如 "ffmpeg"）时按 PATH 查找；带路径的相对写法按配置文件目录解析
	if strings.ContainsRune(cfg.FFmpegPath, os.PathSeparator) {
		cfg.FFmpegPath = resolve(cfg.FFmpegPath)
	}
	return &cfg, cfg.validate()
}

// validate 填充端口与间隔的默认值，并检查口令与取值范围。
func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = ":9001"
	}
	if c.BootstrapListen == "" {
		c.BootstrapListen = ":9000"
	}
	if c.PollIntervalS == 0 {
		c.PollIntervalS = manifest.DefaultPollIntervalS
	}
	if c.HeartbeatIntervalS == 0 {
		c.HeartbeatIntervalS = manifest.DefaultHeartbeatIntervalS
	}
	for name, tok := range map[string]string{"admin_token": c.AdminToken, "enroll_token": c.EnrollToken} {
		switch tok {
		case "":
			return fmt.Errorf("config: %s is empty (generate one with make tokens)", name)
		case placeholderToken:
			return fmt.Errorf("config: %s is still the public placeholder %q from the sample config; generate one with make tokens",
				name, placeholderToken)
		}
	}
	switch {
	case c.MediaRoot == "" || c.DataDir == "":
		return fmt.Errorf("config: media_root and data_dir are required")
	case c.BootstrapListen == c.Listen:
		return fmt.Errorf("config: bootstrap_listen (%s) must differ from listen (HTTPS)", c.Listen)
	case c.PollIntervalS < manifest.MinPollIntervalS || c.PollIntervalS > manifest.MaxPollIntervalS:
		return fmt.Errorf("config: poll_interval_s must be %d-%d seconds", manifest.MinPollIntervalS, manifest.MaxPollIntervalS)
	case c.HeartbeatIntervalS < manifest.MinHeartbeatIntervalS || c.HeartbeatIntervalS > manifest.MaxHeartbeatIntervalS:
		return fmt.Errorf("config: heartbeat_interval_s must be %d-%d seconds", manifest.MinHeartbeatIntervalS, manifest.MaxHeartbeatIntervalS)
	}
	return nil
}
