#!/bin/sh
# 本版本的安装步骤：首次安装（install-agent.sh）与 OTA（display-agent 解开新包后）都会执行。
#
#   sh update.sh <安装目录>      # 通常是 /usr/local/lib/display-agent
#
# 此时本脚本所在的目录就是 <安装目录>/versions/<版本>/（整包解开），current 还指着旧版本；
# 执行成功后由调用方把 current 切过来。约定：
#   - 幂等，可重复执行；
#   - 离线可运行：不调用 apt，不访问外网（设备通常在没有外网的局域网里）；
#   - 版本目录以外的改动要与上一个版本兼容——回滚只切回旧版本目录，不会撤销这里的改动。
# 将来需要随程序一起调整的系统设置（systemd 单元、启动参数、播放依赖的配置等）都写在这里。
set -eu
DIR="${1:?用法: update.sh <安装目录>}"
HERE=$(cd "$(dirname "$0")" && pwd)

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
