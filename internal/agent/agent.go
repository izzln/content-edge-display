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
	"sync/atomic"
	"time"

	"github.com/izzln/content-edge-display/internal/fsutil"
	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/player"
	"github.com/izzln/content-edge-display/internal/sign"
)

// Version 是代理程序版本，构建时经 -ldflags -X 注入；随心跳上报并用于 OTA 判断。
var Version = "dev"

// maxBackoff 是请求失败后重试间隔的上限。
const maxBackoff = 5 * time.Minute

type Agent struct {
	cfg      *Config
	player   player.Player
	api      *http.Client // 清单/心跳/注册：短超时
	dl       *http.Client // 文件下载：不设总超时，靠停滞检测（见 download.go）
	clock    *serverClock // 签名用的时间以服务端为准，见 clock.go
	sched    *schedule    // 轮询/心跳间隔由服务端规定，见 schedule.go
	install  installLayout
	identity deviceIdentity
	hw       hardwareInfo

	registered  bool
	manifestVer string    // 当前已应用的清单版本
	testUntil   time.Time // 正在显示的测试卡的到期时间（服务端时间）；零值 = 不是测试卡，见 testcard.go
	failures    int       // 连续失败次数（决定重试退避）
	link        linkStatus

	update   *manifest.Update // 清单里待执行的程序更新（update.go）
	updRetry retry
	verified bool   // 本次运行是否已确认过版本（首个成功心跳后）
	status   []byte // 上次写进 status.env 的内容

	access    *manifest.Access // 清单里的访问凭据（access.go）
	accRetry  retry
	accessFP  string       // 已应用的访问凭据的指纹
	accessSys accessTarget // 凭据落地的位置（测试里换成临时目录）

	brightness  int    // 当前设给播放器的亮度（百分比），见 brightness.go
	noMedia     bool   // 当前是否停播媒体区（同上）
	brightSaved string // 已存盘的亮度计划（FormatBrightness 的结果）

	zoneSys   zoneTarget                    // 系统时区文件的位置（timezone.go）；零值 = 不设（测试、未以服务方式运行时）
	zoneRetry retry                         // 设时区失败后隔一会儿再试
	zone      atomic.Pointer[time.Location] // 设好的系统时区：救援屏按它显示时间（time.Local 只在启动时读一次）
}

// retry 记录一项随清单下发、失败后要隔一会儿再试的操作（程序更新、访问凭据）：原因随心跳上报。
type retry struct {
	every time.Duration
	at    time.Time // 上次失败的时间
	err   string    // 上次失败的原因
}

func (r *retry) due() bool       { return time.Since(r.at) >= r.every }
func (r *retry) fail(err string) { r.at, r.err = time.Now(), err }
func (r *retry) clear()          { r.at, r.err = time.Time{}, "" }

// New 创建代理；设备身份在 Run 中解析。
func New(cfg *Config, p player.Player) *Agent {
	clock, sched := newServerClock(), newSchedule()
	tr := newTransport(cfg.CacheDir, cfg.TLSFingerprint, clock, sched)
	a := &Agent{
		cfg:     cfg,
		player:  p,
		api:     &http.Client{Timeout: apiTimeout, Transport: tr},
		dl:      &http.Client{Transport: tr},
		clock:   clock,
		sched:   sched,
		install: detectInstallLayout(),

		updRetry:   retry{every: updateRetryInterval},
		accRetry:   retry{every: accessRetryInterval},
		accessSys:  systemAccess,
		zoneRetry:  retry{every: accessRetryInterval},
		brightness: 100,
	}
	a.accessFP = a.loadAccessFP()
	a.loadBrightness()
	return a
}

func (a *Agent) mediaDir() string    { return filepath.Join(a.cfg.CacheDir, "media") }
func (a *Agent) currentPath() string { return filepath.Join(a.cfg.CacheDir, "current.json") }

