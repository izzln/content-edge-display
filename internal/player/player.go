// Package player 抽象端侧播放器：GStreamer（生产，见 gst.go）与 null（测试/无显示环境）。
package player

import (
	"context"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// Item 是一条本地播放条目。
type Item struct {
	Path     string
	Type     string // "image" | "video"
	Duration int    // 秒；仅图片有效
}

// Scene 是设备当前应呈现的完整画面。
//
// OverlayPNG 为空时整屏播放 Items（测试卡、以及已渲染成整屏图的模板）。
// 不为空时它是贴在画面之上的模板叠加图（服务端按模板画布渲染的 PNG，媒体区透明）：播放区被限制在
// Media 矩形内并按 cover 撑满，属性、文字变了只需换一张叠加图，播放内容不受影响。
// 叠加图与 Media 都按画布坐标给出：显示屏的实际输出分辨率只有播放器知道，由它换算（见 rasterize）。
type Scene struct {
	Items      []Item
	OverlayPNG string
	Media      manifest.Rect // 播放区在画布中的位置（有叠加图时有效）
}

// Player 是播放器统一接口。
type Player interface {
	// Start 启动播放器，非阻塞；ctx 取消后播放器应退出。
	Start(ctx context.Context) error
	// Load 用新画面替换当前画面（Items 为空表示黑屏待机）。
	Load(scene Scene) error
	// SetPaused 暂停/恢复播放：暂停时播放进程退出、释放显示屏（内核随即把控制台显示回来），
	// 期间 Load 只记下画面，恢复时再播放。现场救援（插键盘看控制台）用它。
	SetPaused(paused bool)
	// Stats 返回随心跳上报的播放器运行状态（实际解码方式、输出分辨率）；不阻塞，问不到的项留空。
	Stats() Stats
}

// Stats 是播放器的运行状态，用于后台观察现场是否正常。
type Stats struct {
	// HWDec 是最近一次播放视频用的硬件解码器（如 v4l2slh264dec）；"no" 表示退化成了软解
	// （cedrus 没加载、插件没装），空串表示还没放过视频。硬解是必须的，退化了要在后台能看到。
	HWDec string
	// OutputW/H 是显示屏实际输出分辨率。与模板画布不一致时叠加图会被缩放，
	// 对不上通常说明内核没吃下 video= 参数或换了块屏。
	OutputW, OutputH int
}
