// Package player 抽象端侧播放器：mpv（生产）与 null（测试/无显示环境）。
package player

import "context"

// Item 是一条本地播放条目。
type Item struct {
	Path     string
	Type     string // "image" | "video"
	Duration int    // 秒；仅图片有效
}

// Rect 是画布上的一个矩形（像素）。
type Rect struct{ X, Y, W, H int }

// Overlay 是贴在画面之上的模板叠加层：一块解码好的 BGRA 原始像素
// （mpv 的 overlay-add 只吃原始像素，不认 PNG）。
type Overlay struct {
	Path string // BGRA 数据文件路径，w*h*4 字节
	W, H int
}

// Scene 是设备当前应呈现的完整画面。
//
// Overlay 为 nil 时整屏播放 Items（无模板的目录轮播、测试卡、以及已渲染成整屏图的模板）。
// Overlay 非 nil 时播放区被限制在 Media 矩形内并按 cover 撑满，叠加层盖在其余部分之上——
// 这样模板区域里可以直接放视频，不需要服务端转码。
type Scene struct {
	Items   []Item
	Overlay *Overlay
	Media   Rect // 播放区在画布中的位置（Overlay 非 nil 时有效）
	CanvasW int
	CanvasH int
}

// Player 是播放器统一接口。
type Player interface {
	// Start 启动播放器，非阻塞；ctx 取消后播放器应退出。
	Start(ctx context.Context) error
	// Load 用新画面替换当前画面（Items 为空表示黑屏待机）。
	Load(scene Scene) error
	// NowPlaying 返回当前播放条目的本地路径，未知时为空串。
	NowPlaying() string
}