// Run 是代理主循环：身份 → 启动播放器 → 恢复本地缓存 → 注册 + 轮询 + 心跳，直到 ctx 取消。
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
	if err := a.resolveIdentity(); err != nil {
		return err
	}
	a.clock.setSystem = setSystemClock // 以服务方式运行时才校准系统时钟（见 clock.go）与系统时区（timezone.go）
	a.zoneSys = systemZone
	log.Printf("agent: device_id=%s version=%s host=%s serial=%s", a.identity.DeviceID, Version, a.hw.Hostname, a.hw.HWSerial)

	if err := a.player.Start(ctx); err != nil {
		return err
	}
	// 断网兜底：先恢复播放本地已缓存内容（连同上次存下的亮度计划），再联网。
	if err := a.loadCurrent(); err != nil {
		log.Printf("agent: no local playlist to restore (%v)", err)
	}
	a.applyBrightness()
	sdNotify("READY=1")
	if a.cfg.Player == "gst" { // 只有真正占着显示屏时才需要：插键盘按任意键交还控制台
		go a.rescueLoop(ctx, watchKeyboards(ctx), vtConsole{}, rescueIdle)
	}

	pollTimer := time.NewTimer(0)
	hbEvery := a.sched.Heartbeat()
	hbTimer := time.NewTimer(hbEvery)
	firstBeat := true
	wd := time.NewTicker(watchdogInterval)    // 等待期间（含失败退避的几分钟）持续喂狗
	bright := time.NewTicker(brightnessCheck) // 分时段亮度到点切换，与能否联系上服务端无关
	// 测试卡到点自己退回正常内容（联系不上服务端时也照样，见 testcard.go）；每次内容变化后重新排
	testTimer := time.NewTimer(time.Hour)
	armTest := func() {
		testTimer.Stop()
		if d, ok := a.testRemaining(); ok {
			testTimer.Reset(d)
		}
	}
	armTest()
	defer testTimer.Stop()
	defer bright.Stop()
	defer pollTimer.Stop()
	defer hbTimer.Stop()
	defer wd.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-wd.C:
			sdNotify("WATCHDOG=1")
		case <-bright.C:
			a.applyBrightness()
		case <-testTimer.C:
			a.expireTest()
			armTest()
		case <-pollTimer.C:
			err := a.step(ctx)
			if errors.Is(err, ErrRestartForUpdate) {
				return err
			}
			armTest()
			pollTimer.Reset(a.retryDelay(err))
			if firstBeat && a.registered { // 刚注册上：立刻心跳，确认新版本、让后台尽快看到健康数据
				firstBeat = false
				hbTimer.Reset(0)
			} else if every := a.sched.Heartbeat(); every != hbEvery {
				hbEvery = every // 服务端改了心跳间隔：立即按新间隔重排，不等旧间隔（可能长达一小时）走完
				hbTimer.Reset(every)
			}
		case <-hbTimer.C:
			a.writeStatus(a.player.Stats()) // 连不上服务端时现场自检最需要它，与心跳成败无关
			if a.registered {
				if err := a.contact(func() error { return a.heartbeat(ctx) }); err != nil {
					log.Printf("agent: heartbeat failed: %v", err)
				}
			}
			hbEvery = a.sched.Heartbeat()
			hbTimer.Reset(hbEvery)
		}
	}
}

// step 是一轮联系服务端：没注册就先注册，然后拉清单、执行待执行的程序更新。
func (a *Agent) step(ctx context.Context) error {
	err := a.contact(func() error {
		if !a.registered {
			if err := a.register(ctx); err != nil {
				return fmt.Errorf("register: %w", err)
			}
			a.registered = true
		}
		err := a.poll(ctx)
		if errors.Is(err, errUnknownDevice) {
			a.registered = false // 运营方在后台删了这台设备：下一轮重新注册，而不是一直 401 下去
		}
		return err
	})
	if err != nil {
		a.failures++
		log.Printf("agent: %v (attempt %d, retry in %s)", err, a.failures, a.retryDelay(err))
		return err
	}
	a.failures = 0
	a.applyAccess()
	a.applyZone()
	a.applyBrightness()
	return a.applyPendingUpdate(ctx)
}

// retryDelay 返回下一轮的间隔：正常按服务端规定的轮询间隔，失败时指数退避。
func (a *Agent) retryDelay(err error) time.Duration {
	d := a.sched.Poll()
	for i := 0; i < a.failures && d < maxBackoff; i++ {
		d *= 2
	}
	d = min(d, maxBackoff)
	if errors.Is(err, errKeyConflict) {
		// 密钥冲突要等运营方在后台点「接受新密钥」，点完应尽快生效
		d = min(d, time.Minute)
	}
	return d
}

