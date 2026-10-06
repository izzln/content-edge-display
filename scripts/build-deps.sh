#!/bin/sh
# 打设备端离线依赖包：deploy/agent/deps.txt 里的软件包连同全部下层依赖（armhf），外加 apt 索引。
# 在后台「管理」页上传一次，之后装机与 OTA 新增的依赖都从服务端局域网安装，不访问外网、不跑境外源的 apt-get update。
#
#   scripts/build-deps.sh [代号]      # 默认 trixie；须与设备 Armbian 的 VERSION_CODENAME 一致（/etc/os-release），
#                                     # Debian 系如 trixie，Ubuntu 系如 noble
#   → bin/display-deps-<代号>-armhf.tar.gz
#
# 需要 docker，且能运行 arm 容器（qemu binfmt：apt install qemu-user-static，或
# docker run --privileged --rm tonistiigi/binfmt --install arm）。
set -eu
CODENAME="${1:-trixie}"
ROOT=$(cd "$(dirname "$0")/.." && pwd)
NAME="display-deps-$CODENAME"
STAGE="$ROOT/bin/stage-deps"
case "$CODENAME" in
jammy | noble | plucky | questing) IMAGE="ubuntu:$CODENAME" ;;
*) IMAGE="debian:$CODENAME" ;;
esac
rm -rf "$STAGE" && mkdir -p "$STAGE/$NAME"

# 用空的 dpkg 状态文件让 apt 当作什么都没装：下载的是完整依赖闭包（含 libc 等基础库）。
# 设备上的 Armbian 镜像可能比这里旧，缺的、需要更新的基础库都能从这个仓库补齐，只用局域网源也装得上。
docker run --rm --platform linux/arm/v7 -e CODENAME="$CODENAME" \
	-v "$STAGE/$NAME:/out" -v "$ROOT/deploy/agent/deps.txt:/deps.txt:ro" "$IMAGE" sh -euc '
	apt-get update -qq
	apt-get install -qq -y --no-install-recommends apt-utils >/dev/null
	: > /tmp/status
	mkdir -p /out/partial
	apt-get install -qq -y --download-only --no-install-recommends \
		-o Dir::State::status=/tmp/status -o Dir::Cache::archives=/out $(grep -v "^#" /deps.txt)
	rm -rf /out/partial /out/lock
	cd /out
	apt-ftparchive packages . > Packages
	apt-ftparchive release . > Release
	echo "$CODENAME" > CODENAME
	chown -R "$(stat -c %u:%g /deps.txt)" /out
'
tar -czf "$ROOT/bin/$NAME-armhf.tar.gz" -C "$STAGE" "$NAME"
rm -rf "$STAGE"
echo "→ bin/$NAME-armhf.tar.gz（$(du -h "$ROOT/bin/$NAME-armhf.tar.gz" | cut -f1)，后台「管理」页上传一次即可）"
