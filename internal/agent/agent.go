// Package agent 实现设备端播放代理：身份注册、轮询清单、下载校验、原子切换、心跳上报、程序更新。
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/sign"
)

// Version 是代理程序版本，构建时经 -ldflags -X 注入；随心跳上报并用于 OTA 判断。
var Version = "dev"

// maxPollBackoff 是轮询失败指数退避的上限。
const maxPollBackoff = 5 * time.Minute

type Agent struct {
	cfg       *Config
	player    player.Player
	api       *http.Client // 清单/心跳/注册：短超时
	dl        *http.Client // 文件下载：不设总超时，靠停滞检测（见 download.go）
	identity  Identity
	hw        HardwareInfo
	version   string // 当前已应用的 manifest 版本
	startedAt time.Time
	failures  int

	updateFailedAt map[string]time.Time
	verified       bool         // 本次运行是否已确认过版本（首个成功心跳后）
	clock          *serverClock // 签名用的时间以服务端为准，见 clock.go
	sched          *schedule    // 轮询/心跳间隔由服务端规定，见 schedule.go
}

// New 创建代理；设备身份在 Run（或 ResolveIdentity）中解析。
func New(cfg *Config, p player.Player) *Agent {
	clock, sched := newServerClock(), newSchedule()
	tr := newTransport(cfg.CacheDir, clock, sched)
	return &Agent{
		cfg:            cfg,
		player:         p,
		api:            &http.Client{Timeout: apiTimeout, Transport: tr},
		dl:             &http.Client{Transport: tr},
		startedAt:      time.Now(),
		updateFailedAt: map[string]time.Time{},
		clock:          clock,
		sched:          sched,
	}
}

func (a *Agent) mediaDir() string    { return filepath.Join(a.cfg.CacheDir, "media") }
func (a *Agent) currentPath() string { return filepath.Join(a.cfg.CacheDir, "current.json") }

// Version 返回当前已应用的 manifest 版本（测试用）。
func (a *Agent) Version() string { return a.version }

// DeviceID 返回解析后的设备编号。
func (a *Agent) DeviceID() string { return a.identity.DeviceID }

// Run 是代理主循环：身份 → 注册 → 启动播放器 → 恢复本地缓存 → 轮询 + 心跳，直到 ctx 取消。
// 返回 ErrRestartForUpdate 表示应退出进程以切换到新版本。
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.mediaDir(), 0o755); err != nil {
		return err
	}
	unlock, err := lockCacheDir(a.cfg.CacheDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.ResolveIdentity(); err != nil {
		return err
	}
	log.Printf("agent: device_id=%s version=%s host=%s serial=%s", a.identity.DeviceID, Version, a.hw.Hostname, a.hw.HWSerial)

	if err := a.player.Start(ctx); err != nil {
		return err
	}
	// 断网兜底：先恢复播放本地已缓存内容，再联网。
	if err := a.LoadCurrent(); err != nil {
		log.Printf("agent: no local playlist to restore (%v)", err)
	}
	sdNotify("READY=1")

	// 自注册（幂等）：成功前不进入正常轮询，但已在播放缓存内容且持续喂狗。
	if err := a.registerLoop(ctx); err != nil {
		return err
	}

	pollTimer := time.NewTimer(0)
	hbTimer := time.NewTimer(0)
	hbEvery := a.sched.Heartbeat()
	wdTicker := time.NewTicker(watchdogInterval) // 不依赖轮询/心跳间隔的配置值
	defer pollTimer.Stop()
	defer hbTimer.Stop()
	defer wdTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollTimer.C:
			sdNotify("WATCHDOG=1")
			changed, err := a.PollOnce(ctx)
			switch {
			case errors.Is(err, ErrRestartForUpdate):
				return err
			case errors.Is(err, errUnknownDevice):
				// 服务端没有这台设备了（运营方在后台删了它）：重新注册，而不是一直 401 下去
				log.Printf("agent: %v; re-registering", err)
				if err := a.registerLoop(ctx); err != nil {
					return err
				}
			case err != nil:
				a.failures++
				log.Printf("agent: poll failed (attempt %d): %v", a.failures, err)
			default:
				a.failures = 0
				if changed {
					log.Printf("agent: applied manifest version %s", a.version)
				}
			}
			pollTimer.Reset(a.pollDelay())
			// 服务端改了心跳间隔：立即按新间隔重排，不等旧间隔走完（旧的可能长达一小时）
			if every := a.sched.Heartbeat(); every != hbEvery {
				hbEvery = every
				hbTimer.Reset(every)
			}
		case <-wdTicker.C:
			sdNotify("WATCHDOG=1")
		case <-hbTimer.C:
			sdNotify("WATCHDOG=1")
			if err := a.Heartbeat(ctx); err != nil {
				log.Printf("agent: heartbeat failed: %v", err)
			}
			hbEvery = a.sched.Heartbeat()
			hbTimer.Reset(hbEvery)
		}
	}
}

