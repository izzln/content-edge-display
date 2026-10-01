#!/bin/sh
# 现场自检：确认显示输出模式、硬件解码、SoC 温度是否正常。
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
	else
		printf '%s\n' "$req" | timeout 2 nc -U "$SOCK" 2>/dev/null
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
		say "硬件解码" "问不到（当前可能没在放视频）" ;;
	"no")
		fail "硬件解码" "no —— 正在软解"
		echo "               H3 软解 1440x900 带不动：会卡顿、发热，严重时过热关机。"
		echo "               检查：ls -l /dev/video* （应有 cedrus 的 v4l2 m2m 设备）"
		echo "                     mpv --hwdec=auto-safe --vo=gpu -v <文件> 2>&1 | grep -i hwdec"
		echo "               另外确认视频是 H.264：H.265 在 H3 上没有硬解。" ;;
	*)
		say "硬件解码" "$HW ✓ （视频编码 ${VID:-?}）" ;;
	esac
fi

echo
echo "=== 3. SoC 温度与降频 ==="
T=$(cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null)
if [ -n "${T:-}" ]; then
	[ "$T" -gt 1000 ] && T=$((T / 1000))
	if [ "$T" -ge 80 ]; then
		fail "温度" "${T}°C —— 偏高"
		echo "               H3 到 85°C 开始降频、更高会关机。确认：散热片装了没、"
		echo "               视频码率是不是过高（服务端上传时会转码压到 4Mbps 以内）、"
		echo "               以及第 2 项是不是在软解。"
	else
		say "温度" "${T}°C ✓"
	fi
fi
command -v vcgencmd >/dev/null 2>&1 || true
FREQ=$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq 2>/dev/null)
[ -n "${FREQ:-}" ] && say "CPU 频率" "$((FREQ / 1000)) MHz"

echo
[ "$bad" -eq 0 ] && echo "全部正常。" || echo "有问题，见上面每项的处理办法。"
exit "$bad"
