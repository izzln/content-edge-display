package manifest

import "regexp"

// 设备的轮询与心跳间隔由服务端统一规定（server.json），随每个设备请求的响应头下发
// （包括 304 与注册响应），设备照办。改间隔只改服务端一处；服务端也因此知道每台设备
// 多久该来一次，能据此判断离线。
const (
	HeaderPollInterval      = "X-Poll-Interval"      // 秒
	HeaderHeartbeatInterval = "X-Heartbeat-Interval" // 秒

	DefaultPollIntervalS      = 10
	DefaultHeartbeatIntervalS = 60
	MinPollIntervalS          = 1
	MaxPollIntervalS          = 300
	MinHeartbeatIntervalS     = 10
	MaxHeartbeatIntervalS     = 3600
)

// DeviceIDPattern 是合法的设备编号：设备拿主机名当编号前要过它，服务端注册时也按它校验。
var DeviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,63}$`)

// RegisterRequest 是设备自注册请求体（POST /api/v1/device/register，不签名，凭 enroll_token）。
type RegisterRequest struct {
	DeviceID     string `json:"device_id"`
	Secret       string `json:"secret"`
	EnrollToken  string `json:"enroll_token"`
	Hostname     string `json:"hostname"`
	HWSerial     string `json:"hw_serial"`
	MAC          string `json:"mac"`
	AgentVersion string `json:"agent_version"`
}

// Heartbeat 是设备心跳上报体（POST /api/v1/device/heartbeat）：健康数据与 OTA 确认。
// 设备正在显示哪个版本不在这里——每次轮询的 If-None-Match 已经带着了；设备 IP 服务端从连接上取。
type Heartbeat struct {
	AgentVersion string `json:"agent_version"` // 设备端程序版本
	UptimeS      int64  `json:"uptime"`        // 系统运行时长：突然变小说明重启过（如过热关机）
	DiskFreeMB   int64  `json:"disk_free_mb"`  // 缓存目录所在分区的剩余空间
	TempC        int    `json:"temp_c,omitempty"`
	// HWDec 是最近一次播放视频用的硬件解码器（如 v4l2slh264dec）；"no" = 退化成了软解
	// （会发热、卡顿，后台标红），空串 = 还没放过视频或问不到。
	HWDec string `json:"hwdec,omitempty"`
	// OutputW/H 是显示屏实际输出分辨率，与模板画布不一致时要查内核的 video= 参数。
	OutputW int `json:"output_w,omitempty"`
	OutputH int `json:"output_h,omitempty"`
	// UpdateError 是最近一次程序更新失败的原因（下载/解包/update.sh），后台设备列表标红显示。
	UpdateError string `json:"update_error,omitempty"`
}
