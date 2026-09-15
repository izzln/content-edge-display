package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Identity 是设备的持久化身份。
type Identity struct {
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

// HardwareInfo 随注册/心跳上报，便于运营方识别机器。
type HardwareInfo struct {
	Hostname string
	HWSerial string
	MAC      string
}

// 默认主机名不能当设备编号（母镜像克隆出的机器主机名相同）。
var defaultHostnames = map[string]bool{
	"orangepione": true, "orangepi": true, "armbian": true, "localhost": true, "debian": true,
}

var hostnameIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,63}$`)

// loadOrCreateIdentity 解析设备身份，优先级：
// 配置文件显式值 > cache_dir/identity.json > 主机名（非默认值）> SoC 序列号/MAC 派生。
// 生成的值持久化到 identity.json，重启/重试不变。
func loadOrCreateIdentity(cfg *Config, hw HardwareInfo) (Identity, error) {
	path := filepath.Join(cfg.CacheDir, "identity.json")
	var id Identity
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &id)
	}
	changed := false

	if cfg.DeviceID != "" {
		id.DeviceID = cfg.DeviceID
	} else if id.DeviceID == "" {
		id.DeviceID = deriveDeviceID(hw)
		changed = true
	}
	if cfg.Secret != "" {
		id.Secret = cfg.Secret
	} else if id.Secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return Identity{}, err
		}
		id.Secret = hex.EncodeToString(b)
		changed = true
	}

	if changed {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			return Identity{}, err
		}
		data, _ := json.MarshalIndent(id, "", "  ")
		if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
			return Identity{}, err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

// deriveDeviceID 从主机名或硬件序列号派生设备编号。
func deriveDeviceID(hw HardwareInfo) string {
	host := strings.ToLower(strings.TrimSpace(hw.Hostname))
	if host != "" && !defaultHostnames[host] && hostnameIDPattern.MatchString(host) {
		return host
	}
	if s := strings.ToLower(hw.HWSerial); len(s) >= 8 {
		return "opi-" + s[len(s)-8:]
	}
	if m := strings.ToLower(strings.ReplaceAll(hw.MAC, ":", "")); len(m) >= 8 {
		return "opi-" + m[len(m)-8:]
	}
	// 完全取不到硬件信息（异常）：随机编号，仍持久化保证稳定。
	b := make([]byte, 4)
	rand.Read(b)
	return "opi-" + hex.EncodeToString(b)
}

// collectHardwareInfo 读取主机名、SoC 序列号（全志 SID → /proc/cpuinfo Serial）与 eth0 MAC。
func collectHardwareInfo() HardwareInfo {
	var hw HardwareInfo
	hw.Hostname, _ = os.Hostname()

	if data, err := os.ReadFile("/sys/bus/nvmem/devices/sunxi-sid0/nvmem"); err == nil && len(data) >= 16 {
		hw.HWSerial = hex.EncodeToString(data[:16])
	} else if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Serial") {
				if i := strings.Index(line, ":"); i >= 0 {
					hw.HWSerial = strings.TrimSpace(line[i+1:])
				}
			}
		}
	}

	for _, ifname := range []string{"eth0", "end0", "wlan0"} {
		if data, err := os.ReadFile("/sys/class/net/" + ifname + "/address"); err == nil {
			hw.MAC = strings.TrimSpace(string(data))
			break
		}
	}
	return hw
}

// localIP 返回到服务器方向的本机出口 IP（不实际发包）。
func localIP(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, port))
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return fmt.Sprint(conn.LocalAddr())
}