// resolveIdentity 采集硬件信息并确定设备编号/密钥。
func (a *Agent) resolveIdentity() error {
	a.hw = collectHardwareInfo()
	id, err := loadOrCreateIdentity(a.cfg.CacheDir, a.hw)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	a.identity = id
	return nil
}

var (
	errClockSkew     = errors.New("clock skew")                       // 服务端因时钟偏差拒绝了签名
	errUnknownDevice = errors.New("server does not know this device") // 被后台删除了，需要重新注册
	errKeyConflict   = errors.New("key conflict")                     // 服务端已有同编号、不同密钥的设备
)

// contact 执行一次与服务端的往来（拉清单、心跳），记下连接状态（救援信息里显示）：
//   - 因时钟偏差被拒时立即再试一次——偏差已从这次响应的 Date 头学到（clock.go），第二次就按服务端时间签名了。
//     没有 RTC 的设备断电重启后时间回退，不这样做就要白等一个轮询周期（还会被计入失败退避）；
//   - 出错时丢掉空闲连接：失败多半是复用了一条已被对端断掉的长连接（connection reset / EOF），下次用新连接。
func (a *Agent) contact(do func() error) error {
	err := do()
	if errors.Is(err, errClockSkew) {
		log.Printf("agent: request rejected for clock skew, retrying with the server's time")
		err = do()
	}
	a.link.record(err)
	if err != nil {
		a.api.CloseIdleConnections()
	}
	return err
}

// statusError 把非预期的响应变成错误，带上服务端给出的原因（401 时说明是时钟、密钥还是设备已删除）。
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	switch {
	case resp.StatusCode == http.StatusUnauthorized && strings.Contains(msg, "unknown device"):
		return fmt.Errorf("%w (%s)", errUnknownDevice, msg)
	case resp.StatusCode == http.StatusUnauthorized && strings.Contains(msg, "clock skew"):
		return fmt.Errorf("%w (%s)", errClockSkew, msg)
	case msg == "":
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return fmt.Errorf("unexpected status %s: %s", resp.Status, msg)
}

// newRequest 构造发给服务端的请求；signed 时带设备签名（基于解码后的 URL path），注册请求不签名。
func (a *Agent) newRequest(ctx context.Context, method, urlPath string, body io.Reader, signed bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.ServerURL+urlPath, body)
	if err != nil || !signed {
		return req, err
	}
	ts := strconv.FormatInt(a.clock.Now().Unix(), 10)
	req.Header.Set(sign.HeaderDeviceID, a.identity.DeviceID)
	req.Header.Set(sign.HeaderTimestamp, ts)
	req.Header.Set(sign.HeaderSign, sign.Sign(a.identity.Secret, ts, method, req.URL.Path))
	return req, nil
}

// postJSON 把 v 以 JSON POST 到 urlPath；signed 为 false 时不带设备签名（注册）。
func (a *Agent) postJSON(ctx context.Context, urlPath string, v any, signed bool) (*http.Response, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := a.newRequest(ctx, http.MethodPost, urlPath, bytes.NewReader(body), signed)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return a.api.Do(req)
}

// register 发送一次注册请求（幂等）。
func (a *Agent) register(ctx context.Context) error {
	resp, err := a.postJSON(ctx, "/api/v1/device/register", manifest.RegisterRequest{
		DeviceID:     a.identity.DeviceID,
		Secret:       a.identity.Secret,
		EnrollToken:  a.cfg.EnrollToken,
		Hostname:     a.hw.Hostname,
		HWSerial:     a.hw.HWSerial,
		MAC:          a.hw.MAC,
		AgentVersion: Version,
	}, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		log.Printf("agent: registered as device %s", a.identity.DeviceID)
		return nil
	case http.StatusConflict:
		// 本机密钥与服务端记录不符：要么 identity.json 丢过（重装系统、换卡、手工删除），
		// 要么另一台机器用了同一个编号。设备已把新密钥报给服务端，后台会显示待确认。
		return fmt.Errorf("%w: device_id=%s is registered on the server with a different key (local key=%s). "+
			"If this really is this device (reinstalled, new SD card, identity file lost), accept the new key in the admin UI; "+
			"if another machine uses the same ID, change the hostname of one of them",
			errKeyConflict, a.identity.DeviceID, sign.Fingerprint(a.identity.Secret))
	}
	return statusError(resp)
}

