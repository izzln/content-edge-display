#!/bin/sh
# 生成一个 8 位口令并打印到标准输出。
#
# 只用字母与数字、不含符号：这些口令要写进 JSON、通过环境变量传给 shell 脚本、
# 还要在浏览器里手敲，带符号只会徒增转义与输入错误。
# 复杂度要求：必须同时包含大写字母、小写字母和数字。
#
# 8 位 [A-Za-z0-9] 约 47 位熵。这两个口令只用于局域网内的 HTTP 认证
# （没有离线爆破的可能），够用；若要更高强度，把下面的 LEN 调大即可。
set -eu
LEN="${LEN:-8}"

i=0
while [ "$i" -lt 200 ]; do
	i=$((i + 1))
	t=$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c "$LEN")
	[ "${#t}" -eq "$LEN" ] || continue
	case "$t" in *[a-z]*) ;; *) continue ;; esac
	case "$t" in *[A-Z]*) ;; *) continue ;; esac
	case "$t" in *[0-9]*) ;; *) continue ;; esac
	printf '%s\n' "$t"
	exit 0
done

echo "gen-token.sh: 连续 $i 次都没生成出满足复杂度的口令，请检查 /dev/urandom" >&2
exit 1
