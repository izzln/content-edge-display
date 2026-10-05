#!/bin/sh
# 本版本的安装步骤：首次安装（install-agent.sh）与 OTA（display-agent 解开新包后）都会执行。
#
#   SERVER_URL=https://<服务器>:9001 sh <安装目录>/versions/<版本>/update.sh
#
# 此时本脚本所在的目录就是 <安装目录>/versions/<版本>/（整包解开），current 还指着旧版本；
# 执行成功后由调用方把 current 切过来（失败则不切，旧版本照常运行）。约定：
#   - 幂等，可重复执行；
#   - 软件包只在这里装：deps.txt 里加上即可，优先从服务端的离线依赖包装，不依赖外网；
#   - 版本目录以外的改动要与上一个版本兼容——回滚只切回旧版本目录，不会撤销这里的改动。
# 将来需要随程序一起调整的系统设置（systemd 单元、启动参数、播放依赖的配置等）都写在这里。
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DIR=$(dirname "$(dirname "$HERE")") # 安装目录
CA=/etc/display-agent/server.crt    # 服务端证书（install.sh 写入，与代理固定的是同一张）
PKGS=$(grep -v '^#' "$HERE/deps.txt")
APT_OPTS="-y -q --no-install-recommends -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold"
export DEBIAN_FRONTEND=noninteractive

# missing 列出 deps.txt 里还没装好的软件包
missing() {
	installed=$(dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' $PKGS 2>/dev/null | awk '$1 == "ii" {print $2}')
	for p in $PKGS; do
		echo "$installed" | grep -qx "$p" || printf '%s ' "$p"
	done
}

# from_server 从服务端的离线依赖仓库（与本机 Debian 版本代号一致的那份）安装：经 HTTPS、只信任服务端证书，
# 局域网下载，不访问外网。源列表与索引放在临时目录，不动系统的 apt 配置。
from_server() {
	codename=$(. /etc/os-release && echo "${VERSION_CODENAME:-}")
	[ -n "${SERVER_URL:-}" ] && [ -s "$CA" ] && [ -n "$codename" ] || return 1
	t=$(mktemp -d) && chmod 755 "$t" && mkdir -p "$t/lists/partial" "$t/parts" || return 1
	echo "deb [trusted=yes] $SERVER_URL/apt/$codename ./" >"$t/sources.list"
	# 服务端证书是自签的、不含设备访问它用的 IP：把它当作唯一可信的证书，不核对主机名
	set -- -o Dir::Etc::SourceList="$t/sources.list" -o Dir::Etc::SourceParts="$t/parts" -o Dir::State::Lists="$t/lists" \
		-o Acquire::https::CaInfo="$CA" -o Acquire::https::Verify-Host=false -o Acquire::https::Timeout=30
	ok=1
	if apt-get "$@" update --error-on=any >"$t/log" 2>&1; then
		echo "   从服务端离线依赖仓库安装（$codename）"
		apt-get "$@" install $APT_OPTS $PKGS && ok=0
	else
		echo "   服务端没有 $codename 的离线依赖包或连不上（$(grep -m1 '^[EW]:' "$t/log")）"
	fi
	rm -rf "$t"
	return $ok
}

# 1. 播放依赖：新版本 deps.txt 里新增的软件包在这里装上。优先服务端的离线依赖包；还缺包时在线 apt
#    （设备能上外网时才会成功）；最后核对全部装好，缺了就失败——不切到缺依赖的新版本，原因随心跳显示在后台。
dpkg --configure -a --force-confold || true # 上次被打断（断电、超时）时先收尾，否则 apt 拒绝工作
if ! from_server && [ -n "$(missing)" ]; then
	echo "   在线安装依赖"
	apt-get -o Acquire::http::Timeout=20 -o Acquire::https::Timeout=20 update || true
	apt-get install $APT_OPTS $PKGS || true
fi
apt-get clean # 下载的 .deb 不留在 SD 卡上
left=$(missing)
if [ -n "$left" ]; then
	echo "缺少依赖 ${left% }：请在后台上传本机 Debian 版本（$(. /etc/os-release && echo "${VERSION_CODENAME:-?}")）的离线依赖包" >&2
	exit 1
fi

# 2. 设备不自行升级系统：内核、dtb、u-boot 升级可能弄坏显示与硬解，只随整包 OTA 有计划地变；
#    apt 的定时任务还会在 OTA 安装依赖时占着 dpkg 锁
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
dpkg-query -W -f='${db:Status-Abbrev}${Package}\n' 'linux-image-*' 'linux-dtb-*' 'linux-u-boot-*' 'armbian-firmware*' 2>/dev/null |
	sed -n 's/^ii *//p' | xargs -r apt-mark hold >/dev/null

# 3. 回滚检查放在固定路径：systemd 的 ExecStartPre 指向它，新版本起不来时也要能跑
install -m 0755 "$HERE/rollback-check.sh" "$DIR/rollback-check.sh.new"
mv "$DIR/rollback-check.sh.new" "$DIR/rollback-check.sh"

# 4. 现场自检脚本：固定路径指向当前版本里的那份
ln -sfn current/check-display.sh "$DIR/check-display.sh"

# 5. systemd 单元：有变化才替换并 reload
UNIT=/etc/systemd/system/display-agent.service
if ! cmp -s "$HERE/display-agent.service" "$UNIT" 2>/dev/null; then
	install -m 0644 "$HERE/display-agent.service" "$UNIT"
	systemctl daemon-reload
	echo "updated $UNIT"
fi
echo "version $("$HERE/display-agent" -version) installed"