// ResolveIdentity 采集硬件信息并确定设备编号/密钥（可单独调用，便于测试）。
func (a *Agent) ResolveIdentity() error {
	a.hw = collectHardwareInfo()
	id, err := loadOrCreateIdentity(a.cfg, a.hw)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	a.identity = id
	return nil
}

// registerLoop 向服务端自注册，失败指数退避重试直到成功或 ctx 取消。
// 期间一直在播放本地缓存，并持续喂狗。
func (a *Agent) registerLoop(ctx context.Context) error {
	delay := 5 * time.Second
	for {
		err := a.Register(ctx)
		if err == nil {
			return nil
		}
		// 密钥冲突要等运营方在后台点「接受新密钥」，点完应尽快生效，所以最多 1 分钟重试一次
		if errors.Is(err, errKeyConflict) && delay > time.Minute {
			delay = time.Minute
		}
		log.Printf("agent: register failed: %v (retry in %s)", err, delay)
		sdNotify("WATCHDOG=1")
		if !sleepFeeding(ctx, delay) {
			return nil
		}
		if delay < maxPollBackoff {
			delay *= 2
		}
	}
}

// errUnknownDevice 表示服务端没有这台设备的记录（被后台删除了），需要重新注册。
var errUnknownDevice = errors.New("server does not know this device")

// statusError 把非预期的响应变成错误，带上服务端给出的原因（401 时说明是时钟、密钥还是设备已删除）。
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusUnauthorized && strings.Contains(msg, "unknown device") {
		return fmt.Errorf("%w (%s)", errUnknownDevice, msg)
	}
	if msg == "" {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return fmt.Errorf("unexpected status %s: %s", resp.Status, msg)
}

// errKeyConflict 表示服务端已有同编号、不同密钥的设备记录。
var errKeyConflict = errors.New("key conflict")

