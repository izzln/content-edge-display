package player

import (
	"context"
	"log"
	"sync"
)

// Null 是不驱动真实显示的播放器实现，用于集成测试与无显示环境验证。
type Null struct {
	mu    sync.Mutex
	items []Item
}

func NewNull() *Null { return &Null{} }

func (p *Null) Start(ctx context.Context) error { return nil }

func (p *Null) Load(items []Item) error {
	p.mu.Lock()
	p.items = items
	p.mu.Unlock()
	log.Printf("player(null): loaded %d item(s)", len(items))
	return nil
}

func (p *Null) NowPlaying() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.items) == 0 {
		return ""
	}
	return p.items[0].Path
}
