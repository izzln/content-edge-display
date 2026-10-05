#!/bin/sh
# display-agent 更新回滚检查（systemd ExecStartPre，用系统 sh 执行，不依赖新程序本身能运行）。
#
# OTA 切换 current 后会写 pending-verify（内容是已尝试启动的次数）；新版本首个心跳成功即删除它。
# 新版本连续 3 次启动仍未确认，就把 current 指回 previous——整个版本目录（程序与配套脚本）一起回退，
# 并把失败的版本记进 failed-version：代理不再自动重试它，心跳把原因报给后台。
set -u
DIR=$(dirname "$0") # 安装根目录（update.sh 把本脚本装在那里）
PV="$DIR/pending-verify"
[ -f "$PV" ] || exit 0

version=$(basename "$(readlink "$DIR/current")")
attempts=$(cat "$PV")
case "$attempts" in '' | *[!0-9]*) attempts=0 ;; esac
attempts=$((attempts + 1))

if [ "$attempts" -lt 3 ]; then
	echo "$attempts" >"$PV"
	echo "display-agent: verifying version $version (start attempt $attempts/3)"
	exit 0
fi
rm -f "$PV"
if [ -L "$DIR/previous" ]; then
	prev=$(readlink "$DIR/previous")
	ln -sfn "$prev" "$DIR/current.tmp" && mv -T "$DIR/current.tmp" "$DIR/current"
	echo "$version" >"$DIR/failed-version"
	sync
	echo "display-agent: version $version failed $attempts starts, rolled back to $(basename "$prev")"
else
	echo "display-agent: version $version failed $attempts starts but there is no previous version to roll back to"
fi
