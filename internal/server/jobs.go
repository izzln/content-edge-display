package server

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/izzln/content-edge-display/internal/store"
	"github.com/izzln/content-edge-display/internal/transcode"
)

// videoEncoder 是转码器的最小接口（生产用 transcode.Encoder，测试可注入假实现）。
type videoEncoder interface {
	Video(ctx context.Context, src, dst string, spec transcode.Spec, onProgress func(seconds float64)) error
	Duration(ctx context.Context, src string) float64
	Version() string
}

// 转码任务状态。
const (
	jobQueued  = "queued"
	jobRunning = "transcoding"
	jobFailed  = "failed"
)

// transcodeJob 是一个排队中的视频转码任务。
//
// 转码放在后台做：一段几百 MB 的原片转码可能要几分钟，同步做会让上传请求超时。
// 运营方上传后立刻看到"转码中 xx%"，完成后自动出现在播放列表末尾。
type transcodeJob struct {
	deviceID string
	name     string // 最终文件名（容器统一为 .mp4）
	src      string // 暂存的原片
	status   string
	progress int // 0~100；拿不到时长时恒为 0
	err      string
	cancel   context.CancelFunc
}

// jobQueue 串行执行转码：运营方的小服务器同时跑几个 ffmpeg 只会互相拖慢。
type jobQueue struct {
	mu   sync.Mutex
	jobs []*transcodeJob
	wake chan struct{}
}

func newJobQueue() *jobQueue { return &jobQueue{wake: make(chan struct{}, 1)} }

func (q *jobQueue) add(j *transcodeJob) {
	q.mu.Lock()
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// snapshot 返回某台设备的任务副本（按提交顺序）。
func (q *jobQueue) snapshot(deviceID string) []transcodeJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []transcodeJob
	for _, j := range q.jobs {
		if j.deviceID == deviceID {
			out = append(out, *j)
		}
	}
	return out
}

// next 取出第一个排队中的任务并标记为运行中。
func (q *jobQueue) next() *transcodeJob {
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
	return q.removeLocked(func(j *transcodeJob) bool { return j.deviceID == deviceID && j.name == name })
}

// removeDevice 删除某台设备的全部任务（删除设备时用）。
func (q *jobQueue) removeDevice(deviceID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.removeLocked(func(j *transcodeJob) bool { return j.deviceID == deviceID })
}

func (q *jobQueue) removeLocked(match func(*transcodeJob) bool) bool {
	found := false
	kept := q.jobs[:0]
	for _, j := range q.jobs {
		if !match(j) {
			kept = append(kept, j)
			continue
		}
		found = true
		if j.cancel != nil {
			j.cancel() // 运行中：杀掉 ffmpeg，原片由工作协程清理
		} else {
			os.Remove(j.src) // 排队中/已失败：原片在这里清理
		}
	}
	q.jobs = kept
	return found
}

func (q *jobQueue) update(j *transcodeJob, fn func(*transcodeJob)) {
	q.mu.Lock()
	fn(j)
	q.mu.Unlock()
}

// alive 判断任务是否还在队列里（运营方可能在转码过程中删了它）。
func (q *jobQueue) alive(j *transcodeJob) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, x := range q.jobs {
		if x == j {
			return true
		}
	}
	return false
}

func (q *jobQueue) drop(j *transcodeJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, x := range q.jobs {
		if x == j {
			q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
			return
		}
	}
}

// incomingDir 存放待转码的原片。
func (s *Server) incomingDir() string { return filepath.Join(s.cfg.DataDir, "incoming") }

// runTranscoder 是转码工作协程，随服务端生命周期运行。
func (s *Server) runTranscoder(ctx context.Context) {
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
		s.transcodeOne(ctx, j)
	}
}

func (s *Server) transcodeOne(ctx context.Context, j *transcodeJob) {
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.jobs.update(j, func(j *transcodeJob) { j.cancel = cancel })
	defer os.Remove(j.src)

	start := time.Now()
	total := s.encoder.Duration(jctx, j.src)
	dir := s.deviceMediaDir(j.deviceID)
	dst := filepath.Join(dir, j.name)
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = s.encoder.Video(jctx, j.src, dst, transcode.DefaultSpec(), func(sec float64) {
			if total > 0 {
				p := int(sec * 100 / total)
				s.jobs.update(j, func(j *transcodeJob) { j.progress = min(p, 99) })
			}
		})
	}
	if !s.jobs.alive(j) {
		// 运营方在转码期间删掉了它：产物（若已生成）也不要
		os.Remove(dst)
		return
	}
	if err != nil {
		log.Printf("transcode %s/%s failed after %s: %v", j.deviceID, j.name, time.Since(start).Round(time.Second), err)
		s.jobs.update(j, func(j *transcodeJob) { j.status, j.err, j.cancel = jobFailed, err.Error(), nil })
		return
	}
	// 完成：追加到播放列表末尾，然后从队列里摘掉
	if err := s.store.Update(func(st *store.State) error {
		d := st.Displays[j.deviceID]
		if d.Mode == "" {
			d.Mode = store.ModeGlobal
		}
		d.Playlist = append(d.Playlist, j.name)
		st.Displays[j.deviceID] = d
		return nil
	}); err != nil {
		log.Printf("transcode %s/%s: update playlist failed: %v", j.deviceID, j.name, err)
	}
	s.jobs.drop(j)
	log.Printf("transcode %s/%s done in %s", j.deviceID, j.name, time.Since(start).Round(time.Second))
}
