package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
	"github.com/izzln/content-edge-display/internal/transcode"
)

// videoEncoder 是转码器的最小接口（生产用 transcode.Encoder，测试可注入假实现）。
type videoEncoder interface {
	Video(ctx context.Context, src, dst string, spec transcode.Spec, onProgress func(seconds float64)) error
	Duration(ctx context.Context, src string) float64
	Version() string
}

// pdfRenderer 是 PDF 渲染器的最小接口（生产用 transcode.PDFRenderer）。
type pdfRenderer interface {
	Pages(ctx context.Context, src string) (int, error)
	Render(ctx context.Context, src, dir string, pages int, onPage func(done int)) error
	Version() string
}

// 任务状态。
const (
	jobQueued  = "queued"
	jobRunning = "transcoding"
	jobFailed  = "failed"
)

// 任务种类。
const (
	jobVideo = "video" // 视频转码
	jobPDF   = "pdf"   // PDF 逐页渲染
)

// mediaJob 是一个排队中的媒体处理任务：视频转码或 PDF 逐页渲染。
//
// 放在后台做：一段几百 MB 的原片转码可能要几分钟，同步做会让上传请求超时。
// 运营方上传后立刻看到进度，完成后自动出现在播放列表末尾。
type mediaJob struct {
	kind     string
	deviceID string
	name     string // 最终文件名（视频容器统一为 .mp4；PDF 保持原名）
	key      string // 缓存区的处理键（原片 sha256 + 处理参数），完成后登记，供同一原片复用
	src      string // 暂存的原片
	status   string
	progress int // 0~100；拿不到视频时长时恒为 0
	pages    int // PDF 总页数（读出来之前为 0）
	err      string
	cancel   context.CancelFunc
}

// jobQueue 串行执行任务：运营方的小服务器同时跑几个 ffmpeg 只会互相拖慢。
type jobQueue struct {
	mu   sync.Mutex
	jobs []*mediaJob
	wake chan struct{}
}

func newJobQueue() *jobQueue { return &jobQueue{wake: make(chan struct{}, 1)} }

func (q *jobQueue) add(j *mediaJob) {
	q.mu.Lock()
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// snapshot 返回某台设备的任务副本（按提交顺序）。
func (q *jobQueue) snapshot(deviceID string) []mediaJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []mediaJob
	for _, j := range q.jobs {
		if j.deviceID == deviceID {
			out = append(out, *j)
		}
	}
	return out
}

// next 取出第一个排队中的任务并标记为运行中。
func (q *jobQueue) next() *mediaJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.status == jobQueued {
			j.status = jobRunning
			return j
		}
	}
	return nil
}

// remove 删除某台设备的一个任务；运行中的会被取消（ffmpeg 随之被杀）。返回是否找到。
func (q *jobQueue) remove(deviceID, name string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.removeLocked(func(j *mediaJob) bool { return j.deviceID == deviceID && j.name == name })
}

// removeDevice 删除某台设备的全部任务（删除设备时用）。
func (q *jobQueue) removeDevice(deviceID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.removeLocked(func(j *mediaJob) bool { return j.deviceID == deviceID })
}

func (q *jobQueue) removeLocked(match func(*mediaJob) bool) bool {
	found := false
	kept := q.jobs[:0]
	for _, j := range q.jobs {
		if !match(j) {
			kept = append(kept, j)
			continue
		}
		found = true
		if j.cancel != nil {
			j.cancel() // 运行中：杀掉 ffmpeg/pdftoppm，原片由工作协程清理
		} else {
			os.Remove(j.src) // 排队中/已失败：原片在这里清理
		}
	}
	q.jobs = kept
	return found
}

func (q *jobQueue) update(j *mediaJob, fn func(*mediaJob)) {
	q.mu.Lock()
	fn(j)
	q.mu.Unlock()
}

// alive 判断任务是否还在队列里（运营方可能在转码过程中删了它）。
func (q *jobQueue) alive(j *mediaJob) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, x := range q.jobs {
		if x == j {
			return true
		}
	}
	return false
}

func (q *jobQueue) drop(j *mediaJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, x := range q.jobs {
		if x == j {
			q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
			return
		}
	}
}

// progressLogInterval 是转码进度写进控制台的间隔（测试会调小）。
var progressLogInterval = 30 * time.Second

// incomingDir 存放待转码的原片。
func (s *Server) incomingDir() string { return filepath.Join(s.cfg.DataDir, "incoming") }

// runJobs 是媒体处理工作协程，随服务端生命周期运行。
func (s *Server) runJobs(ctx context.Context) {
	for {
		j := s.jobs.next()
		if j == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.jobs.wake:
				continue
			}
		}
		if j.kind == jobPDF {
			s.renderPDF(ctx, j)
		} else {
			s.transcodeOne(ctx, j)
		}
	}
}

