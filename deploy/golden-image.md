# 母镜像制作与批量部署（Orange Pi One）

目标：所有设备烧**同一个镜像**，首次上电自动获得唯一编号并向管理后台注册；之后只通过管理后台
更新代理程序（OTA），不再需要物理接触设备。

## 1. 分层：OS 母镜像 一次性，程序 OTA 反复

| 层 | 内容 | 更新方式 |
|---|---|---|
| OS 母镜像 | Armbian + mpv + 加固 + 代理安装布局 + 含 enroll_token 的配置 | 烧录时一次性写入，之后不动 |
| 代理程序 | `display-agent` 单一静态二进制 | 管理后台"固件"页上传 → 立即/定时下发 → 设备自动切换、失败自动回滚 |

## 2. 设备编号规则

代理首次启动按以下优先级确定编号并持久化到 `/var/lib/display-agent/identity.json`：

1. `agent.json` 里显式的 `device_id`（手工配置场景）；
2. **主机名**——烧录时在 Armbian Imager 的"自定义设置"里填写 hostname（如 `scr-0017`）即为设备编号；
3. 主机名是默认值（`orangepione` 等）时，取 H3 SoC 序列号后 8 位 → `opi-1a2b3c4d`（退回 eth0 MAC）。

> Armbian Imager 的自定义对官方镜像有效；对克隆的母镜像是否生效需实机验证一次。
> 不生效也没关系：规则 3 保证编号唯一，运营方在管理后台把名称改成"3 楼大堂"即可。

密钥在首启随机生成，只存在设备与服务端两处；`enroll_token` 只用于首次注册。

## 3. 制作母镜像（一台样机）

```sh
# 0. 开发机：交叉编译代理
make agent-arm                     # bin/display-agent-armv7，版本号取 git describe

# 1. 样机烧官方 Armbian（Bookworm minimal），完成首次初始化并联网；把以下文件 scp 到样机 /root/deploy/:
#    bin/display-agent-armv7  deploy/install-agent.sh  deploy/display-agent.service
#    deploy/rollback-check.sh deploy/harden.sh

# 2. 样机上（root）
cd /root/deploy
SSH_ALLOW_FROM=192.168.1.10 SSH_PUBKEY="ssh-ed25519 AAAA... ops" ./harden.sh
SERVER_URL=http://192.168.1.10:8080 ENROLL_TOKEN=<server.json 里的 enroll_token> ./install-agent.sh
systemctl start display-agent && journalctl -u display-agent -n 20   # 应看到注册成功、在线

# 3. 验证无误后清理，让镜像"回到出厂"
systemctl stop display-agent
rm -f /var/lib/display-agent/identity.json /var/lib/display-agent/current.json
rm -rf /var/lib/display-agent/media/* /usr/local/lib/display-agent/pending-verify
rm -f /etc/ssh/ssh_host_*            # 首启由 Armbian 重新生成
journalctl --rotate && journalctl --vacuum-time=1s
systemctl enable armbian-resize-filesystem 2>/dev/null || true   # 让克隆机首启自动扩展分区
apt-get clean
poweroff
```

在管理后台**删除样机注册的那台设备**（它的密钥已随 identity.json 删除）。

## 4. 读出并收缩镜像（开发机/Linux）

```sh
sudo dd if=/dev/sdX of=display-golden.raw bs=4M status=progress
sudo pishrink.sh -z display-golden.raw display-golden.img   # https://github.com/Drewsif/PiShrink
```

## 5. 批量烧录与上线

1. Armbian Imager（或 balenaEtcher）烧 `display-golden.img.gz`；若 Imager 支持，为每张卡填 hostname 作为编号；
2. 插卡、接屏、接网、上电；
3. 1~2 分钟内设备出现在管理后台设备列表（在线），编号为主机名或 `opi-xxxxxxxx`；
4. 后台改名、设属性（如 `room=302`）、绑定图片 → 屏幕在一个轮询周期内显示；
5. 点【测试】确认是哪块屏。

## 6. 程序 OTA

```sh
make agent-arm      # 产出新版本（版本号 = git tag/commit）
```
管理后台 → 固件页 → 上传 `bin/display-agent-armv7` 并填版本号 → 【下发】选择立即或定时、全部或指定设备。

设备端流程：轮询拿到 `update` 指令 → 断点续传下载 → sha256 校验 → `current` 符号链接切到新版本 →
进程退出 → systemd 拉起新版本 → 首个心跳成功即确认；若新版本连续 3 次启动失败，
`rollback-check.sh`（ExecStartPre）自动把 `current` 指回上一版本。

后台设备列表显示"程序版本 / 目标版本"，两者一致即完成。

## 7. 安全基线（harden.sh 做了什么，为什么）

- **系统自动更新关闭**并锁定内核/dtb 包：屏幕设备要的是十年如一日，不是最新内核；
- **入站默认拒绝**，仅放行运维地址的 SSH：设备不对外提供任何服务，唯一入站是运维；
- **SSH 保留但仅密钥、仅运维地址**：设备装好后难以物理接触，OTA 出问题时 SSH 是唯一救援通道，
  关掉它等于放弃远程修复能力；限定源地址后攻击面可忽略；
- 代理经 HMAC 签名访问服务端，密钥每台唯一，设备只能读到自己的内容。
