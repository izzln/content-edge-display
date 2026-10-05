package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
)

// deviceIdentity 是设备的持久化身份。
type deviceIdentity struct {
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

// hardwareInfo 随注册上报，便于运营方识别机器。
type hardwareInfo struct {
	Hostname string
	HWSerial string
	MAC      string
}

// 公版镜像的默认主机名不能当设备编号（每台新装的设备都一样）。
var defaultHostnames = map[string]bool{
	"orangepione": true, "orangepi": true, "armbian": true, "localhost": true, "debian": true,
}

// loadOrCreateIdentity 解析设备身份：cache_dir/identity.json 有就用；没有就按主机名（非默认值）或
// SoC 序列号/MAC 派生编号、随机生成密钥并持久化，重启/重试不变。
//
// 这个文件丢了或读不出来，设备就会用**同一个编号、新的密钥**去注册，服务端会当成冒名顶替
// 拒绝（409）。所以：写入要落盘（fsync）；读不出来时不悄悄重建，而是把坏文件留作现场证据、
// 大声报出来——新密钥要运营方在后台确认后才生效。
func loadOrCreateIdentity(cacheDir string, hw hardwareInfo) (deviceIdentity, error) {
	path := filepath.Join(cacheDir, "identity.json")
	var id deviceIdentity
	switch data, err := os.ReadFile(path); {
	case err == nil:
		if err := json.Unmarshal(data, &id); err == nil && id.DeviceID != "" && id.Secret != "" {
			log.Printf("agent: using existing identity device_id=%s key=%s (%s)", id.DeviceID, sign.Fingerprint(id.Secret), path)
			return id, nil
		}
		bad := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
		os.Rename(path, bad)
		log.Printf("agent: WARNING %s is invalid; moved to %s and generating a new identity. "+
			"The server will reject the new key until it is accepted in the admin UI", path, bad)
	case os.IsNotExist(err):
	default:
		// 读不了（权限、IO 错误）不是"没有"：此时重建只会得到一个服务端不认的新密钥
		return deviceIdentity{}, fmt.Errorf("read %s: %w", path, err)
	}

	id = deviceIdentity{DeviceID: deriveDeviceID(hw), Secret: rand.Text() + rand.Text()}
	data, _ := json.MarshalIndent(id, "", "  ")
	if err := fsutil.WriteFile(path, data, 0o600); err != nil {
		return deviceIdentity{}, err
	}
	log.Printf("agent: new identity device_id=%s key=%s (%s)", id.DeviceID, sign.Fingerprint(id.Secret), path)
	return id, nil
}

// deriveDeviceID 从主机名或硬件序列号派生设备编号。
func deriveDeviceID(hw hardwareInfo) string {
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
	return "opi-" + strings.ToLower(rand.Text()[:8])
}

// collectHardwareInfo 读取主机名、SoC 序列号（全志 SID → /proc/cpuinfo Serial）与第一块网卡的 MAC。
func collectHardwareInfo() hardwareInfo {
	var hw hardwareInfo
	hw.Hostname, _ = os.Hostname()
	if data, err := os.ReadFile("/sys/bus/nvmem/devices/sunxi-sid0/nvmem"); err == nil && len(data) >= 16 {
		hw.HWSerial = hex.EncodeToString(data[:16])
	} else if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Serial" {
				hw.HWSerial = strings.TrimSpace(v)
			}
		}
	}
	if ifs, err := net.Interfaces(); err == nil {
		for _, ifc := range ifs {
			if ifc.Flags&net.FlagLoopback == 0 && len(ifc.HardwareAddr) > 0 {
				hw.MAC = ifc.HardwareAddr.String()
				break
			}
		}
	}
	return hw
}
