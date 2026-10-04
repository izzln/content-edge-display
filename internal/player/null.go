package player

import (
	"context"
	"log"
	"sync"
)

// Null 是不驱动真实显示的播放器实现，用于集成测试与无显示环境验证。
type Null struct {
	mu     sync.Mutex
	scene  Scene
	paused bool
}

func NewNull() *Null { return &Null{} }

func (p *Null) Start(ctx context.Context) error { return nil }

func (p *Null) Load(scene Scene) error {
	p.mu.Lock()
	p.scene = scene
	p.mu.Unlock()
	if scene.Overlay != nil {
		log.Printf("player(null): loaded %d item(s) + overlay in %dx%d at (%d,%d)",
			len(scene.Items), scene.Media.W, scene.Media.H, scene.Media.X, scene.Media.Y)
	} else {
		log.Printf("player(null): loaded %d item(s) fullscreen", len(scene.Items))
	}
	return nil
}

// Scene 返回最近一次加载的画面（测试用）。
func (p *Null) Scene() Scene {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scene
}

func (p *Null) SetPaused(paused bool) {
	p.mu.Lock()
	p.paused = paused
	p.mu.Unlock()
	log.Printf("player(null): paused=%v", paused)
}

// Paused 返回是否处于暂停（测试用）。
func (p *Null) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func (p *Null) Stats() Stats { return Stats{} }