// Register 发送一次注册请求（幂等）。
func (a *Agent) Register(ctx context.Context) error {
	body, err := json.Marshal(manifest.RegisterRequest{
		DeviceID:     a.identity.DeviceID,
		Secret:       a.identity.Secret,
		EnrollToken:  a.cfg.EnrollToken,
		Hostname:     a.hw.Hostname,
		HWSerial:     a.hw.HWSerial,
		MAC:          a.hw.MAC,
		IP:           localIP(a.cfg.ServerURL),
		AgentVersion: Version,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+"/api/v1/device/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch resp.StatusCode {
	case http.StatusCreated:
		log.Printf("agent: registered as new device %s", a.identity.DeviceID)
		return nil
	case http.StatusOK:
		log.Printf("agent: registration confirmed for device %s", a.identity.DeviceID)
		return nil
	case http.StatusConflict:
		// 本机密钥与服务端记录不符：要么 identity.json 丢过（重装系统、换卡、手工删除），
		// 要么另一台机器用了同一个编号。设备已把新密钥报给服务端，后台会显示待确认。
		return fmt.Errorf("%w: device_id=%s is registered on the server with a different key (local key=%s, identity file %s). "+
			"If this really is this device (reinstalled, new SD card, identity file lost), accept the new key in the admin UI; "+
			"if another machine uses the same ID, change the hostname of one of them", errKeyConflict,
			a.identity.DeviceID, sign.Fingerprint(a.identity.Secret), identityPath(a.cfg))
	default:
		return fmt.Errorf("register: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
}

// pollDelay 返回下一次轮询间隔（失败时指数退避）。
func (a *Agent) pollDelay() time.Duration {
	d := a.sched.Poll()
	for i := 0; i < a.failures && d < maxPollBackoff; i++ {
		d *= 2
	}
	if d > maxPollBackoff {
		d = maxPollBackoff
	}
	return d
}

// newRequest 构造带设备签名的请求；签名基于解码后的 URL path。
func (a *Agent) newRequest(ctx context.Context, method, urlPath string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.ServerURL+urlPath, body)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(a.clock.Now().Unix(), 10)
	req.Header.Set(sign.HeaderDeviceID, a.identity.DeviceID)
	req.Header.Set(sign.HeaderTimestamp, ts)
	req.Header.Set(sign.HeaderSign, sign.Sign(a.identity.Secret, ts, method, req.URL.Path))
	return req, nil
}

// PollOnce 拉取一次 manifest；有更新则同步并应用，返回是否发生了变更。
// 清单附带的指令在内容同步完成后执行；更新指令会返回 ErrRestartForUpdate。
func (a *Agent) PollOnce(ctx context.Context) (bool, error) {
	req, err := a.newRequest(ctx, http.MethodGet, "/api/v1/device/manifest", nil)
	if err != nil {
		return false, err
	}
	if a.version != "" {
		req.Header.Set("If-None-Match", `"`+a.version+`"`)
	}
	resp, err := a.api.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("manifest: %w", statusError(resp))
	}

	var m manifest.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return false, fmt.Errorf("manifest: decode: %w", err)
	}
	if m.Version == a.version {
		return false, nil
	}
	if err := a.syncManifest(ctx, &m); err != nil {
		return false, err
	}
	if err := a.handleCommands(ctx, m.Commands); err != nil {
		return true, err
	}
	return true, nil
}

// syncManifest 下载缺失文件、校验、原子落盘 current.json 并切换播放列表。
func (a *Agent) syncManifest(ctx context.Context, m *manifest.Manifest) error {
	for _, item := range m.Downloads() {
		dst := a.localPath(item)
		if fi, err := os.Stat(dst); err == nil && fi.Size() == item.Size {
			continue // 文件名内嵌哈希前缀 + 尺寸一致，视为已就绪
		}
		if err := a.downloadFile(ctx, item.URL, item.SHA256, item.Size, dst); err != nil {
			return fmt.Errorf("download %s: %w", item.Name, err)
		}
	}

	// 全部就绪后才落盘并切换（原子性：中途失败保持旧列表继续播放）。
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.currentPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.currentPath()); err != nil {
		return err
	}

	if err := a.apply(m); err != nil {
		return err
	}
	a.cleanup(m)
	return nil
}

