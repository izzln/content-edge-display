package agent

import (
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// schedule 是设备的轮询与心跳间隔，以及系统该用的时区。都由服务端统一规定，随每个响应头下发
// （manifest.HeaderPollInterval、manifest.HeaderTimezone），设备照办：改设置只改服务端一处，
// 所有设备下一次请求就跟上。联系上服务端之前间隔用内置默认值，时区不动。
type schedule struct {
	poll, heartbeat atomic.Int64 // 秒
	zone            atomic.Pointer[string]
}

func newSchedule() *schedule {
	s := &schedule{}
	s.poll.Store(manifest.DefaultPollIntervalS)
	s.heartbeat.Store(manifest.DefaultHeartbeatIntervalS)
	return s
}

func (s *schedule) Poll() time.Duration      { return time.Duration(s.poll.Load()) * time.Second }
func (s *schedule) Heartbeat() time.Duration { return time.Duration(s.heartbeat.Load()) * time.Second }

// Zone 返回服务端的时区名称（如 Asia/Shanghai）；还没学到时为空。
func (s *schedule) Zone() string {
	if z := s.zone.Load(); z != nil {
		return *z
	}
	return ""
}

// observe 从响应头学习间隔；超出合理范围的值收进范围内（服务端配错了也不能把设备带偏）。
func (s *schedule) observe(h http.Header) {
	learn := func(v *atomic.Int64, header string, lo, hi int64, what string) {
		n, err := strconv.ParseInt(h.Get(header), 10, 64)
		if err != nil {
			return // 响应里没带（如代理链路剥掉了头）：保持现状
		}
		n = min(max(n, lo), hi)
		if old := v.Swap(n); old != n {
			log.Printf("agent: %s interval set by server: %ds -> %ds", what, old, n)
		}
	}
	learn(&s.poll, manifest.HeaderPollInterval, manifest.MinPollIntervalS, manifest.MaxPollIntervalS, "poll")
	learn(&s.heartbeat, manifest.HeaderHeartbeatInterval, manifest.MinHeartbeatIntervalS, manifest.MaxHeartbeatIntervalS, "heartbeat")
	if z := h.Get(manifest.HeaderTimezone); z != "" {
		s.zone.Store(&z)
	}
}
