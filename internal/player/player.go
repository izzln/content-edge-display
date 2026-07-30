// Package player 抽象端侧播放器：mpv（生产）与 null（测试/无显示环境）。
package player

import "context"

// Item 是一条本地播放条目。
type Item struct {
	Path     string
	Type     string // "image" | "video"
	Duration int    // 秒；仅图片有效
}

// Player 是播放器统一接口。
type Player interface {
	// Start 启动播放器，非阻塞；ctx 取消后播放器应退出。
	Start(ctx context.Context) error
	// Load 用新列表替换当前播放列表（列表为空表示黑屏待机）。
	Load(items []Item) error
	// NowPlaying 返回当前播放条目的本地路径，未知时为空串。
	NowPlaying() string
}
