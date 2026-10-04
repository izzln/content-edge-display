#!/bin/sh
# 现场自检：确认显示输出模式、硬件解码、SoC 温度是否正常。
# 装机后、以及怀疑"画面错位 / 卡顿 / 自动关机"时在设备上运行。
#
#   /usr/local/lib/display-agent/check-display.sh
#
# 退出码：0 全部正常；1 有问题（每一项都会打印怎么修）。
set -u
STATUS="${STATUS_FILE:-/var/lib/display-agent/status.json}"
WANT_W="${WANT_W:-1440}"
WANT_H="${WANT_H:-900}"
bad=0

say()  { printf '%-14s %s\n' "$1" "$2"; }
fail() { printf '%-14s %s\n' "$1" "$2"; bad=1; }
# 从 JSON 里抠一个字段（不引 jq）
jget() { sed -n "s/.*\"$1\":\([^,}]*\).*/\1/p" | head -1 | tr -d '" '; }

echo "=== 1. 内核输出模式 ==="
MODES=$(cat /sys/class/drm/card*-HDMI-A-1/modes 2>/dev/null | head -3)
CUR=$(echo "$MODES" | head -1)
if [ -z "$CUR" ]; then
	fail "HDMI" "读不到 /sys/class/drm/card*-HDMI-A-1/modes（HDMI 没接？驱动没加载？）"
else
	say "可用模式" "$(echo "$MODES" | tr '\n' ' ')"
	if echo "$MODES" | grep -qx "${WANT_W}x${WANT_H}"; then
		say "目标模式" "${WANT_W}x${WANT_H} 可用 ✓"
	else
		fail "目标模式" "显示屏不提供 ${WANT_W}x${WANT_H}"
		echo "               面板 EDID 里没有这个模式，播放时会退回首选模式 $CUR（画面按比例缩放）。"
		echo "               编辑 /boot/armbianEnv.txt 的 extraargs，把 video= 参数改成强制："
		echo "                 video=HDMI-A-1:${WANT_W}x${WANT_H}@60,e"
		echo "               改完必须 reboot。"
	fi
fi

echo
echo "=== 2. 硬件解码条件 ==="
# H3 的硬件解码器 cedrus 是"无状态"解码器，GStreamer 的 v4l2codecs 插件直接驱动它（v4l2slh264dec）。
# 这个元素只在内核驱动正常加载时才会注册，所以它在不在，就说明硬解能不能用。
if grep -qs cedrus /sys/class/video4linux/*/name; then
	say "cedrus" "已加载 ✓"
else
	fail "cedrus" "没有找到 cedrus 视频解码设备"
	echo "               内核没带 sunxi-cedrus 驱动，或设备树里 video-codec 节点没启用："
	echo "               ls /dev/video* /dev/media*；dmesg | grep -i cedrus；lsmod | grep cedrus"
fi
if ! command -v gst-inspect-1.0 >/dev/null 2>&1; then
	fail "GStreamer" "没装 gstreamer1.0-tools，无法检查（重跑 install-agent.sh）"
elif gst-inspect-1.0 v4l2slh264dec >/dev/null 2>&1; then
	say "硬解元素" "v4l2slh264dec ✓"
else
	fail "硬解元素" "v4l2slh264dec 不可用——视频会退化成软解，发热、卡顿"
	echo "               确认 cedrus 已加载（上一项），并已安装 gstreamer1.0-plugins-bad："
	echo "               apt install gstreamer1.0-plugins-bad；rm -rf ~/.cache/gstreamer-1.0 后重试"
fi
if command -v gst-inspect-1.0 >/dev/null 2>&1 && ! gst-inspect-1.0 kmssink >/dev/null 2>&1; then
	fail "kmssink" "不可用（apt install gstreamer1.0-plugins-bad）"
fi
# cedrus 的解码缓冲（1440×900 一帧约 2MB，要二十来帧）、模板叠加层的两块帧缓冲（各约 5MB）、控制台帧缓冲
# 都从 CMA（连续物理内存）里分；实测 128MB 在播放时只剩约 5MB，不够时视频直接放不出来。
CMA_T=$(awk '/^CmaTotal:/ {print int($2 / 1024)}' /proc/meminfo)
CMA_F=$(awk '/^CmaFree:/ {print int($2 / 1024)}' /proc/meminfo)
if [ -z "$CMA_T" ]; then
	say "CMA" "读不到（/proc/meminfo 里没有 CmaTotal）"
elif [ "$CMA_T" -lt 160 ]; then
	fail "CMA" "共 ${CMA_T}MB、空闲 ${CMA_F}MB —— 偏小，播放视频时余量只剩几 MB，换片时可能分不到缓冲"
	echo "               编辑 /boot/armbianEnv.txt，在 extraargs 里加上 cma=192M"
	echo "               （已有 extraargs 就用空格追加在后面），然后 reboot。"
	echo "               CMA 空闲时仍可被普通内存借用，调大不浪费内存。"
else
	say "CMA" "共 ${CMA_T}MB，空闲 ${CMA_F}MB ✓"
fi
if ! python3 -c 'import gi; gi.require_version("Gst", "1.0"); from gi.repository import Gst' 2>/dev/null; then
	fail "Python" "缺少 GStreamer 的 Python 绑定（apt install python3-gst-1.0 gir1.2-gst-plugins-base-1.0）"
fi

echo
echo "=== 3. 实际播放状态 ==="
if [ ! -s "$STATUS" ]; then
	say "播放状态" "还没有（$STATUS 不存在：display-agent 没在跑，或还没发过第一个心跳）"
else
	HW=$(jget hwdec < "$STATUS")
	OW=$(jget output_w < "$STATUS"); OH=$(jget output_h < "$STATUS")
	if [ -n "${OW:-}" ] && [ "$OW" != "0" ]; then
		if [ "$OW" = "$WANT_W" ] && [ "$OH" = "$WANT_H" ]; then
			say "输出分辨率" "${OW}x${OH} ✓"
		else
			say "输出分辨率" "${OW}x${OH}（与模板画布 ${WANT_W}x${WANT_H} 不一致，画面按比例缩放；按第 1 项修）"
		fi
	fi
	case "${HW:-}" in
	"") say "解码方式" "还没放过视频" ;;
	"no") fail "解码方式" "软解——硬解没起来，按第 2 项排查" ;;
	*) say "解码方式" "硬解 $HW ✓" ;;
	esac
fi

echo
echo "=== 4. SoC 温度与降频 ==="
T=$(cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null)
if [ -n "${T:-}" ]; then
	[ "$T" -gt 1000 ] && T=$((T / 1000))
	if [ "$T" -ge 80 ]; then
		fail "温度" "${T}°C —— 偏高"
		echo "               H3 到 85°C 开始降频、更高会关机。确认：散热片装了没、通风是否良好、"
		echo "               第 3 项是不是在软解。"
	else
		say "温度" "${T}°C ✓"
	fi
fi
FREQ=$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq 2>/dev/null)
[ -n "${FREQ:-}" ] && say "CPU 频率" "$((FREQ / 1000)) MHz"

echo
[ "$bad" -eq 0 ] && echo "全部正常。" || echo "有问题，见上面每项的处理办法。"
exit "$bad"
