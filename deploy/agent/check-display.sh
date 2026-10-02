#!/bin/sh
# 现场自检：确认显示输出模式、解码方式、SoC 温度是否正常。
# 装机后、以及怀疑"画面错位 / 卡顿 / 自动关机"时在设备上运行。
#
#   /usr/local/lib/display-agent/check-display.sh
#
# 退出码：0 全部正常；1 有问题（每一项都会打印怎么修）。
set -u
SOCK="${MPV_SOCKET:-/run/display-agent/mpv.sock}"
WANT_W="${WANT_W:-1440}"
WANT_H="${WANT_H:-900}"
bad=0

say()  { printf '%-14s %s\n' "$1" "$2"; }
fail() { printf '%-14s %s\n' "$1" "$2"; bad=1; }

# mpv 的 JSON IPC：发一条命令取一个属性。socat 不一定装了，所以用 nc -U 兜底。
mpv_get() {
	req="{\"command\":[\"get_property\",\"$1\"]}"
	if command -v socat >/dev/null 2>&1; then
		printf '%s\n' "$req" | socat -t1 - "UNIX-CONNECT:$SOCK" 2>/dev/null
	elif command -v nc >/dev/null 2>&1; then
		printf '%s\n' "$req" | timeout 2 nc -U "$SOCK" 2>/dev/null
	elif command -v python3 >/dev/null 2>&1; then
		python3 -c 'import socket,sys
s=socket.socket(socket.AF_UNIX); s.settimeout(2); s.connect(sys.argv[1]); s.sendall(sys.argv[2].encode()+b"\n")
print(s.makefile().readline())' "$SOCK" "$req" 2>/dev/null
	fi
}
# 从 JSON 里抠一个字段（不引 jq）
jget() { sed -n "s/.*\"$1\":\([^,}]*\).*/\1/p" | head -1 | tr -d '" '; }

echo "=== 1. 内核实际输出模式 ==="
MODES=$(cat /sys/class/drm/card*-HDMI-A-1/modes 2>/dev/null | head -3)
CUR=$(echo "$MODES" | head -1)
if [ -z "$CUR" ]; then
	fail "HDMI" "读不到 /sys/class/drm/card*-HDMI-A-1/modes（HDMI 没接？驱动没加载？）"
else
	say "可用模式" "$(echo "$MODES" | tr '\n' ' ')"
	if [ "$CUR" = "${WANT_W}x${WANT_H}" ]; then
		say "首选模式" "$CUR ✓"
	else
		fail "首选模式" "$CUR（期望 ${WANT_W}x${WANT_H}）"
		echo "               面板 EDID 优先级高于默认设置，必须在内核命令行显式指定："
		echo "               编辑 /boot/armbianEnv.txt 的 extraargs，确保含"
		echo "                 video=HDMI-A-1:${WANT_W}x${WANT_H}@60"
		echo "               EDID 里没有这个模式时内核会忽略它，改成强制："
		echo "                 video=HDMI-A-1:${WANT_W}x${WANT_H}@60,e"
		echo "               改完必须 reboot。"
	fi
fi

echo
echo "=== 2. mpv 实际输出分辨率与解码方式 ==="
if ! command -v socat >/dev/null 2>&1 && ! command -v nc >/dev/null 2>&1 && ! command -v python3 >/dev/null 2>&1; then
	echo "               （需要 socat、nc 或 python3 之一才能查询 mpv；都没有时这一项跳过，"
	echo "                 也可以在管理后台设备列表里看解码方式与输出分辨率——设备随心跳上报）"
fi
if [ ! -S "$SOCK" ]; then
	fail "mpv" "$SOCK 不存在（display-agent 没在跑？systemctl status display-agent）"
else
	OSD=$(mpv_get osd-dimensions)
	OW=$(echo "$OSD" | jget w); OH=$(echo "$OSD" | jget h)
	HW=$(mpv_get hwdec-current | jget data)
	VID=$(mpv_get video-codec | jget data)
	if [ -n "${OW:-}" ] && [ "$OW" != "0" ]; then
		if [ "$OW" = "$WANT_W" ] && [ "$OH" = "$WANT_H" ]; then
			say "输出分辨率" "${OW}x${OH} ✓"
		else
			# 这不致命：叠加图会按实际分辨率重做。但说明第 1 项没配对。
			say "输出分辨率" "${OW}x${OH}（与模板画布 ${WANT_W}x${WANT_H} 不一致，叠加图会被缩放；按第 1 项修）"
		fi
	fi
	case "${HW:-}" in
	""|"null")
		say "解码方式" "问不到（当前可能没在放视频）" ;;
	"no")
		# Armbian/Debian 自带的 mpv/FFmpeg 驱动不了 H3 的硬件解码器（见下），软解是常态，不算故障。
		# 服务端转码已按软解可承受的规格出片；真正要盯的是第 3 项的温度。
		say "解码方式" "软解（Armbian 自带 mpv 的正常情况）" ;;
	*)
		say "解码方式" "硬解 $HW ✓ （视频编码 ${VID:-?}）" ;;
	esac
fi

# 硬件解码器（cedrus）是"无状态"解码器，播放器要通过 V4L2 Request API 驱动它；
# FFmpeg 上游至今没有合入这部分支持，所以自带的 mpv 一律软解。这里只报告现状，供排查参考。
if ls /dev/video* >/dev/null 2>&1 && grep -qs cedrus /sys/class/video4linux/*/name; then
	say "硬件解码器" "cedrus 已加载"
else
	say "硬件解码器" "未发现 cedrus（内核没带该驱动）"
fi
if command -v ffmpeg >/dev/null 2>&1; then
	# 只认 v4l2request：官方版本的 -hwaccels 也会列出 drm（那只是设备类型，驱动不了 cedrus）
	if ffmpeg -hide_banner -hwaccels 2>/dev/null | grep -q v4l2request; then
		say "FFmpeg" "支持 v4l2request（可尝试 mpv --hwdec=v4l2request-copy）"
	else
		say "FFmpeg" "官方版本，不支持 v4l2request（无法驱动 cedrus，只能软解）"
	fi
fi

echo
echo "=== 3. SoC 温度与降频 ==="
T=$(cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null)
if [ -n "${T:-}" ]; then
	[ "$T" -gt 1000 ] && T=$((T / 1000))
	if [ "$T" -ge 80 ]; then
		fail "温度" "${T}°C —— 偏高"
		echo "               H3 到 85°C 开始降频、更高会关机。确认：散热片装了没、"
		echo "               通风是否良好；视频是否都经服务端转码（上传时统一压成软解吃得消的规格）。"
	else
		say "温度" "${T}°C ✓"
	fi
fi
FREQ=$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq 2>/dev/null)
[ -n "${FREQ:-}" ] && say "CPU 频率" "$((FREQ / 1000)) MHz"

echo
[ "$bad" -eq 0 ] && echo "全部正常。" || echo "有问题，见上面每项的处理办法。"
exit "$bad"
