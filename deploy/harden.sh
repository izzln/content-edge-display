#!/bin/sh
# 设备端安全加固（母镜像制作时以 root 运行一次）。
#
# 用法:
#   SSH_ALLOW_FROM=192.168.1.10 SSH_PUBKEY="ssh-ed25519 AAAA... ops" ./harden.sh
#   SSH_ALLOW_FROM 可为单个 IP 或网段（如 192.168.1.0/24），多个用逗号分隔。
#
# 做的事:
#   1. 禁用系统自动更新（apt 定时器 / unattended-upgrades），并锁定内核与 dtb 包版本
#   2. nftables 入站默认 DROP：只放行 lo、已建立连接、ICMP、来自运维地址的 SSH
#   3. sshd 仅密钥登录、禁止口令、仅 root 用密钥（无其他用户）
#   4. 禁用无关服务（avahi / bluetooth / ModemManager）
#
# SSH 结论：保留，但只对运维地址开放且仅密钥登录——设备装好后很难再物理接触，
# OTA 万一出问题，SSH 是唯一的远程救援通道；本机本就在局域网内，攻击面可接受。
set -eu
: "${SSH_ALLOW_FROM:?需要 SSH_ALLOW_FROM（允许 SSH 的运维 IP/网段）}"
: "${SSH_PUBKEY:?需要 SSH_PUBKEY（运维公钥）}"

echo "== 1. 禁用系统自动更新"
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
systemctl mask apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
apt-get remove -y unattended-upgrades 2>/dev/null || true
# 锁定内核/dtb/u-boot：升级它们可能破坏显示或硬解
dpkg -l | awk '/^ii +(linux-image|linux-dtb|linux-u-boot|armbian-firmware)/ {print $2}' | xargs -r apt-mark hold

echo "== 2. 防火墙（nftables）"
apt-get install -y nftables >/dev/null
ALLOW_SET=$(echo "$SSH_ALLOW_FROM" | sed 's/,/, /g')
cat > /etc/nftables.conf <<EOF
#!/usr/sbin/nft -f
flush ruleset
table inet filter {
  chain input {
    type filter hook input priority 0; policy drop;
    iif lo accept
    ct state established,related accept
    ct state invalid drop
    ip protocol icmp accept
    ip6 nexthdr icmpv6 accept
    ip saddr { $ALLOW_SET } tcp dport 22 accept
  }
  chain forward { type filter hook forward priority 0; policy drop; }
  chain output  { type filter hook output priority 0; policy accept; }
}
EOF
systemctl enable --now nftables
nft -f /etc/nftables.conf

echo "== 3. sshd 仅密钥"
mkdir -p /root/.ssh && chmod 700 /root/.ssh
grep -qxF "$SSH_PUBKEY" /root/.ssh/authorized_keys 2>/dev/null || echo "$SSH_PUBKEY" >> /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
cat > /etc/ssh/sshd_config.d/90-harden.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
PubkeyAuthentication yes
X11Forwarding no
AllowTcpForwarding no
MaxAuthTries 3
LoginGraceTime 20
EOF
sshd -t && systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true

echo "== 4. 禁用无关服务"
for svc in avahi-daemon bluetooth ModemManager cups; do
  systemctl disable --now "$svc" 2>/dev/null || true
done

echo "== 完成。请先用另一个终端验证密钥 SSH 可登录，再断开当前会话。"
