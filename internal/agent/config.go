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
// 轮询/心跳间隔不在这里配置：由服务端规定，设备照办（见 schedule.go）。
type Config struct {
	ServerURL string `json:"server_url"`
	// TLSFingerprint 是服务端证书的公钥指纹（sha256 hex，后台与 install.sh 里都有）：
	// server_url 为 https 时必填，设备只认这把公钥，防止局域网里有人冒充服务端。
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	EnrollToken    string `json:"enroll_token"`
	CacheDir       string `json:"cache_dir"`
	InstallDir     string `json:"install_dir"` // OTA 安装布局根目录；空=禁用 OTA
	Player         string `json:"player"`      // "gst" | "null"
	// DisplayMode 是显示屏输出模式（WxH，可带 @刷新率），见 OutputMode；留空用显示屏的首选模式。
	DisplayMode string `json:"display_mode"`
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
	for _, p := range []*string{&cfg.CacheDir, &cfg.InstallDir} {
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
		return errors.New("config: enroll_token is required (must match enroll_token in the server's server.json)")
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	c.TLSFingerprint = strings.ToLower(strings.ReplaceAll(c.TLSFingerprint, ":", ""))
	if strings.HasPrefix(c.ServerURL, "https://") && len(c.TLSFingerprint) != 64 {
		return errors.New("config: tls_fingerprint is required for an https server_url " +
			"(64 hex characters, shown in the admin UI and filled in by install.sh)")
	}
	if c.CacheDir == "" {
		c.CacheDir = "/var/lib/display-agent"
	}
	if c.Player == "" {
		c.Player = "gst"
	}
	if c.Player != "gst" && c.Player != "null" {
		return fmt.Errorf("config: unknown player %q", c.Player)
	}
	if c.DisplayMode != "" && !displayModePattern.MatchString(c.DisplayMode) {
		// 在这里报错，装机时就能发现
		return fmt.Errorf("config: display_mode %q is invalid, expected e.g. 1440x900 or 1440x900@60", c.DisplayMode)
	}
	return nil
}
