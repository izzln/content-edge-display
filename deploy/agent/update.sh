#!/bin/sh
# 本版本的安装步骤：首次安装（install-agent.sh）与 OTA（display-agent 解开新包后）都会执行。
#
#   sh update.sh <安装目录>      # 通常是 /usr/local/lib/display-agent
#
# 此时本脚本所在的目录就是 <安装目录>/versions/<版本>/（整包解开），current 还指着旧版本；
# 执行成功后由调用方把 current 切过来（失败则不切，旧版本照常运行）。约定：
#   - 幂等，可重复执行；
#   - 软件包只经 deps.sh 安装：deps.txt 里加上即可，优先从服务端的离线依赖包装，不依赖外网；
#   - 版本目录以外的改动要与上一个版本兼容——回滚只切回旧版本目录，不会撤销这里的改动。
# 将来需要随程序一起调整的系统设置（systemd 单元、启动参数、播放依赖的配置等）都写在这里。
set -eu
DIR="${1:?用法: update.sh <安装目录>}"
HERE=$(cd "$(dirname "$0")" && pwd)
CONF=/etc/display-agent

# 播放依赖：新版本 deps.txt 里新增的软件包在这里装上（装不上就失败，不切换版本）
. "$HERE/deps.sh"
SERVER_URL=$(sed -n 's/.*"server_url": *"\([^"]*\)".*/\1/p' "$CONF/agent.json")
install_deps "$SERVER_URL" "$CONF/server.crt"

# 回滚检查放在固定路径：systemd 的 ExecStartPre 指向它，新版本起不来时也要能跑
install -m 0755 "$HERE/rollback-check.sh" "$DIR/rollback-check.sh.new"
mv "$DIR/rollback-check.sh.new" "$DIR/rollback-check.sh"

# 现场自检脚本：固定路径指向当前版本里的那份
ln -sfn current/check-display.sh "$DIR/check-display.sh"

# systemd 单元：有变化才替换并 reload
UNIT=/etc/systemd/system/display-agent.service
if ! cmp -s "$HERE/display-agent.service" "$UNIT" 2>/dev/null; then
	install -m 0644 "$HERE/display-agent.service" "$UNIT"
	systemctl daemon-reload
	echo "updated $UNIT"
fi
echo "version $(cat "$HERE/VERSION") installed"
