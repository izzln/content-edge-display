#!/bin/sh
# 现场自检：确认显示输出模式、硬件解码、SoC 温度是否正常。
# 装机后、以及怀疑"画面错位 / 卡顿 / 自动关机"时在设备上运行：/usr/local/lib/display-agent/check-display.sh
#
# 退出码：0 全部正常；1 有问题（每一项都会打印怎么修）。
set -u
STATUS=/var/lib/display-agent/status.env # 代理每个心跳周期写一次（连不上服务端时也写）
bad=0

say()  { printf '%-14s %s\n' "$1" "$2"; }
fail() { printf '%-14s %s\n' "$1" "$2"; bad=1; }

# 播放状态：HWDEC、OUTPUT（实际输出 WxH）、DISPLAY_MODE（agent.json 的 display_mode，装机时写入）
HWDEC="" OUTPUT="" DISPLAY_MODE=""
[ -s "$STATUS" ] && . "$STATUS"
WANT="${DISPLAY_MODE:-1440x900}" # 没配就按默认模板画布
WANT_W="${WANT%x*}" WANT_H="${WANT#*x}"

echo "=== 1. 内核输出模式 ==="
# 全部逐行扫描模式（去重、保持 EDID 顺序，第一个是首选；隔行模式如 1920x1080i 播放进程不用）
MODES=$(for c in /sys/class/drm/card*-*; do [ "$(cat "$c/status" 2>/dev/null)" = connected ] && cat "$c/modes"; done | grep -x '[0-9]*x[0-9]*' | awk '!seen[$0]++')
CUR=$(echo "$MODES" | head -1)
if [ -z "$CUR" ]; then
	fail "显示接口" "没有已连接的显示屏（/sys/class/drm/card*-*/status 都不是 connected：HDMI 没接？驱动没加载？）"
else
	say "可用模式" "$(echo "$MODES" | head -6 | tr '\n' ' ')"
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
# 用播放进程同样的 Python 绑定查元素（一次查完）：查得到就说明播放进程也用得上
if ! ELEMS=$(python3 -c 'import gi, sys; gi.require_version("Gst", "1.0"); from gi.repository import Gst; Gst.init(None)
print(" ".join(e for e in sys.argv[1:] if Gst.ElementFactory.find(e)))' v4l2slh264dec kmssink 2>/dev/null); then
	fail "Python" "缺少 GStreamer 的 Python 绑定（apt install python3-gst-1.0 gir1.2-gst-plugins-base-1.0）"
else
	case " $ELEMS " in
	*" v4l2slh264dec "*) say "硬解元素" "v4l2slh264dec ✓" ;;
	*)
		fail "硬解元素" "v4l2slh264dec 不可用——视频会退化成软解，发热、卡顿"
		echo "               确认 cedrus 已加载（上一项），并已安装 gstreamer1.0-plugins-bad："
		echo "               apt install gstreamer1.0-plugins-bad；rm -rf ~/.cache/gstreamer-1.0 后重试"
		;;
	esac
	case " $ELEMS " in *" kmssink "*) ;; *) fail "kmssink" "不可用（apt install gstreamer1.0-plugins-bad）" ;; esac
fi
# cedrus 的解码缓冲（1440×900 一帧约 2MB，要二十来帧）、模板叠加层的两块帧缓冲（各约 5MB）、控制台帧缓冲
# 都从 CMA（连续物理内存）里分；实测 128MB 在播放时只剩约 5MB，不够时视频直接放不出来。
# install-agent.sh 按内存大小把 cma= 写进内核参数（1GB 板 256M，512MB 板 192M）：这里核对它生效了没有。
CMA_T=$(awk '/^CmaTotal:/ {print int($2 / 1024)}' /proc/meminfo)
CMA_F=$(awk '/^CmaFree:/ {print int($2 / 1024)}' /proc/meminfo)
CMA_SET=$(tr ' ' '\n' </proc/cmdline | sed -n 's/^cma=\([0-9]*\)M$/\1/p')
if [ -z "$CMA_T" ]; then
	say "CMA" "读不到（/proc/meminfo 里没有 CmaTotal）"
elif [ -z "$CMA_SET" ] || [ "$CMA_T" -lt "$CMA_SET" ]; then
	fail "CMA" "共 ${CMA_T}MB、空闲 ${CMA_F}MB —— 内核参数里${CMA_SET:+的 cma=${CMA_SET}M 没生效}${CMA_SET:-没有 cma=}，换片时可能分不到缓冲"
	echo "               编辑 /boot/armbianEnv.txt，在 extraargs 里加上 cma=256M（512MB 内存的板子 192M），然后 reboot。"
	echo "               CMA 空闲时仍可被普通内存借用，调大不浪费内存。"
else
	say "CMA" "共 ${CMA_T}MB，空闲 ${CMA_F}MB ✓"
fi
echo
echo "=== 3. 实际播放状态 ==="
if [ ! -s "$STATUS" ]; then
	say "播放状态" "还没有（$STATUS 不存在：display-agent 没在跑，或刚启动不到一个心跳周期）"
else
	case "$OUTPUT" in
	"" | 0x0) ;;
	"$WANT") say "输出分辨率" "$OUTPUT ✓" ;;
	*) say "输出分辨率" "$OUTPUT（与 display_mode ${WANT} 不一致，画面按比例缩放；按第 1 项修）" ;;
	esac
	case "$HWDEC" in
	"") say "解码方式" "还没放过视频" ;;
	"no") fail "解码方式" "软解——硬解没起来，按第 2 项排查" ;;
	*) say "解码方式" "硬解 $HWDEC ✓" ;;
	esac
fi

echo
echo "=== 4. SoC 温度与降频 ==="
T=$(cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null)
if [ -n "${T:-}" ]; then
	T=$((T / 1000)) # 内核给的是毫摄氏度
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
