package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"regexp"

	"github.com/izzln/content-edge-display/internal/store"
)

var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,63}$`)

// RegisterRequest 是设备自注册请求体。
type RegisterRequest struct {
	DeviceID     string `json:"device_id"`
	Secret       string `json:"secret"`
	EnrollToken  string `json:"enroll_token"`
	Hostname     string `json:"hostname"`
	HWSerial     string `json:"hw_serial"`
	MAC          string `json:"mac"`
	IP           string `json:"ip"`
	AgentVersion string `json:"agent_version"`
}

// handleRegister 处理设备首启自注册：
// enroll_token 正确 → 新设备写入 store（管理后台立即可见）；
// 同 id 同 secret → 幂等 200；
// 同 id 不同 secret → 409，并把新密钥记为待确认请求：运营方在后台核对后点「接受新密钥」，
// 设备下次重试即可注册成功，属性、播放列表等配置都保留（不必删设备重来）。
// 不自动接受，是因为 enroll_token 烧在每台设备里，自动接受等于谁都能冒充任意设备。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	s.announceSchedule(w) // 注册成功后设备即按服务端规定的间隔轮询、心跳
	if s.cfg.EnrollToken == "" {
		http.Error(w, "enrollment disabled", http.StatusForbidden)
		return
	}
	var req RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.EnrollToken), []byte(s.cfg.EnrollToken)) != 1 {
		http.Error(w, "bad enroll token", http.StatusUnauthorized)
		return
	}
	if !deviceIDPattern.MatchString(req.DeviceID) || len(req.Secret) < 32 {
		http.Error(w, "invalid device_id or secret", http.StatusBadRequest)
		return
	}
	if req.IP == "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			req.IP = host
		}
	}

	conflict := false
	created := false
	err := s.store.Update(func(st *store.State) error {
		existing, ok := st.Devices[req.DeviceID]
		if ok && existing.Secret != req.Secret {
			conflict = true
			existing.Rekey = &store.RekeyRequest{
				Secret: req.Secret, Fingerprint: keyFingerprint(req.Secret), At: s.now(),
				IP: req.IP, Hostname: req.Hostname, HWSerial: req.HWSerial, MAC: req.MAC,
			}
			st.Devices[req.DeviceID] = existing
			return nil
		}
		d := existing
		if !ok {
			d = store.Device{ID: req.DeviceID, Secret: req.Secret, RegisteredAt: s.now()}
			created = true
		}
		d.Hostname, d.HWSerial, d.MAC, d.IP, d.AgentVersion = req.Hostname, req.HWSerial, req.MAC, req.IP, req.AgentVersion
		st.Devices[req.DeviceID] = d
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conflict {
		log.Printf("register: device %s 用新密钥（key=%s，ip=%s）请求注册，等待后台确认",
			req.DeviceID, keyFingerprint(req.Secret), req.IP)
		http.Error(w, "device id already registered with a different secret; accept the new key in the admin UI", http.StatusConflict)
		return
	}
	if created {
		log.Printf("register: new device %s (host=%s serial=%s ip=%s agent=%s)",
			req.DeviceID, req.Hostname, req.HWSerial, req.IP, req.AgentVersion)
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// keyFingerprint 返回密钥指纹（sha256 前 8 位），与设备端日志里的 key= 一致。
func keyFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:4])
}

// handleRekey 处理待确认的换密钥请求：{"accept": true} 换成新密钥，false 则丢弃请求。
func (s *Server) handleRekey(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.pathDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Accept bool `json:"accept"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var missing bool
	err := s.store.Update(func(st *store.State) error {
		d := st.Devices[dev.ID]
		if d.Rekey == nil {
			missing = true
			return nil
		}
		if req.Accept {
			d.Secret = d.Rekey.Secret
			d.Hostname, d.HWSerial, d.MAC = d.Rekey.Hostname, d.Rekey.HWSerial, d.Rekey.MAC
			if d.Rekey.IP != "" {
				d.IP = d.Rekey.IP
			}
		}
		d.Rekey = nil
		st.Devices[dev.ID] = d
		return nil
	})
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case missing:
		http.Error(w, "该设备没有待确认的换密钥请求", http.StatusConflict)
	default:
		action := "忽略"
		if req.Accept {
			action = "接受"
		}
		log.Printf("admin: device %s 的换密钥请求已%s", dev.ID, action)
		w.WriteHeader(http.StatusNoContent)
	}
}
