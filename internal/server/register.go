package server

import (
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
// 同 id 同 secret → 幂等 200；同 id 不同 secret → 409（需管理员先删除旧设备）。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.cfg.EnrollToken == "" {
		http.Error(w, "enrollment disabled", http.StatusForbidden)
		return
	}
	var req RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if req.EnrollToken != s.cfg.EnrollToken {
		http.Error(w, "bad enroll token", http.StatusUnauthorized)
		return
	}
	if !deviceIDPattern.MatchString(req.DeviceID) || len(req.Secret) < 32 {
		http.Error(w, "invalid device_id or secret", http.StatusBadRequest)
		return
	}
	// 与静态配置设备同名：只接受密钥一致的情况（等价于幂等）。
	if static, ok := s.devices[req.DeviceID]; ok {
		if static.Secret != req.Secret {
			http.Error(w, "device id conflicts with configured device", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
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
			return nil
		}
		d := existing
		if !ok {
			d = store.Device{ID: req.DeviceID, Secret: req.Secret, Name: req.DeviceID, RegisteredAt: s.now()}
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
		http.Error(w, "device id already registered with a different secret", http.StatusConflict)
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
