// Package transcode 把运营方上传的素材归一化成设备能稳定播放的形态。
//
// 为什么非转码不可：设备上的 mpv 是**软解**的——H3 的硬件解码器（cedrus）需要 V4L2 Request API，
// Armbian/Debian 自带的 FFmpeg/mpv 不支持（补丁至今未进 FFmpeg 上游），所以视频全靠 4 个 A7 核心解。
// 原片动辄 1080p、10~20Mbps，软解不动，硬撑就发热，到 85°C 开始降频、再高直接关机。
// 所以上传时统一压成软解吃得消的 H.264：1440×900 以内、30fps 以内、码率 4Mbps 以内，
// 并用 x264 的 fastdecode 调优；顺带把声音去掉（屏幕一律静音）并把 moov 放到文件头（faststart）。
//
// 图片同理但不用 ffmpeg：纯 Go 缩到画布尺寸即可，省得设备上解一张几千万像素的图。
package transcode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Spec 是转码目标，取值见 DefaultSpec。
type Spec struct {
	MaxW, MaxH  int // 输出不超过这个尺寸（等比缩小，不放大）
	MaxFPS      int // 帧率上限
	BitrateK    int // 目标码率（kbps）
	MaxBitrateK int // 瞬时码率上限（kbps）
}

// DefaultSpec 针对 Orange Pi One + 1440×900 的取值。
//
// 4Mbps 对 1440×900 的宣传片绰绰有余（蓝光 1080p 也就 20~40Mbps，而这里分辨率更低、
// 内容多为静态画面与缓慢运镜），同时把解码与发热压在 H3 吃得消的范围内。
func DefaultSpec() Spec {
	return Spec{MaxW: 1440, MaxH: 900, MaxFPS: 30, BitrateK: 2500, MaxBitrateK: 4000}
}

// Encoder 封装一个可用的 ffmpeg。
type Encoder struct {
	bin     string
	version string
	fpsMax  bool // ffmpeg 是否支持 -fpsmax（5.1 起）
}

// Find 定位并试运行 ffmpeg：bin 为空时在 PATH 里找。不可用时返回的错误说明了具体原因。
//
// "明明装了却说找不到"几乎都是运行环境不同：服务端由 systemd 以 display 用户启动，
// PATH 只有 /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin，没有登录 shell 里的
// /snap/bin、~/bin、/opt/…；装在 /root 下的文件 display 用户也读不了；snap 版 ffmpeg
// 要往 $HOME/snap 写数据，系统用户没有家目录会直接失败。所以错误里带上实际 PATH 与运行用户。
func Find(bin string) (*Encoder, error) {
	if bin == "" {
		bin = "ffmpeg"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) && !strings.ContainsRune(bin, os.PathSeparator) {
			return nil, fmt.Errorf("%s not found in PATH (service PATH=%s, user %s)", bin, os.Getenv("PATH"), currentUser())
		}
		return nil, fmt.Errorf("%s is not usable (user %s): %v", bin, currentUser(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-hide_banner", "-version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("found %s but it fails to run as user %s (%v): %s", path, currentUser(), err, firstLine(out))
	}
	e := &Encoder{bin: path, version: firstLine(out)}
	e.fpsMax = e.supportsFPSMax()
	return e, nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return strconv.Itoa(os.Getuid())
}

// supportsFPSMax 试跑一帧看 -fpsmax 认不认。老版本 ffmpeg（< 5.1）没有这个选项，
// 直接传会让整个转码失败，所以启动时探一次。
func (e *Encoder) supportsFPSMax() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "nullsrc=s=16x16:d=0.1", "-fpsmax", "30",
		"-frames:v", "1", "-f", "null", "-")
	return cmd.Run() == nil
}

// Version 返回 ffmpeg 的版本行，用于在管理后台显示。
func (e *Encoder) Version() string {
	if e == nil {
		return ""
	}
	return e.version
}

// Video 把 src 转成设备能稳定播放的 H.264 MP4 写到 dst。
// onProgress 以已处理的秒数回调（可为 nil），用于在后台显示进度。
func (e *Encoder) Video(ctx context.Context, src, dst string, spec Spec, onProgress func(seconds float64)) error {
	// 写到同目录的隐藏文件里，完成后再改名：转码可能要几分钟，半成品如果是可见文件，
	// 会被当成就绪内容列进播放列表、甚至下发给设备。媒体目录扫描会跳过 . 开头的文件。
	// 后缀保留 .mp4，ffmpeg 靠它选封装格式。
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp.mp4")
	defer os.Remove(tmp)

	cmd := exec.CommandContext(ctx, e.bin, e.videoArgs(src, tmp, spec)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	readProgress(stdout, onProgress)
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return fmt.Errorf("ffmpeg failed: %s", msg)
	}
	return os.Rename(tmp, dst)
}

