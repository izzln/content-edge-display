package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Config 是设备代理配置（JSON 文件）。
//
// 批量部署时 device_id/secret 留空：编号取自主机名或 SoC 序列号，密钥首启随机生成，
// 两者持久化在 cache_dir/identity.json，并用 enroll_token 向服务端自注册。
type Config struct {
	ServerURL          string   `json:"server_url"`
	DeviceID           string   `json:"device_id,omitempty"`
	Secret             string   `json:"secret,omitempty"`
	EnrollToken        string   `json:"enroll_token,omitempty"`
	CacheDir           string   `json:"cache_dir"`
	InstallDir         string   `json:"install_dir"` // OTA 安装布局根目录；空=禁用 OTA
	PollIntervalS      int      `json:"poll_interval_s"`
	HeartbeatIntervalS int      `json:"heartbeat_interval_s"`
	Player             string   `json:"player"` // "mpv" | "null"
	ImageDurationS     int      `json:"image_duration_s"`
	MpvSocket          string   `json:"mpv_socket"`
	MpvExtraArgs       []string `json:"mpv_extra_args"`
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
	return &cfg, nil
}

func (c *Config) fillDefaults() error {
	if c.ServerURL == "" {
		return errors.New("config: server_url is required")
	}
	if c.EnrollToken == "" && (c.DeviceID == "" || c.Secret == "") {
		return errors.New("config: 需要 enroll_token（自注册）或显式 device_id + secret")
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	if c.CacheDir == "" {
		c.CacheDir = "/var/lib/display-agent"
	}
	if c.PollIntervalS <= 0 {
		c.PollIntervalS = 30
	}
	if c.HeartbeatIntervalS <= 0 {
		c.HeartbeatIntervalS = 60
	}
	if c.Player == "" {
		c.Player = "mpv"
	}
	if c.Player != "mpv" && c.Player != "null" {
		return fmt.Errorf("config: unknown player %q", c.Player)
	}
	if c.ImageDurationS <= 0 {
		c.ImageDurationS = 10
	}
	if c.MpvSocket == "" {
		c.MpvSocket = "/run/display-agent/mpv.sock"
	}
	return nil
}
