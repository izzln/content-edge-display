package agent

import (
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// schedule 是设备的轮询与心跳间隔。间隔由服务端统一规定，随每个响应头下发
// （manifest.HeaderPollInterval），设备照办：改间隔只改服务端一处，所有设备下一次请求就跟上。
// 联系上服务端之前用内置默认值。
type schedule struct {
	poll, heartbeat atomic.Int64 // 秒
}

func newSchedule() *schedule {
	s := &schedule{}
	s.poll.Store(manifest.DefaultPollIntervalS)
	s.heartbeat.Store(manifest.DefaultHeartbeatIntervalS)
	return s
}

func (s *schedule) Poll() time.Duration      { return time.Duration(s.poll.Load()) * time.Second }
func (s *schedule) Heartbeat() time.Duration { return time.Duration(s.heartbeat.Load()) * time.Second }

// observe 从响应头学习间隔；超出合理范围的值收进范围内（服务端配错了也不能把设备带偏）。
func (s *schedule) observe(h http.Header) {
	learn := func(v *atomic.Int64, header string, lo, hi int64, what string) {
		n, err := strconv.ParseInt(h.Get(header), 10, 64)
		if err != nil {
			return // 旧版服务端不发这个头：保持现状
		}
		n = min(max(n, lo), hi)
		if old := v.Swap(n); old != n {
			log.Printf("agent: 按服务端设定，%s间隔 %ds → %ds", what, old, n)
		}
	}
	learn(&s.poll, manifest.HeaderPollInterval, manifest.MinPollIntervalS, manifest.MaxPollIntervalS, "轮询")
	learn(&s.heartbeat, manifest.HeaderHeartbeatInterval, manifest.MinHeartbeatIntervalS, manifest.MaxHeartbeatIntervalS, "心跳")
}
