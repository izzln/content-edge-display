#!/bin/sh
# 在 Orange Pi One（公版 Armbian）上首次安装 display-agent。
#
# 一般不直接运行它，而是用服务端生成的一键安装脚本（会下载程序包并带上下面的参数）：
#   curl -fsSL http://<服务器>:9000/install.sh | ENROLL_TOKEN=注册口令 sh
# 手工安装（解开程序包后在包目录里以 root 运行）：
#   SERVER_URL=https://<服务器>:9001 TLS_FINGERPRINT=<后台显示的指纹> ENROLL_TOKEN=注册口令 ./install-agent.sh
#
# 可选参数：
#   HDMI_MODE=1440x900@60（默认）；EDID 里没有该模式时加 HDMI_FORCE=e 强制输出
#   CMA=256M        连续内存，默认按内存大小定（1GB 板 256M，512MB 板 192M）
#   NO_REBOOT=1     装完不自动重启（显示参数要重启才生效）
#   SERVER_CERT=<文件>  服务端证书（一键安装脚本自动带上；手工安装时拷服务端的 data/tls/server.crt）。
#                   有了它，依赖从服务端的离线依赖包安装（首次安装与之后每次 OTA），没有时在线 apt
#
# 按 OTA 布局安装到 /usr/local/lib/display-agent（布局说明见 internal/agent/update.go），之后的程序更新都由 OTA 完成。
set -eu
: "${SERVER_URL:?需要 SERVER_URL，如 https://display.lan:9001（建议用域名而非 IP：server_url 写在设备上，换服务器只改解析）}"
: "${TLS_FINGERPRINT:?需要 TLS_FINGERPRINT（管理后台「程序更新」页显示的服务端证书指纹）}"
: "${ENROLL_TOKEN:?需要 ENROLL_TOKEN（与服务端 server.json 的 enroll_token 一致）}"
case "$SERVER_URL" in https://*) ;; *) echo "SERVER_URL 必须是 https:// 地址" >&2; exit 1 ;; esac
[ "$(id -u)" = 0 ] || { echo "请以 root 运行" >&2; exit 1; }

HERE=$(cd "$(dirname "$0")" && pwd)
INSTALL_DIR=/usr/local/lib/display-agent
# 版本号内置在程序里；顺带确认程序能在这块板子上运行
VERSION=$("$HERE/display-agent" -version) || { echo "display-agent 无法在本机运行（不是 ARMv7 的 Armbian？）" >&2; exit 1; }
# 显示屏模式。面板 EDID 报的往往是 1920x1080——必须在两处显式指定：
#   - 内核 video= 参数（armbianEnv.txt）：管控制台，也让该模式出现在连接器的可用模式里；
#   - agent.json 的 display_mode：播放进程据此设置显示模式，不指定就用 EDID 首选模式。
HDMI_MODE="${HDMI_MODE:-1440x900@60}"
HDMI_FORCE="${HDMI_FORCE:+,$HDMI_FORCE}"
# CMA（连续物理内存）：硬解缓冲、模板叠加层与控制台的帧缓冲都从这里分。Armbian 默认 128MB，
# 播放视频时只剩几 MB。CMA 空闲时普通内存照样能借用，调大不浪费。
MEM_MB=$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo)
if [ "${MEM_MB:-0}" -ge 768 ]; then CMA_DEFAULT=256M; else CMA_DEFAULT=192M; fi
CMA="${CMA:-$CMA_DEFAULT}"

# agent.json 每次安装都按当前参数重写：重装/改服务端地址时不用先手工删文件。
# 设备特有的状态（编号、密钥）在 /var/lib/display-agent/identity.json 里，不受影响。
# 先放证书：update.sh 从服务端的离线依赖包安装依赖时要用到它（服务端地址经 SERVER_URL 传过去）。
mkdir -p "$INSTALL_DIR/versions" /etc/display-agent
[ -n "${SERVER_CERT:-}" ] && install -m 0644 "$SERVER_CERT" /etc/display-agent/server.crt
cat > /etc/display-agent/agent.json <<JSON
{
  "server_url": "$SERVER_URL",
  "tls_fingerprint": "$TLS_FINGERPRINT",
  "enroll_token": "$ENROLL_TOKEN",
  "display_mode": "${HDMI_MODE%@*}"
}
JSON
chmod 0600 /etc/display-agent/agent.json
echo "   已写入 /etc/display-agent/agent.json"

echo "== 安装 display-agent $VERSION 到 $INSTALL_DIR（含 GStreamer 等依赖）"
rm -rf "$INSTALL_DIR/versions/$VERSION"
cp -a "$HERE" "$INSTALL_DIR/versions/$VERSION"
SERVER_URL="$SERVER_URL" sh "$INSTALL_DIR/versions/$VERSION/update.sh"
ln -sfn "$INSTALL_DIR/versions/$VERSION" "$INSTALL_DIR/current"
rm -f "$INSTALL_DIR/pending-verify" "$INSTALL_DIR/previous"

echo "== 固定 HDMI 输出 ${HDMI_MODE}、禁用息屏、CMA ${CMA}"
ENV=/boot/armbianEnv.txt
if [ -f "$ENV" ]; then
	# 先清掉以前写进去的 video=/consoleblank=/cma=，再写当前这份，避免反复安装越堆越多、
	# 或者改了参数却被旧值盖住。
	sed -i -E 's/[[:space:]]*video=HDMI-A-1:[^[:space:]]*//g; s/[[:space:]]*consoleblank=0//g; s/[[:space:]]*cma=[^[:space:]]*//g' "$ENV"
	sed -i -E 's/^extraargs=[[:space:]]+/extraargs=/; /^extraargs=$/d' "$ENV"
	if grep -q '^extraargs=' "$ENV"; then
		sed -i "s|^extraargs=\(.*\)$|extraargs=\1 video=HDMI-A-1:${HDMI_MODE}${HDMI_FORCE} consoleblank=0 cma=${CMA}|" "$ENV"
	else
		echo "extraargs=video=HDMI-A-1:${HDMI_MODE}${HDMI_FORCE} consoleblank=0 cma=${CMA}" >> "$ENV"
	fi
	echo "   $(grep '^extraargs=' "$ENV")"
fi

echo "== 开机自启"
systemctl enable display-agent

echo
echo "== 完成。重启后自动运行；现场排查："
echo "   $INSTALL_DIR/check-display.sh     # 分辨率、硬解、CMA、温度"
echo "   journalctl -u display-agent -f"
echo "   屏幕被播放画面占着时，插上 USB 键盘按任意键即可看到设备信息（含 IP）并登录控制台。"
if [ "${NO_REBOOT:-}" = 1 ]; then
	echo "   （NO_REBOOT=1：请手工 reboot 让显示参数生效）"
else
	echo "   10 秒后重启……"
	sleep 10
	reboot
fi
