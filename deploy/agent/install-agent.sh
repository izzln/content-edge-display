#!/bin/sh
# 在 Orange Pi One (Armbian) 上安装 display-agent（OTA 布局）。
#
# 用法（在设备上以 root 运行，同目录需有 display-agent-armv7、display-agent.service、rollback-check.sh）:
#   SERVER_URL=http://display.lan:9000 ./install-agent.sh
# 注册口令默认取同目录的 enroll-token（make package 时写入），也可用 ENROLL_TOKEN= 覆盖。
# 显示模式默认 1440x900@60，可用 HDMI_MODE= 改；EDID 里没有该模式时用 HDMI_FORCE=e 强制。
#
# 每次运行都会重写 /etc/display-agent/agent.json；安装后**不会** enable 服务，
# 需要人工确认画面无误后再 systemctl enable display-agent。
#
# 安装布局:
#   /usr/local/lib/display-agent/versions/display-agent-<ver>
#   /usr/local/lib/display-agent/current -> versions/...   (systemd ExecStart)
#   /usr/local/lib/display-agent/rollback-check.sh          (ExecStartPre)
#   /etc/display-agent/agent.json
set -eu
: "${SERVER_URL:?需要 SERVER_URL，如 http://display.lan:9000（建议用域名而非 IP：server_url 写死在设备上，OTA 改不了）}"
BIN="${BIN:-./display-agent-armv7}"
INSTALL_DIR=/usr/local/lib/display-agent
HERE=$(cd "$(dirname "$0")" && pwd)
# 显示屏模式。面板 EDID 报的往往是 1920x1080——必须在两处显式指定：
#   - 内核 video= 参数（armbianEnv.txt）：管控制台，也让该模式出现在连接器的可用模式里；
#   - agent.json 的 display_mode → mpv --drm-mode：mpv 默认用 EDID 首选模式，不看内核参数。
# 只设前者的结果就是"armbianEnv.txt 改了，播放时还是 1080p"。
HDMI_MODE="${HDMI_MODE:-1440x900@60}"
# 有些 HDMI 驱动板的 EDID 里根本没有 1440x900 这个模式，内核会忽略 video= 退回 EDID 首选模式。
# 这种情况下加 e（force）强制输出：HDMI_FORCE=e ./install-agent.sh
HDMI_FORCE="${HDMI_FORCE:+,$HDMI_FORCE}"

# 注册口令：优先用环境变量，否则取包内 enroll-token（make package 时写入）
if [ -z "${ENROLL_TOKEN:-}" ] && [ -f "$HERE/enroll-token" ]; then
	ENROLL_TOKEN=$(cat "$HERE/enroll-token")
fi
: "${ENROLL_TOKEN:?需要 ENROLL_TOKEN（与服务端 server.json 的 enroll_token 一致）}"
if [ "$ENROLL_TOKEN" = "change-me-too" ]; then
	echo "enroll-token 还是占位值（CI 构建的包不含真实口令）。" >&2
	echo "请用服务端 server.json 里的 enroll_token：ENROLL_TOKEN=xxxxxxxx $0" >&2
	exit 1
fi

[ -f "$BIN" ] || { echo "找不到二进制 $BIN"; exit 1; }
VERSION=$("$BIN" -version 2>/dev/null || echo dev)

echo "== 安装 display-agent $VERSION 到 $INSTALL_DIR"
mkdir -p "$INSTALL_DIR/versions" /etc/display-agent /var/lib/display-agent
install -m 0755 "$BIN" "$INSTALL_DIR/versions/display-agent-$VERSION"
ln -sfn "$INSTALL_DIR/versions/display-agent-$VERSION" "$INSTALL_DIR/current"
install -m 0755 "$HERE/rollback-check.sh" "$INSTALL_DIR/rollback-check.sh"
install -m 0755 "$HERE/check-display.sh" "$INSTALL_DIR/check-display.sh"
rm -f "$INSTALL_DIR/pending-verify"

# agent.json 每次安装都按当前参数重写：重装/改服务端地址时不用先手工删文件，
# 也避免"装完了还是连旧地址"这种查半天的问题。设备特有的状态（编号、密钥）在
# /var/lib/display-agent/identity.json 里，不受影响。
cat > /etc/display-agent/agent.json <<EOF
{
  "server_url": "$SERVER_URL",
  "enroll_token": "$ENROLL_TOKEN",
  "cache_dir": "/var/lib/display-agent",
  "install_dir": "$INSTALL_DIR",
  "poll_interval_s": 10,
  "heartbeat_interval_s": 60,
  "player": "mpv",
  "display_mode": "$HDMI_MODE",
  "mpv_socket": "/run/display-agent/mpv.sock",
  "mpv_extra_args": []
}
EOF
chmod 0600 /etc/display-agent/agent.json
echo "== 已写入 /etc/display-agent/agent.json（每次安装都会覆盖）"

echo "== 安装 mpv 与 systemd 单元"
command -v mpv >/dev/null 2>&1 || { apt-get update && apt-get install -y mpv; }
install -m 0644 "$HERE/display-agent.service" /etc/systemd/system/display-agent.service
systemctl daemon-reload
# 刻意不 enable：装完先人工确认一次（分辨率、硬解、画面），确认无误再
#   systemctl enable --now display-agent
# 做母镜像时也应保持未 enable，烧完卡再按需开启。

echo "== 固定 HDMI 输出 ${HDMI_MODE}、禁用息屏"
ENV=/boot/armbianEnv.txt
if [ -f "$ENV" ]; then
  # 先清掉我们以前写进去的 video=/consoleblank=，再写当前这份，避免反复安装越堆越多、
  # 或者改了分辨率却被旧参数盖住。
  sed -i -E 's/[[:space:]]*video=HDMI-A-1:[^[:space:]]*//g; s/[[:space:]]*consoleblank=0//g' "$ENV"
  sed -i -E '/^extraargs=[[:space:]]*$/d' "$ENV"
  if grep -q '^extraargs=' "$ENV"; then
    sed -i "s|^extraargs=\(.*\)$|extraargs=\1 video=HDMI-A-1:${HDMI_MODE}${HDMI_FORCE} consoleblank=0|" "$ENV"
  else
    echo "extraargs=video=HDMI-A-1:${HDMI_MODE}${HDMI_FORCE} consoleblank=0" >> "$ENV"
  fi
  echo "   $(grep '^extraargs=' "$ENV")"
  echo "   提示：这项要重启才生效。重启后用下面这条确认实际输出模式："
  echo "     cat /sys/class/drm/card*-HDMI-A-1/modes | head -1"
fi

echo
echo "== 完成。接下来："
echo "   1) reboot                                   # 让 HDMI 模式生效"
echo "   2) systemctl start display-agent            # 先手工起一次看效果"
echo "   3) /usr/local/lib/display-agent/check-display.sh   # 确认分辨率与硬解"
echo "   4) systemctl enable display-agent           # 确认无误后再设为开机自启"
echo "   日志: journalctl -u display-agent -f"