// poll 拉取一次清单；有更新则同步内容并应用。
func (a *Agent) poll(ctx context.Context) error {
	req, err := a.newRequest(ctx, http.MethodGet, "/api/v1/device/manifest", nil, true)
	if err != nil {
		return err
	}
	if a.manifestVer != "" {
		req.Header.Set("If-None-Match", `"`+a.manifestVer+`"`)
	}
	resp, err := a.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("manifest: %w", statusError(resp))
	}
	var m manifest.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return fmt.Errorf("manifest: decode: %w", err)
	}
	if err := a.syncManifest(ctx, &m); err != nil {
		return err
	}
	log.Printf("agent: applied manifest version %s", m.Version)
	return nil
}

// syncManifest 下载缺失文件、校验、原子落盘 current.json 并切换播放列表。
func (a *Agent) syncManifest(ctx context.Context, m *manifest.Manifest) error {
	for _, item := range m.Downloads() {
		if a.cached(item) {
			continue
		}
		if err := a.downloadFile(ctx, item.URL, item.SHA256, item.Size, a.localPath(item)); err != nil {
			return fmt.Errorf("download %s: %w", item.Name, err)
		}
	}
	// 全部就绪后才落盘并切换（中途失败保持旧列表继续播放）。
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	a.keepBase(m) // 换成测试卡前记下正常内容，到期自己退回（testcard.go）
	if err := fsutil.WriteFile(a.currentPath(), data, 0o644); err != nil {
		return err
	}
	if err := a.apply(m); err != nil {
		return err
	}
	a.cleanup(m)
	return nil
}

// loadCurrent 从本地 current.json 恢复播放（启动时断网兜底）。记着的是已经过期的测试卡时，
// 恢复测试卡之前的正常内容。
func (a *Agent) loadCurrent() error {
	m, err := readManifest(a.currentPath())
	if err != nil {
		return err
	}
	if a.testExpired(m.Expires) && a.backToBase() == nil {
		return nil
	}
	return a.restore(m)
}

// restore 播放本地已缓存的清单 m。
func (a *Agent) restore(m *manifest.Manifest) error {
	// 个别文件坏了（SD 卡出错、被人删了）也要把其余的放起来，不能因为一个文件黑屏：
	// 缺的条目跳过；叠加图缺了就整屏播放。这样恢复的列表不完整，不记它的版本号——
	// 联系上服务端后拿到完整清单，把缺的补下载回来（记了版本号的话服务端只会回 304）。
	var missing []string
	items := m.Items[:0:0]
	for _, it := range m.Items {
		if a.cached(it) {
			items = append(items, it)
		} else {
			missing = append(missing, it.Name)
		}
	}
	if m.Layout != nil && !a.cached(m.Layout.Overlay) {
		missing = append(missing, m.Layout.Overlay.Name)
		m.Layout = nil
	}
	if len(missing) == 0 {
		return a.apply(m)
	}
	if len(items) == 0 && m.Layout == nil {
		return fmt.Errorf("cached files missing or truncated: %s", strings.Join(missing, ", "))
	}
	log.Printf("agent: cached files missing or truncated, playing the rest until the server is reachable: %s", strings.Join(missing, ", "))
	m.Items = items
	if err := a.apply(m); err != nil {
		return err
	}
	a.manifestVer = ""
	return nil
}

// cached 判断条目的文件已在本地缓存里：文件名内嵌内容哈希前缀、尺寸一致即视为完整（下载时已校验过 sha256）。
func (a *Agent) cached(item manifest.Item) bool {
	fi, err := os.Stat(a.localPath(item))
	return err == nil && fi.Size() == item.Size
}

func (a *Agent) apply(m *manifest.Manifest) error {
	scene := player.Scene{Items: make([]player.Item, 0, len(m.Items))}
	for _, it := range m.Items {
		scene.Items = append(scene.Items, player.Item{Path: a.localPath(it), Type: it.Type, Duration: it.Duration})
	}
	if l := m.Layout; l != nil {
		// 叠加图只给路径：真正贴图时要按显示屏的实际输出分辨率光栅化，那是播放器的事。
		scene.OverlayPNG, scene.Media = a.localPath(l.Overlay), l.Media
	}
	if err := a.player.Load(scene); err != nil {
		return err
	}
	a.manifestVer = m.Version
	a.setUpdate(m.Update)
	a.access = m.Access
	a.testUntil = m.Expires
	return nil
}

