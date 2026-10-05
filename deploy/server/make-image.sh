#!/bin/sh
# 把公版 Armbian 镜像做成"插卡即装"的装机镜像：首次开机自动设好 root 密码、执行一键装机并注册，全程无人值守。
# 所有设备烧同一个镜像即可（设备编号取 SoC 序列号，各不相同）。在 Linux 上以 root 运行（要挂载镜像里的 ext4 分区）：
#
#   BOOTSTRAP=http://<服务器>:9000 ENROLL_TOKEN=<注册口令> ROOT_PASSWORD=<设备 root 密码> \
#     ./make-image.sh Armbian_<版本>_Orangepione_trixie_current_<内核>_minimal.img.xz
#   → 同目录下 <原名>-display.img，用 balenaEtcher 等烧到 TF 卡，插卡上电即可
#
# 可选：HDMI_MODE、HDMI_FORCE、CMA（同一键装机命令）。
# 镜像里带着注册口令和 root 密码的哈希：当作机密保管，别外传。装机成功后设备上的这两样会被删掉
# （注册口令仍在 agent.json 里，与手工装机相同）。
set -eu
SRC="${1:?用法见脚本开头}"
: "${BOOTSTRAP:?需要 BOOTSTRAP，如 http://display.lan:9000（后台「程序更新」页装机命令里的地址）}"
: "${ENROLL_TOKEN:?需要 ENROLL_TOKEN（server.json 的 enroll_token）}"
: "${ROOT_PASSWORD:?需要 ROOT_PASSWORD（设备 root 登录密码，现场救援控制台用）}"
[ "$(id -u)" = 0 ] || { echo "请以 root 运行（要挂载镜像）" >&2; exit 1; }

OUT="${SRC%.xz}"
OUT="${OUT%.img}-display.img"
echo "== 解出镜像 → $OUT"
case "$SRC" in
*.xz) xz -dc "$SRC" > "$OUT" ;;
*) cp "$SRC" "$OUT" ;;
esac

# 根分区：MBR 里装着 Armbian 的那个 Linux 分区（Orange Pi 的镜像只有一个 ext4 分区，/boot 也在里面）
MNT=$(mktemp -d)
trap 'umount "$MNT" 2>/dev/null; rmdir "$MNT"' EXIT
for i in 0 1 2 3; do
	type=$(od -An -tx1 -j $((446 + 16 * i + 4)) -N1 "$OUT" | tr -d ' ')
	start=$(od -An -tu4 -j $((446 + 16 * i + 8)) -N4 "$OUT" | tr -d ' ')
	[ "$type" = 83 ] || continue
	mount -o loop,offset=$((start * 512)) "$OUT" "$MNT"
	[ -f "$MNT/etc/armbian-release" ] && break
	umount "$MNT"
done
[ -f "$MNT/etc/armbian-release" ] || { echo "镜像里没找到 Armbian 根分区（不是 Armbian 镜像？）" >&2; exit 1; }

echo "== 设置 root 密码，跳过 Armbian 首次登录向导"
# 向导会在控制台自动登录 root、等人输入密码和用户名——无人值守时会一直卡在那里；
# 它的自动登录也要关掉，否则谁接上键盘都直接是 root。
hash=$(openssl passwd -6 "$ROOT_PASSWORD")
awk -F: -v OFS=: -v h="$hash" -v d=$(($(date +%s) / 86400)) '$1 == "root" { $2 = h; $3 = d } 1' \
	"$MNT/etc/shadow" > "$MNT/etc/shadow.new"
cat "$MNT/etc/shadow.new" > "$MNT/etc/shadow" # 覆盖内容，保留原文件的属主与权限
rm -f "$MNT/etc/shadow.new" "$MNT/root/.not_logged_in_yet"
rm -f "$MNT/etc/systemd/system/getty@.service.d/override.conf" "$MNT/etc/systemd/system/getty@tty1.service.d/override.conf" \
	"$MNT/etc/systemd/system/serial-getty@.service.d/override.conf" "$MNT/etc/systemd/system/serial-getty@ttyGS0.service.d/override.conf"

echo "== 写入首次开机装机服务"
umask 077
{
	echo "BOOTSTRAP='$BOOTSTRAP'"
	echo "ENROLL_TOKEN='$ENROLL_TOKEN'"
	for v in HDMI_MODE HDMI_FORCE CMA; do
		eval "val=\${$v:-}"
		[ -z "$val" ] || echo "$v='$val'"
	done
} > "$MNT/etc/display-firstboot.env"
umask 022

cat > "$MNT/usr/local/sbin/display-firstboot" <<'SCRIPT'
#!/bin/sh
# 由 make-image.sh 写入：首次开机执行一键装机（失败隔 30 秒重试，原因显示在屏幕上），成功后删掉自己并重启。
set -a
. /etc/display-firstboot.env
set +a
export NO_REBOOT=1
say() { echo "== display-firstboot: $*" | tee -a /dev/tty1; }
while :; do
	say "开始装机（$BOOTSTRAP）"
	# 输出同时写到 HDMI 屏幕（tty1）与日志；管道会吞掉退出码，所以另存
	{ curl -fsS --connect-timeout 10 -o /run/display-install.sh "$BOOTSTRAP/install.sh" && sh /run/display-install.sh; echo $? > /run/display-install.rc; } 2>&1 | tee -a /dev/tty1
	[ "$(cat /run/display-install.rc)" = 0 ] && break
	say "装机没有完成，30 秒后重试（网线、服务端地址、注册口令？详情：journalctl -u display-firstboot）"
	sleep 30
done
systemctl disable display-firstboot.service
rm -f /etc/display-firstboot.env /etc/systemd/system/display-firstboot.service /usr/local/sbin/display-firstboot
say "装机完成，重启"
reboot
SCRIPT
chmod 0755 "$MNT/usr/local/sbin/display-firstboot"

cat > "$MNT/etc/systemd/system/display-firstboot.service" <<'UNIT'
[Unit]
Description=Display agent first-boot install (make-image.sh)
Wants=network-online.target
After=network-online.target
ConditionPathExists=/etc/display-firstboot.env

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/display-firstboot
TimeoutStartSec=infinity

[Install]
WantedBy=multi-user.target
UNIT
mkdir -p "$MNT/etc/systemd/system/multi-user.target.wants"
ln -sf /etc/systemd/system/display-firstboot.service "$MNT/etc/systemd/system/multi-user.target.wants/display-firstboot.service"

sync
echo "== 完成：$OUT"
echo "   烧到 TF 卡、插网线上电即可：首次开机自动装机并注册（约 5~10 分钟，屏幕上有进度），装完自动重启进入播放。"
echo "   镜像里有注册口令与 root 密码哈希，请妥善保管。"
