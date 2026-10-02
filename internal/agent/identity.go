package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
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

// loadOrCreateIdentity 解析设备身份。编号优先级：
// 配置文件显式 device_id > cache_dir/identity.json > 主机名（非默认值）> SoC 序列号/MAC 派生；
// 密钥首次随机生成。两者持久化到 identity.json，重启/重试不变。
//
// 这个文件丢了或读不出来，设备就会用**同一个编号、新的密钥**去注册，服务端会当成冒名顶替
// 拒绝（409）。所以：写入要落盘（fsync）；读不出来时不悄悄重建，而是把坏文件留作现场证据、
// 大声报出来——新密钥要运营方在后台确认后才生效。
func loadOrCreateIdentity(cfg *Config, hw HardwareInfo) (Identity, error) {
	path := identityPath(cfg)
	var id Identity
	switch data, err := os.ReadFile(path); {
	case err == nil:
		if err := json.Unmarshal(data, &id); err != nil || id.Secret == "" {
			bad := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
			os.Rename(path, bad)
			log.Printf("agent: WARNING %s is invalid (%v); moved to %s and generating a new identity. "+
				"The server will reject the new key until it is accepted in the admin UI", path, err, bad)
			id = Identity{}
		}
	case os.IsNotExist(err):
	default:
		// 读不了（权限、IO 错误）不是"没有"：此时重建只会得到一个服务端不认的新密钥
		return Identity{}, fmt.Errorf("read %s: %w", path, err)
	}

	changed := false
	if cfg.DeviceID != "" && cfg.DeviceID != id.DeviceID {
		id.DeviceID, changed = cfg.DeviceID, true
	} else if id.DeviceID == "" {
		id.DeviceID, changed = deriveDeviceID(hw), true
	}
	if id.Secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return Identity{}, err
		}
		id.Secret, changed = hex.EncodeToString(b), true
		log.Printf("agent: new identity device_id=%s key=%s (%s)", id.DeviceID, sign.Fingerprint(id.Secret), path)
	} else {
		log.Printf("agent: using existing identity device_id=%s key=%s (%s)", id.DeviceID, sign.Fingerprint(id.Secret), path)
	}
	if changed {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			return Identity{}, err
		}
		data, _ := json.MarshalIndent(id, "", "  ")
		if err := writeFileSync(path, data, 0o600); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

func identityPath(cfg *Config) string { return filepath.Join(cfg.CacheDir, "identity.json") }

// writeFileSync 原子且持久地写文件：写临时文件 → fsync → 改名 → fsync 目录。
// 只 rename 不 fsync 的话，首次注册后不久断电，文件可能是空的或根本不存在。
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// deriveDeviceID 从主机名或硬件序列号派生设备编号。
func deriveDeviceID(hw HardwareInfo) string {
	host := strings.ToLower(strings.TrimSpace(hw.Hostname))
	if host != "" && !defaultHostnames[host] && manifest.DeviceIDPattern.MatchString(host) {
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
