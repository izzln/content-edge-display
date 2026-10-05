package server

import (
	"log"
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
// 不自动接受，是因为每台设备上都有 enroll_token，自动接受等于谁都能冒充任意设备。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	s.announceSchedule(w) // 注册成功后设备即按服务端规定的间隔轮询、心跳
	var req manifest.RegisterRequest
	if !decodeJSON(w, r, 16<<10, &req) {
		return
	}
	if !tokenOK(req.EnrollToken, s.cfg.EnrollToken) {
		http.Error(w, "bad enroll token", http.StatusUnauthorized)
		return
	}
	if !manifest.DeviceIDPattern.MatchString(req.DeviceID) || len(req.Secret) < 32 {
		http.Error(w, "invalid device_id or secret", http.StatusBadRequest)
		return
	}
	ip := clientIP(r)

	hw := store.Hardware{Hostname: req.Hostname, HWSerial: req.HWSerial, MAC: req.MAC, IP: ip, AgentVersion: req.AgentVersion}
	var conflict, created bool
	if !s.update(w, func(st *store.State) error {
		switch d, ok := st.Devices[req.DeviceID]; {
		case !ok:
			created = true
			st.Devices[req.DeviceID] = &store.Device{ID: req.DeviceID, Secret: req.Secret, RegisteredAt: s.now(), Hardware: hw}
		case d.Secret != req.Secret:
			conflict = true
			d.Rekey = &store.RekeyRequest{Secret: req.Secret, Fingerprint: sign.Fingerprint(req.Secret), At: s.now(), Hardware: hw}
		default:
			d.Hardware = hw
		}
		return nil
	}) {
		return
	}
	switch {
	case conflict:
		log.Printf("device %s asked to register with a new key (key=%s, %s); waiting for confirmation in the admin UI", req.DeviceID, sign.Fingerprint(req.Secret), ip)
		http.Error(w, "device id already registered with a different secret; accept the new key in the admin UI", http.StatusConflict)
	case created:
		log.Printf("new device registered: %s (host %s, serial %s, %s, agent %s)",
			req.DeviceID, req.Hostname, req.HWSerial, ip, req.AgentVersion)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// handleRekey 处理待确认的换密钥请求：{"accept": true} 换成新密钥，false 则丢弃请求。
func (s *Server) handleRekey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Accept bool `json:"accept"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	if s.editDevice(w, r.PathValue("id"), func(d *store.Device) error {
		if d.Rekey == nil {
			return errConflict("该设备没有待确认的换密钥请求")
		}
		if req.Accept {
			d.Secret, d.Hardware = d.Rekey.Secret, d.Rekey.Hardware
		}
		d.Rekey = nil
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