// videoArgs 返回把 src 转成 dst 的 ffmpeg 参数。
func (e *Encoder) videoArgs(src, dst string, spec Spec) []string {
	// scale 用 min(,) 包住：只缩不放，原本就小的素材不要被拉大（拉大只会更糊更费码率）。
	// force_divisible_by=2 保证宽高是偶数，yuv420p 必需。
	vf := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2",
		spec.MaxW, spec.MaxH)
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", src,
		"-vf", vf,
		"-c:v", "libx264",
		"-profile:v", "high", "-level", "4.0",
		"-preset", "veryfast",
		// fastdecode：关掉 CABAC、环路滤波和加权预测，解码 CPU 省三四成；代价是同画质下文件大 10~20%。
		// 设备是软解，这笔账划算——解码越轻越不容易发热、掉帧。
		"-tune", "fastdecode",
		"-pix_fmt", "yuv420p",
		"-b:v", strconv.Itoa(spec.BitrateK) + "k",
		"-maxrate", strconv.Itoa(spec.MaxBitrateK) + "k",
		"-bufsize", strconv.Itoa(spec.MaxBitrateK*2) + "k",
		"-g", "60", // 2 秒一个关键帧，循环播放时跳转快
		"-an",                     // 屏幕一律静音，音轨纯属浪费码率
		"-movflags", "+faststart", // moov 放文件头
		"-map_metadata", "-1", // 不带入原始元数据（可能含拍摄地点等）
		"-progress", "pipe:1", "-nostats", "-loglevel", "error",
	}
	if e.fpsMax {
		args = append(args, "-fpsmax", strconv.Itoa(spec.MaxFPS))
	}
	return append(args, dst)
}

// Duration 返回素材时长（秒），用于把转码进度换算成百分比；拿不到返回 0。
// 直接解析 `ffmpeg -i` 打印的 "Duration: 00:01:23.45"，不额外依赖 ffprobe。
func (e *Encoder) Duration(ctx context.Context, src string) float64 {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 没有输出文件时 ffmpeg 以非零码退出，但信息已经打到 stderr 了
	out, _ := exec.CommandContext(ctx, e.bin, "-hide_banner", "-nostdin", "-i", src).CombinedOutput()
	return parseDuration(string(out))
}

func parseDuration(s string) float64 {
	i := strings.Index(s, "Duration: ")
	if i < 0 {
		return 0
	}
	var hh, mm int
	var ss float64
	if _, err := fmt.Sscanf(s[i+len("Duration: "):], "%d:%d:%f", &hh, &mm, &ss); err != nil {
		return 0
	}
	return float64(hh*3600+mm*60) + ss
}

// readProgress 解析 ffmpeg -progress 的键值流（out_time_us=12345678）。
func readProgress(r io.Reader, onProgress func(float64)) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || k != "out_time_us" || onProgress == nil {
			continue
		}
		if us, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && us > 0 {
			onProgress(us / 1e6)
		}
	}
	io.Copy(io.Discard, r)
}

// ShrinkImage 把过大的图片等比缩到 maxW×maxH 以内，原地覆盖；本来就够小则原样保留。
// 返回是否真的缩了。
//
// 设备只有 1GB 内存，而解码后的位图是 宽×高×4 字节——一张 4000×3000 的图就是 48MB，
// 屏幕上却只显示 1440×900。在服务端缩一次，所有设备都省。
func ShrinkImage(path string, maxW, maxH int) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	src, format, err := image.Decode(f)
	f.Close()
	if err != nil {
		return false, err
	}
	b := src.Bounds()
	if b.Dx() <= maxW && b.Dy() <= maxH {
		return false, nil
	}
	w, h := fitWithin(b.Dx(), b.Dy(), maxW, maxH)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)

	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	switch format {
	case "png":
		err = png.Encode(out, dst)
	default:
		err = jpeg.Encode(out, dst, &jpeg.Options{Quality: 88})
	}
	out.Close()
	if err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// Thumbnail 把图片等比缩到 maxW×maxH 以内，以 JPEG 写出（后台列表的缩略图）。
// 透明部分垫白，免得 PNG 的透明区在 JPEG 里变黑。
func Thumbnail(w io.Writer, path string, maxW, maxH int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	b := src.Bounds()
	tw, th := b.Dx(), b.Dy()
	if tw > maxW || th > maxH {
		tw, th = fitWithin(tw, th, maxW, maxH)
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.Draw(dst, dst.Bounds(), image.White, image.Point{}, xdraw.Src)
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return jpeg.Encode(w, dst, &jpeg.Options{Quality: 80})
}

// fitWithin 等比缩小到 maxW×maxH 以内。
func fitWithin(w, h, maxW, maxH int) (int, int) {
	if w*maxH > maxW*h {
		return maxW, max(1, h*maxW/w)
	}
	return max(1, w*maxH/h), maxH
}

// OutputName 返回转码产物的文件名：容器统一成 mp4。
func OutputName(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name)) + ".mp4"
}