// localPath 返回条目的本地缓存路径；文件名内嵌内容哈希前缀，内容变化即换名。
func (a *Agent) localPath(item manifest.Item) string {
	return filepath.Join(a.mediaDir(), item.SHA256[:12]+"_"+item.Name)
}

// cleanup 删除不再被当前清单引用的缓存文件。显示测试卡期间，测试卡之前的正常内容也保留（到期要退回去）。
func (a *Agent) cleanup(m *manifest.Manifest) {
	referenced := map[string]bool{}
	keep := m.Downloads()
	if !m.Expires.IsZero() {
		if b, err := readManifest(a.basePath()); err == nil {
			keep = append(keep, b.Downloads()...)
		}
	}
	for _, it := range keep {
		referenced[filepath.Base(a.localPath(it))] = true
	}
	entries, err := os.ReadDir(a.mediaDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		// 保留：被引用的文件本身、它正在续传的 .part、由它派生出来的文件（叠加图按输出分辨率光栅化出的
		// <名字>.<宽>x<高>.bgra），以及正在写的临时文件（隐藏文件）。
		if name := e.Name(); !referenced[name] && !derivedFromReferenced(name, referenced) && !strings.HasPrefix(name, ".") {
			os.Remove(filepath.Join(a.mediaDir(), name))
		}
	}
}

// derivedFromReferenced 判断 name 是否由某个仍被引用的文件派生而来（"源文件名.后缀"）。
func derivedFromReferenced(name string, referenced map[string]bool) bool {
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == '.' && referenced[name[:i]] {
			return true
		}
	}
	return false
}

// heartbeat 上报一次心跳；本次运行首个成功心跳会确认当前版本（清除 pending-verify）。
func (a *Agent) heartbeat(ctx context.Context) error {
	st := a.player.Stats()
	resp, err := a.postJSON(ctx, "/api/v1/device/heartbeat", manifest.Heartbeat{
		AgentVersion: Version,
		UptimeS:      uptimeSeconds(),
		DiskFreeMB:   fsutil.DiskFree(a.cfg.CacheDir) >> 20,
		TempC:        socTempC(),
		HWDec:        st.HWDec,
		OutputW:      st.OutputW,
		OutputH:      st.OutputH,
		UpdateError:  a.updRetry.err,
		AccessError:  a.accRetry.err,
	}, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("heartbeat: %w", statusError(resp))
	}
	if !a.verified {
		a.verified = true
		a.commitUpdate()
	}
	return nil
}

// writeStatus 在每个心跳周期把播放状态留一份在本地（仅变化时写盘）：shell 变量格式，现场自检脚本
// check-display.sh 直接读入，据此报告解码方式与输出分辨率（DISPLAY_MODE 是期望的分辨率，空 = 显示屏首选）。
func (a *Agent) writeStatus(st player.Stats) {
	data := fmt.Appendf(nil, "HWDEC=%s\nOUTPUT=%dx%d\nDISPLAY_MODE=%s\n", st.HWDec, st.OutputW, st.OutputH, a.cfg.DisplayMode)
	if bytes.Equal(data, a.status) {
		return
	}
	if fsutil.WriteFile(filepath.Join(a.cfg.CacheDir, statusFile), data, 0o644) == nil {
		a.status = data
	}
}

// statusFile 是 cache_dir 下的播放状态文件（见 writeStatus）。
const statusFile = "status.env"

// uptimeSeconds 读系统运行时长（/proc/uptime）。
func uptimeSeconds() int64 {
	var up float64
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		fmt.Sscanf(string(data), "%f", &up)
	}
	return int64(up)
}

// socTempC 读取 SoC 温度（摄氏度），读不到返回 0。
// H3 在软解或高码率视频下很容易过热——Armbian 默认 85°C 触发降频、更高会直接关机，
// 所以温度要能在后台看到，而不是等现场发现屏幕黑了。
func socTempC() int {
	var milli int
	if data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &milli)
	}
	return milli / 1000
}
