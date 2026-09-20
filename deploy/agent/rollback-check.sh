#!/bin/sh
# display-agent 更新回滚检查（systemd ExecStartPre）。
#
# OTA 切换 current 符号链接后会写 pending-verify；新版本首个心跳成功即删除该文件。
# 若新版本反复启动失败（累计 3 次仍未确认），把 current 指回 previous，自动回滚。
# 用系统 sh 执行，不依赖新二进制本身可运行。
set -u
DIR="${1:-/usr/local/lib/display-agent}"
PV="$DIR/pending-verify"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-3}"

[ -f "$PV" ] || exit 0

version=$(sed -n 's/^version=//p' "$PV" | head -n1)
attempts=$(sed -n 's/^attempts=//p' "$PV" | head -n1)
attempts=$(( ${attempts:-0} + 1 ))

if [ "$attempts" -ge "$MAX_ATTEMPTS" ]; then
  if [ -L "$DIR/previous" ]; then
    prev=$(readlink "$DIR/previous")
    ln -sfn "$prev" "$DIR/current.tmp" && mv -T "$DIR/current.tmp" "$DIR/current"
    echo "display-agent: version ${version:-?} failed $attempts starts, rolled back to $prev"
  else
    echo "display-agent: version ${version:-?} failed $attempts starts but no previous version to roll back to"
  fi
  rm -f "$PV"
  exit 0
fi

printf 'version=%s\nattempts=%s\n' "$version" "$attempts" > "$PV.tmp" && mv "$PV.tmp" "$PV"
echo "display-agent: verifying version ${version:-?} (start attempt $attempts/$MAX_ATTEMPTS)"
exit 0
