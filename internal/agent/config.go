package agent

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Config 是设备代理配置（/etc/display-agent/agent.json，由 install-agent.sh 写入）。
//
// 设备一律凭 enroll_token 自注册：编号取自主机名或 SoC 序列号，密钥首启随机生成，
// 两者持久化在 CacheDir/identity.json。轮询/心跳间隔由服务端规定（见 schedule.go）。
type Config struct {
	ServerURL string `json:"server_url"` // https://<服务器>:9001
	// TLSFingerprint 是服务端证书的公钥指纹（sha256 hex）：设备只认这把公钥，防止局域网里有人冒充服务端。
	TLSFingerprint string `json:"tls_fingerprint"`
	EnrollToken    string `json:"enroll_token"`
	Player         string `json:"player"` // "gst" | "null"
	// DisplayMode 是显示屏输出分辨率（WxH），见 OutputMode；留空用显示屏的首选模式。
	DisplayMode string `json:"display_mode"`

	// CacheDir 放身份、缓存与播放状态：systemd 的 StateDirectory（$STATE_DIRECTORY，即 /var/lib/display-agent）。
	// 不是配置项，测试里直接赋值。
	CacheDir string `json:"-"`
}

var displayModePattern = regexp.MustCompile(`^[1-9][0-9]{2,4}x[1-9][0-9]{2,4}$`)

// LoadConfig 读取配置文件、填充默认值并校验。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, cfg.fillDefaults()
}

func (c *Config) fillDefaults() error {
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	c.TLSFingerprint = strings.ToLower(strings.ReplaceAll(c.TLSFingerprint, ":", ""))
	if c.CacheDir == "" {
		c.CacheDir = cmp.Or(os.Getenv("STATE_DIRECTORY"), "/var/lib/display-agent")
	}
	if c.Player == "" {
		c.Player = "gst"
	}
	switch {
	case !strings.HasPrefix(c.ServerURL, "https://"):
		return errors.New("config: server_url must be https://<server>:9001")
	case len(c.TLSFingerprint) != 64:
		return errors.New("config: tls_fingerprint must be the server certificate fingerprint " +
			"(64 hex characters, shown in the admin UI and filled in by install.sh)")
	case c.EnrollToken == "":
		return errors.New("config: enroll_token is required (must match enroll_token in the server's server.json)")
	case c.Player != "gst" && c.Player != "null":
		return fmt.Errorf("config: unknown player %q", c.Player)
	case c.DisplayMode != "" && !displayModePattern.MatchString(c.DisplayMode):
		return fmt.Errorf("config: display_mode %q is invalid, expected e.g. 1440x900", c.DisplayMode)
	}
	return nil
}
