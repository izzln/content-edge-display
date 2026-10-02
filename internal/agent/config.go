package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是设备代理配置（JSON 文件）。
//
// 设备一律凭 enroll_token 自注册：编号取自主机名或 SoC 序列号（device_id 可显式覆盖），
// 密钥首启随机生成，两者持久化在 cache_dir/identity.json。
// 轮询/心跳间隔不在这里配置：由服务端规定，设备照办（见 schedule.go）。旧配置里的
// poll_interval_s / heartbeat_interval_s 会被忽略。
type Config struct {
	ServerURL   string `json:"server_url"`
	DeviceID    string `json:"device_id,omitempty"`
	EnrollToken string `json:"enroll_token"`
	CacheDir    string `json:"cache_dir"`
	InstallDir  string `json:"install_dir"` // OTA 安装布局根目录；空=禁用 OTA
	Player      string `json:"player"`      // "mpv" | "null"
	// DisplayMode 是显示屏输出模式（WxH 或 WxH@刷新率），传给 mpv 的 --drm-mode。
	// mpv 默认用 EDID 首选模式、不理会内核的 video= 参数，所以要单独指定；留空则不强制。
	DisplayMode  string   `json:"display_mode"`
	MpvSocket    string   `json:"mpv_socket"`
	MpvExtraArgs []string `json:"mpv_extra_args"`
}

// LoadConfig 读取配置文件并填充默认值。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.fillDefaults(); err != nil {
		return nil, err
	}
	// 相对路径按配置文件所在目录解析，而不是进程工作目录：systemd 下工作目录是 /，
	// 手工在别的目录试跑时又是另一个——cache_dir 一变，identity.json 就是另一份，
	// 设备会拿新密钥去注册而被服务端拒绝。
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for _, p := range []*string{&cfg.CacheDir, &cfg.InstallDir, &cfg.MpvSocket} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return &cfg, nil
}

func (c *Config) fillDefaults() error {
	if c.ServerURL == "" {
		return errors.New("config: server_url is required")
	}
	if c.EnrollToken == "" {
		return errors.New("config: enroll_token is required（与服务端 server.json 的 enroll_token 一致）")
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	if c.CacheDir == "" {
		c.CacheDir = "/var/lib/display-agent"
	}
	if c.Player == "" {
		c.Player = "mpv"
	}
	if c.Player != "mpv" && c.Player != "null" {
		return fmt.Errorf("config: unknown player %q", c.Player)
	}
	if c.DisplayMode != "" && !displayModePattern.MatchString(c.DisplayMode) {
		// 格式不对 mpv 会拒绝启动，设备就黑屏了——在这里报错，装机时就能发现
		return fmt.Errorf("config: display_mode %q 格式不对，应为 1440x900 或 1440x900@60", c.DisplayMode)
	}
	if c.MpvSocket == "" {
		c.MpvSocket = "/run/display-agent/mpv.sock"
	}
	return nil
}
