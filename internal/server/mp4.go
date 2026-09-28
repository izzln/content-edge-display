package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MP4/MOV 只是容器，光看后缀说明不了能不能播：Orange Pi One 的 H3 芯片只有 H.264 硬解，
// 没有 HEVC 硬解，1440×900 的 H.265 软解也带不动——传上去就是卡顿或黑屏。
// 所以上传时解析容器里的编码 fourcc，当场把放不动的文件拦下来，而不是等现场发现。
//
// 这里只做一件事：把 ISO-BMFF 的盒子结构走一遍，取出各轨道 stsd 里的 fourcc。
// 不引 ffprobe——服务端跑在运营方的小服务器上，少一个外部依赖少一处运维负担。

// 已知的视频编码 fourcc → 人类可读名 / 是否能播。
var videoFourCC = map[string]struct {
	name     string
	playable bool
}{
	"avc1": {"H.264", true},
	"avc3": {"H.264", true},
	"hvc1": {"H.265/HEVC", false},
	"hev1": {"H.265/HEVC", false},
	"vp09": {"VP9", false},
	"av01": {"AV1", false},
	"mp4v": {"MPEG-4 Part 2", false},
}

// containerBoxes 是需要继续往里走的盒子。
var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
}

// maxBoxDepth 防御畸形/恶意文件里的无限嵌套。
const maxBoxDepth = 8

// checkVideoPlayable 解析视频容器，确认设备端能播。
// 返回的错误直接面向使用者（会显示在管理后台上）。
func checkVideoPlayable(r io.ReaderAt, size int64) error {
	codecs, err := sampleFourCCs(r, 0, size, 0)
	if err != nil {
		return fmt.Errorf("无法解析视频文件（不是有效的 MP4/MOV？）：%w", err)
	}
	for _, c := range codecs {
		info, known := videoFourCC[c]
		if !known {
			continue // 音频轨（mp4a 等）或字幕轨，不关心
		}
		if info.playable {
			return nil
		}
		return fmt.Errorf("视频编码是 %s，设备（H3 芯片）只能硬解 H.264，请用 H.264 重新编码后再上传"+
			"（例：ffmpeg -i 原文件 -c:v libx264 -profile:v high -level 4.0 -an 新文件.mp4）", info.name)
	}
	return errors.New("文件里没有找到视频轨道，请确认上传的是视频文件")
}

// sampleFourCCs 递归遍历 [off, off+size) 区间内的盒子，收集 stsd 里的 fourcc。
func sampleFourCCs(r io.ReaderAt, off, size int64, depth int) ([]string, error) {
	if depth > maxBoxDepth {
		return nil, nil
	}
	var out []string
	end := off + size
	for off+8 <= end {
		hdr := make([]byte, 8)
		if _, err := r.ReadAt(hdr, off); err != nil {
			return out, nil // 读到文件尾，按已解析到的内容处理
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		boxType := string(hdr[4:8])
		body := off + 8
		switch boxSize {
		case 0:
			boxSize = end - off // 延伸到末尾
		case 1:
			ext := make([]byte, 8)
			if _, err := r.ReadAt(ext, body); err != nil {
				return out, nil
			}
			boxSize = int64(binary.BigEndian.Uint64(ext))
			body += 8
		}
		if boxSize < body-off || off+boxSize > end {
			return out, fmt.Errorf("盒子 %q 长度非法", boxType)
		}

		switch {
		case boxType == "stsd":
			out = append(out, stsdFourCCs(r, body, off+boxSize)...)
		case containerBoxes[boxType]:
			inner, err := sampleFourCCs(r, body, boxSize-(body-off), depth+1)
			if err != nil {
				return out, err
			}
			out = append(out, inner...)
		}
		off += boxSize
	}
	return out, nil
}

// stsdFourCCs 读取 stsd 的各个 sample entry 的 4 字节编码标识。
// stsd 负载布局：version+flags(4) entry_count(4) 之后是若干 entry，每个 entry 以 size(4)+format(4) 开头。
func stsdFourCCs(r io.ReaderAt, off, end int64) []string {
	head := make([]byte, 8)
	if _, err := r.ReadAt(head, off); err != nil {
		return nil
	}
	count := int(binary.BigEndian.Uint32(head[4:8]))
	off += 8
	var out []string
	for i := 0; i < count && off+8 <= end; i++ {
		e := make([]byte, 8)
		if _, err := r.ReadAt(e, off); err != nil {
			return out
		}
		entrySize := int64(binary.BigEndian.Uint32(e[:4]))
		out = append(out, strings.TrimSpace(string(e[4:8])))
		if entrySize < 8 {
			return out
		}
		off += entrySize
	}
	return out
}
