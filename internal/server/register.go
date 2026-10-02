package server

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/sign"
	"github.com/izzln/content-edge-display/internal/store"
)

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
	var req manifest.RegisterRequest
	if !decodeJSON(w, r, 16<<10, &req) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.EnrollToken), []byte(s.cfg.EnrollToken)) != 1 {
		http.Error(w, "bad enroll token", http.StatusUnauthorized)
		return
	}
	if !manifest.DeviceIDPattern.MatchString(req.DeviceID) || len(req.Secret) < 32 {
		http.Error(w, "invalid device_id or secret", http.StatusBadRequest)
		return
	}
	if req.IP == "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			req.IP = host
		}
	}

	var conflict, created bool
	if !s.update(w, func(st *store.State) {
		d, ok := st.Devices[req.DeviceID]
		if ok && d.Secret != req.Secret {
			conflict = true
			d.Rekey = &store.RekeyRequest{
				Secret: req.Secret, Fingerprint: sign.Fingerprint(req.Secret), At: s.now(),
				IP: req.IP, Hostname: req.Hostname, HWSerial: req.HWSerial, MAC: req.MAC,
			}
		} else {
			if !ok {
				d, created = store.Device{ID: req.DeviceID, Secret: req.Secret, RegisteredAt: s.now()}, true
			}
			d.Hostname, d.HWSerial, d.MAC, d.IP, d.AgentVersion = req.Hostname, req.HWSerial, req.MAC, req.IP, req.AgentVersion
		}
		st.Devices[req.DeviceID] = d
	}) {
		return
	}
	switch {
	case conflict:
		log.Printf("设备 %s 用新密钥（key=%s，%s）请求注册，等待后台确认", req.DeviceID, sign.Fingerprint(req.Secret), req.IP)
		http.Error(w, "device id already registered with a different secret; accept the new key in the admin UI", http.StatusConflict)
	case created:
		log.Printf("新设备注册 %s（主机名 %s，序列号 %s，%s，程序 %s）",
			req.DeviceID, req.Hostname, req.HWSerial, req.IP, req.AgentVersion)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusOK)
	}
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
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	var missing bool
	if !s.update(w, func(st *store.State) {
		d := st.Devices[dev.ID]
		if d.Rekey == nil {
			missing = true
			return
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
	}) {
		return
	}
	if missing {
		http.Error(w, "该设备没有待确认的换密钥请求", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
