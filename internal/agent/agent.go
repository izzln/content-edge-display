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
	http      *http.Client
	identity  Identity
	hw        HardwareInfo
	version   string // 当前已应用的 manifest 版本
	startedAt time.Time
	failures  int

	updateFailedAt map[string]time.Time
	verified       bool // 本次运行是否已确认过版本（首个成功心跳后）
}

// New 创建代理；identity 为空时在 Run 中解析。
func New(cfg *Config, p player.Player) *Agent {
	return &Agent{
		cfg:            cfg,
		player:         p,
		http:           &http.Client{Timeout: 10 * time.Minute}, // 覆盖大文件下载
		identity:       Identity{DeviceID: cfg.DeviceID, Secret: cfg.Secret},
		startedAt:      time.Now(),
		updateFailedAt: map[string]time.Time{},
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
	if a.cfg.EnrollToken != "" {
		if err := a.registerLoop(ctx); err != nil {
			return err
		}
	}

	pollTimer := time.NewTimer(0)
	hbTimer := time.NewTimer(0)
	defer pollTimer.Stop()
	defer hbTimer.Stop()

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
		case <-hbTimer.C:
			sdNotify("WATCHDOG=1")
			if err := a.Heartbeat(ctx); err != nil {
				log.Printf("agent: heartbeat failed: %v", err)
			}
			hbTimer.Reset(time.Duration(a.cfg.HeartbeatIntervalS) * time.Second)
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
func (a *Agent) registerLoop(ctx context.Context) error {
	delay := 5 * time.Second
	for {
		err := a.Register(ctx)
		if err == nil {
			return nil
		}
		log.Printf("agent: register failed: %v (retry in %s)", err, delay)
		sdNotify("WATCHDOG=1")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < maxPollBackoff {
			delay *= 2
		}
	}
}

// Register 发送一次注册请求（幂等）。
func (a *Agent) Register(ctx context.Context) error {
	body, err := json.Marshal(map[string]string{
		"device_id":     a.identity.DeviceID,
		"secret":        a.identity.Secret,
		"enroll_token":  a.cfg.EnrollToken,
		"hostname":      a.hw.Hostname,
		"hw_serial":     a.hw.HWSerial,
		"mac":           a.hw.MAC,
		"ip":            localIP(a.cfg.ServerURL),
		"agent_version": Version,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+"/api/v1/device/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if resp.StatusCode == http.StatusCreated {
			log.Printf("agent: registered as new device %s", a.identity.DeviceID)
		}
		return nil
	case http.StatusConflict:
		return fmt.Errorf("device id %s 已被其他密钥注册，请管理员在后台删除该设备后重试", a.identity.DeviceID)
	default:
		return fmt.Errorf("register: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
}

// pollDelay 返回下一次轮询间隔（失败时指数退避）。
func (a *Agent) pollDelay() time.Duration {
	d := time.Duration(a.cfg.PollIntervalS) * time.Second
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
	ts := sign.Now()
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
	resp, err := a.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("manifest: unexpected status %s", resp.Status)
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
	for _, item := range m.Items {
		dst := a.localPath(item)
		if fi, err := os.Stat(dst); err == nil && fi.Size() == item.Size {
			continue // 文件名内嵌哈希前缀 + 尺寸一致，视为已就绪
		}
		if err := a.download(ctx, item, dst); err != nil {
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
	for _, item := range m.Items {
		if fi, err := os.Stat(a.localPath(item)); err != nil || fi.Size() != item.Size {
			return fmt.Errorf("cached file %s missing or truncated", item.Name)
		}
	}
	return a.apply(&m)
}

func (a *Agent) apply(m *manifest.Manifest) error {
	items := make([]player.Item, 0, len(m.Items))
	for _, it := range m.Items {
		items = append(items, player.Item{
			Path:     a.localPath(it),
			Type:     it.Type,
			Duration: it.Duration,
		})
	}
	if err := a.player.Load(items); err != nil {
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
	referenced := make(map[string]bool, len(m.Items))
	for _, it := range m.Items {
		referenced[filepath.Base(a.localPath(it))] = true
	}
	entries, err := os.ReadDir(a.mediaDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		base, isPart := trimSuffix(name, ".part")
		// 被引用的文件、以及仍被引用条目正在续传的 .part 文件都保留。
		if referenced[name] || (isPart && referenced[base]) {
			continue
		}
		_ = os.Remove(filepath.Join(a.mediaDir(), name))
	}
}

func trimSuffix(s, suffix string) (string, bool) {
	if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)], true
	}
	return s, false
}

// Heartbeat 上报一次心跳；本次运行首个成功心跳会确认当前版本（清除 pending-verify）。
func (a *Agent) Heartbeat(ctx context.Context) error {
	hb := map[string]any{
		"version":      a.version,
		"uptime":       uptimeSeconds(a.startedAt),
		"disk_free_mb": diskFreeMB(a.cfg.CacheDir),
		"playing":      a.player.NowPlaying(),
		"player_ver":   Version,
		"ip":           localIP(a.cfg.ServerURL),
	}
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	req, err := a.newRequest(ctx, http.MethodPost, "/api/v1/device/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat: unexpected status %s", resp.Status)
	}
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

func diskFreeMB(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize) / (1 << 20)
}
