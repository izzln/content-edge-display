#!/bin/sh
# 在 Orange Pi One (Armbian) 上安装 display-agent（OTA 布局）。
#
# 用法（在设备上以 root 运行，同目录需有 display-agent-armv7、display-agent.service、rollback-check.sh）:
#   SERVER_URL=http://192.168.1.10:8080 ENROLL_TOKEN=xxxx ./install-agent.sh
#
# 安装布局:
#   /usr/local/lib/display-agent/versions/display-agent-<ver>
#   /usr/local/lib/display-agent/current -> versions/...   (systemd ExecStart)
#   /usr/local/lib/display-agent/rollback-check.sh          (ExecStartPre)
#   /etc/display-agent/agent.json
set -eu
: "${SERVER_URL:?需要 SERVER_URL，如 http://192.168.1.10:8080}"
: "${ENROLL_TOKEN:?需要 ENROLL_TOKEN（与服务端 server.json 的 enroll_token 一致）}"
BIN="${BIN:-./display-agent-armv7}"
INSTALL_DIR=/usr/local/lib/display-agent
HERE=$(cd "$(dirname "$0")" && pwd)

[ -f "$BIN" ] || { echo "找不到二进制 $BIN"; exit 1; }
VERSION=$("$BIN" -version 2>/dev/null || echo dev)

echo "== 安装 display-agent $VERSION 到 $INSTALL_DIR"
mkdir -p "$INSTALL_DIR/versions" /etc/display-agent /var/lib/display-agent
install -m 0755 "$BIN" "$INSTALL_DIR/versions/display-agent-$VERSION"
ln -sfn "$INSTALL_DIR/versions/display-agent-$VERSION" "$INSTALL_DIR/current"
install -m 0755 "$HERE/rollback-check.sh" "$INSTALL_DIR/rollback-check.sh"
rm -f "$INSTALL_DIR/pending-verify"

if [ ! -f /etc/display-agent/agent.json ]; then
cat > /etc/display-agent/agent.json <<EOF
{
  "server_url": "$SERVER_URL",
  "enroll_token": "$ENROLL_TOKEN",
  "cache_dir": "/var/lib/display-agent",
  "install_dir": "$INSTALL_DIR",
  "poll_interval_s": 10,
  "heartbeat_interval_s": 60,
  "player": "mpv",
  "image_duration_s": 10,
  "mpv_socket": "/run/display-agent/mpv.sock",
  "mpv_extra_args": []
}
EOF
chmod 0600 /etc/display-agent/agent.json
echo "== 已写入 /etc/display-agent/agent.json"
else
echo "== 保留已有 /etc/display-agent/agent.json"
fi

echo "== 安装 mpv 与 systemd 单元"
command -v mpv >/dev/null 2>&1 || { apt-get update && apt-get install -y mpv; }
install -m 0644 "$HERE/display-agent.service" /etc/systemd/system/display-agent.service
systemctl daemon-reload
systemctl enable display-agent

echo "== 固定 HDMI 输出 1440x900、禁用息屏"
ENV=/boot/armbianEnv.txt
if [ -f "$ENV" ] && ! grep -q 'video=HDMI-A-1:1440x900@60' "$ENV"; then
  if grep -q '^extraargs=' "$ENV"; then
    sed -i 's/^extraargs=\(.*\)$/extraargs=\1 video=HDMI-A-1:1440x900@60 consoleblank=0/' "$ENV"
  else
    echo 'extraargs=video=HDMI-A-1:1440x900@60 consoleblank=0' >> "$ENV"
  fi
fi

echo "== 完成。启动: systemctl start display-agent ；日志: journalctl -u display-agent -f"