// LoadCurrent 从本地 current.json 恢复播放（启动时断网兜底）。
func (a *Agent) LoadCurrent() error {
	data, err := os.ReadFile(a.currentPath())
	if err != nil {
		return err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for _, item := range m.Downloads() {
		if fi, err := os.Stat(a.localPath(item)); err != nil || fi.Size() != item.Size {
			return fmt.Errorf("cached file %s missing or truncated", item.Name)
		}
	}
	return a.apply(&m)
}

func (a *Agent) apply(m *manifest.Manifest) error {
	scene := player.Scene{Items: make([]player.Item, 0, len(m.Items))}
	for _, it := range m.Items {
		scene.Items = append(scene.Items, player.Item{
			Path:     a.localPath(it),
			Type:     it.Type,
			Duration: it.Duration,
		})
	}
	if l := m.Layout; l != nil {
		// 叠加图只给路径：真正贴图时要按 mpv 的实际输出分辨率光栅化，那是播放器的事。
		scene.Overlay = &player.Overlay{PNG: a.localPath(l.Overlay)}
		scene.Media = player.Rect{X: l.Media.X, Y: l.Media.Y, W: l.Media.W, H: l.Media.H}
		scene.CanvasW, scene.CanvasH = l.CanvasW, l.CanvasH
	}
	if err := a.player.Load(scene); err != nil {
		return err
	}
	a.version = m.Version
	return nil
}

// localPath 返回条目的本地缓存路径；文件名内嵌内容哈希前缀，内容变化即换名。
func (a *Agent) localPath(item manifest.Item) string {
	return filepath.Join(a.mediaDir(), item.SHA256[:12]+"_"+item.Name)
}

// cleanup 删除不再被当前清单引用的缓存文件。
func (a *Agent) cleanup(m *manifest.Manifest) {
	downloads := m.Downloads()
	referenced := make(map[string]bool, len(downloads))
	for _, it := range downloads {
		referenced[filepath.Base(a.localPath(it))] = true
	}
	entries, err := os.ReadDir(a.mediaDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// 保留：被引用的文件本身、它正在续传的 .part、以及由它派生出来的文件
		// （叠加图按输出分辨率光栅化出的 <名字>.<宽>x<高>.bgra）。
		if referenced[name] || derivedFromReferenced(name, referenced) {
			continue
		}
		_ = os.Remove(filepath.Join(a.mediaDir(), name))
	}
}

// derivedFromReferenced 判断 name 是否由某个仍被引用的文件派生而来。
// 派生文件一律是“源文件名 + . + 后缀”：续传中的 a.png.part、
// 叠加图按输出分辨率光栅化出的 a.png.1920x1080.bgra。
func derivedFromReferenced(name string, referenced map[string]bool) bool {
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == '.' && referenced[name[:i]] {
			return true
		}
	}
	return false
}

// Heartbeat 上报一次心跳；本次运行首个成功心跳会确认当前版本（清除 pending-verify）。
func (a *Agent) Heartbeat(ctx context.Context) error {
	stats := a.player.Stats()
	body, err := json.Marshal(manifest.Heartbeat{
		AgentVersion: Version,
		IP:           localIP(a.cfg.ServerURL),
		UptimeS:      uptimeSeconds(a.startedAt),
		DiskFreeMB:   diskFreeMB(a.cfg.CacheDir),
		TempC:        socTempC(),
		HWDec:        stats.HWDec,
		OutputW:      stats.OutputW,
		OutputH:      stats.OutputH,
	})
	if err != nil {
		return err
	}
	req, err := a.newRequest(ctx, http.MethodPost, "/api/v1/device/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat: %w", statusError(resp))
	}
	io.Copy(io.Discard, resp.Body)
	if !a.verified {
		a.verified = true
		a.commitUpdate()
	}
	return nil
}

// uptimeSeconds 优先读系统 /proc/uptime，失败则退化为进程运行时长。
func uptimeSeconds(startedAt time.Time) int64 {
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		var up float64
		if _, err := fmt.Sscanf(string(data), "%f", &up); err == nil {
			return int64(up)
		}
	}
	return int64(time.Since(startedAt).Seconds())
}

// socTempC 读取 SoC 温度（摄氏度），读不到返回 0。
// H3 在软解或高码率视频下很容易过热——Armbian 默认 85°C 触发降频、更高会直接关机，
// 所以温度要能在后台看到，而不是等现场发现屏幕黑了。
func socTempC() int {
	for _, p := range []string{
		"/sys/class/thermal/thermal_zone0/temp",
		"/sys/devices/virtual/thermal/thermal_zone0/temp",
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var milli int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &milli); err != nil {
			continue
		}
		if milli > 1000 { // 多数平台是毫摄氏度
			return milli / 1000
		}
		return milli
	}
	return 0
}

func diskFreeMB(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize) / (1 << 20)
}