func (s *Server) transcodeOne(ctx context.Context, j *mediaJob) {
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.jobs.update(j, func(j *mediaJob) { j.cancel = cancel })
	defer os.Remove(j.src)

	start := time.Now()
	enc := s.videoEncoder() // 任务只在有转码器时入队
	total := enc.Duration(jctx, j.src)
	var srcSize int64
	if fi, err := os.Stat(j.src); err == nil {
		srcSize = fi.Size()
	}
	log.Printf("transcode started: device %s, %s (source %s, %.0fs)", j.deviceID, j.name, humanBytes(srcSize), total)
	dir := s.deviceMediaDir(j.deviceID)
	dst := filepath.Join(dir, j.name)
	lastLog := start
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = enc.Video(jctx, j.src, dst, transcode.DefaultSpec(), func(sec float64) {
			p := 0
			if total > 0 {
				p = min(int(sec*100/total), 99)
				s.jobs.update(j, func(j *mediaJob) { j.progress = p })
			}
			// 每 progressLogInterval 报一次进度：长视频要转好几分钟，控制台不能一直没动静
			if time.Since(lastLog) >= progressLogInterval {
				lastLog = time.Now()
				if total > 0 {
					log.Printf("transcoding: device %s, %s %d%% (%s elapsed)", j.deviceID, j.name, p, time.Since(start).Round(time.Second))
				} else {
					log.Printf("transcoding: device %s, %s %.0fs done (%s elapsed)", j.deviceID, j.name, sec, time.Since(start).Round(time.Second))
				}
			}
		})
	}
	if !s.jobs.alive(j) {
		// 运营方在转码期间删掉了它：产物（若已生成）也不要
		os.Remove(dst)
		return
	}
	if err != nil {
		log.Printf("transcode failed: device %s, %s (after %s): %v", j.deviceID, j.name, time.Since(start).Round(time.Second), err)
		s.jobs.update(j, func(j *mediaJob) { j.status, j.err, j.cancel = jobFailed, err.Error(), nil })
		return
	}
	// 完成：纳入缓存区、追加到播放列表末尾，然后从队列里摘掉
	if err := s.cacheAdopt(dir, j.name, j.key); err != nil {
		log.Printf("cache: cannot add %s to the cache: %v", j.name, err)
	}
	s.kickCache()
	if err := s.appendPlaylist(j.deviceID, j.name); err != nil {
		log.Printf("transcoded but adding to playlist failed: device %s, %s: %v", j.deviceID, j.name, err)
	}
	s.jobs.drop(j)
	var outSize int64
	if fi, err := os.Stat(dst); err == nil {
		outSize = fi.Size()
	}
	log.Printf("transcode done: device %s, %s (%s -> %s in %s), added to playlist",
		j.deviceID, j.name, humanBytes(srcSize), humanBytes(outSize), time.Since(start).Round(time.Second))
}

// PDF 上传限制：页数多了渲染慢、清单长，轮播一圈也没人看得完。
const (
	maxPDFUploadBytes = 50 << 20
	maxPDFPages       = 100
)

// renderPDF 把一个 PDF 逐页渲染成图片：页面写进 <媒体目录>/.pages/<文件名>/，
// 全部完成后原 PDF 才放进媒体目录——它一出现，播放列表与清单就把它当成就绪的文档。
func (s *Server) renderPDF(ctx context.Context, j *mediaJob) {
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.jobs.update(j, func(j *mediaJob) { j.cancel = cancel })
	defer os.Remove(j.src)

	start := time.Now()
	dir := s.deviceMediaDir(j.deviceID)
	dst := filepath.Join(dir, j.name)
	pagesDir := filepath.Join(dir, manifest.PagesDir, j.name)
	tmp := filepath.Join(dir, manifest.PagesDir, "."+j.name+".tmp")
	defer os.RemoveAll(tmp)

	pages, uiErr, err := 0, "", error(nil)
	r := s.pdfRenderer() // 任务只在有渲染器时入队
	if pages, err = r.Pages(jctx, j.src); err != nil {
		uiErr = "无法解析 PDF（文件损坏或不是 PDF）"
		if errors.Is(err, transcode.ErrPDFEncrypted) {
			uiErr = "PDF 有密码保护，请导出不带密码的版本再上传"
		}
	} else if pages < 1 {
		uiErr, err = "PDF 没有页面", errors.New("no pages")
	} else if pages > maxPDFPages {
		uiErr = fmt.Sprintf("PDF 有 %d 页，超过上限 %d 页，请拆分后再上传", pages, maxPDFPages)
		err = fmt.Errorf("%d pages exceeds the %d page limit", pages, maxPDFPages)
	}
	if err == nil {
		log.Printf("pdf rendering started: device %s, %s (%d pages)", j.deviceID, j.name, pages)
		s.jobs.update(j, func(j *mediaJob) { j.pages = pages })
		uiErr = "页面渲染失败"
		os.RemoveAll(tmp)
		if err = os.MkdirAll(tmp, 0o755); err == nil {
			err = r.Render(jctx, j.src, tmp, pages, func(done int) {
				s.jobs.update(j, func(j *mediaJob) { j.progress = min(done*100/pages, 99) })
			})
		}
		if err == nil {
			os.RemoveAll(pagesDir)
			if err = os.Rename(tmp, pagesDir); err == nil {
				err = moveFile(j.src, dst)
			}
		}
	}
	if !s.jobs.alive(j) {
		// 运营方在渲染期间删掉了它
		os.RemoveAll(pagesDir)
		os.Remove(dst)
		return
	}
	if err != nil {
		os.RemoveAll(pagesDir)
		log.Printf("pdf rendering failed: device %s, %s: %v", j.deviceID, j.name, err)
		s.jobs.update(j, func(j *mediaJob) { j.status, j.err, j.cancel = jobFailed, uiErr, nil })
		return
	}
	if err := s.cacheAdopt(dir, j.name, j.key); err != nil {
		log.Printf("cache: cannot add %s to the cache: %v", j.name, err)
	}
	s.kickCache()
	if err := s.appendPlaylist(j.deviceID, j.name); err != nil {
		log.Printf("pdf rendered but adding to playlist failed: device %s, %s: %v", j.deviceID, j.name, err)
	}
	s.jobs.drop(j)
	log.Printf("pdf rendering done: device %s, %s (%d pages in %s), added to playlist",
		j.deviceID, j.name, pages, time.Since(start).Round(time.Second))
}

// moveFile 把文件移到 dst：暂存区（data_dir）与媒体目录（media_root）可能不在同一个文件系统，
// 改名不行时退回复制。
func moveFile(src, dst string) error {
	if os.Rename(src, dst) == nil {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}
