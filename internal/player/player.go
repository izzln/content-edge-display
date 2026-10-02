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

// Overlay 是贴在画面之上的模板叠加层：服务端渲染好的整屏 PNG，媒体区是透明的。
//
// 这里只给路径，不给尺寸：真正贴图时要按 mpv 的实际输出分辨率重新光栅化
// （显示屏真实输出不一定等于模板画布尺寸），这件事只有播放器知道。
type Overlay struct {
	PNG string // 本地 PNG 路径
}

// Scene 是设备当前应呈现的完整画面。
//
// Overlay 为 nil 时整屏播放 Items（测试卡、以及已渲染成整屏图的模板）。
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
	// Stats 返回随心跳上报的播放器运行状态（如实际解码方式、输出分辨率）。
	// 问不到的项留空，不影响心跳。
	Stats() Stats
}

// Stats 是播放器的运行状态，用于后台观察现场是否正常。
type Stats struct {
	// HWDec 是 mpv 实际使用的硬解方式；"no" 表示软解（Armbian 自带的 mpv 驱动不了 H3 的
	// 硬件解码器，软解是常态；换装打过 v4l2request 补丁的 mpv 后，这里能看出硬解是否生效）。
	HWDec string
	// OutputW/H 是显示屏实际输出分辨率。与模板画布不一致时叠加图会被缩放，
	// 对不上通常说明内核没吃下 video= 参数或换了块屏。
	OutputW, OutputH int
}
