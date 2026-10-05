# 安装 deps.txt 里的播放依赖。update.sh 每次执行都调用（首次安装与每次 OTA），所以新版本需要的新软件包随 OTA 装上。
#
#   . "$HERE/deps.sh"; install_deps <服务端 HTTPS 地址> <服务端证书>
#
# 1. 服务端有与本机 Debian 版本代号（/etc/os-release 的 VERSION_CODENAME）一致的离线依赖包（make deps，后台上传）时，
#    只从它安装/升级：经 HTTPS、只信任服务端证书（与代理固定的是同一张），局域网下载，不访问外网；
# 2. 否则还有包没装上时，在线 apt（设备能上外网时才会成功）；
# 3. 最后核对 deps.txt 全部已安装，缺了就失败——OTA 不会切到缺依赖的新版本，原因随心跳显示在后台。
# 源列表与索引放在临时目录，不动系统的 apt 配置。

install_deps() {
	local pkgs
	pkgs=$(grep -v '^#' "$HERE/deps.txt")
	export DEBIAN_FRONTEND=noninteractive
	# 上次被打断（断电、超时）时先收尾，否则 apt 拒绝工作
	dpkg --configure -a --force-confold || true
	if ! deps_from_server "$1" "$2" && [ -n "$(deps_missing)" ]; then
		echo "   在线安装依赖"
		apt-get -o Acquire::http::Timeout=20 -o Acquire::https::Timeout=20 update || true
		apt-get install $DEPS_APT_OPTS $pkgs || true
	fi
	apt-get clean # 下载的 .deb 不留在 SD 卡上
	local missing
	missing=$(deps_missing)
	if [ -n "$missing" ]; then
		echo "缺少依赖 ${missing% }：请在后台上传本机 Debian 版本（$(deps_codename)）的离线依赖包" >&2
		return 1
	fi
}

DEPS_APT_OPTS="-y -q --no-install-recommends -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold"

deps_codename() { (. /etc/os-release && echo "${VERSION_CODENAME:-}"); }

deps_missing() {
	local p
	for p in $(grep -v '^#' "$HERE/deps.txt"); do
		dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'ok installed' || printf '%s ' "$p"
	done
}

deps_from_server() {
	local url="$1" ca="$2" codename t ok=1
	codename=$(deps_codename)
	[ -n "$url" ] && [ -s "$ca" ] && [ -n "$codename" ] || return 1
	t=$(mktemp -d) && chmod 755 "$t" && mkdir -p "$t/lists/partial" "$t/parts" || return 1
	echo "deb [trusted=yes] $url/apt/$codename ./" > "$t/sources.list"
	# 服务端证书是自签的、不含设备访问它用的 IP：把它当作唯一可信的证书，不核对主机名
	set -- -o Dir::Etc::SourceList="$t/sources.list" -o Dir::Etc::SourceParts="$t/parts" -o Dir::State::Lists="$t/lists" \
		-o Acquire::https::CaInfo="$ca" -o Acquire::https::Verify-Host=false -o Acquire::https::Timeout=30
	if apt-get "$@" update --error-on=any > "$t/log" 2>&1; then
		echo "   从服务端离线依赖仓库安装（$codename）"
		apt-get "$@" install $DEPS_APT_OPTS $(grep -v '^#' "$HERE/deps.txt") && ok=0
	else
		echo "   服务端没有 $codename 的离线依赖包或连不上（$(grep -m1 '^[EW]:' "$t/log")）"
	fi
	rm -rf "$t"
	return $ok
}
