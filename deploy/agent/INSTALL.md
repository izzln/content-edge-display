# 设备端程序包

这个包（`display-agent-<版本>-armv7.tar.gz`）既是**装机包**，也是**程序更新（OTA）包**：
在管理后台「程序更新」页上传它，新设备装机和已有设备升级都从服务端取这同一个包。

```
display-agent            设备代理程序（ARMv7，静态编译；版本号内置，display-agent -version 可查）
install-agent.sh         首次安装：写配置、按 OTA 布局安装、设置显示参数、开机自启、重启
deps.txt                 播放所需的 Debian 软件包清单（安装与 make deps 共用）
update.sh                本版本的安装步骤：首次安装与每次 OTA 都执行（装 deps.txt：优先服务端的离线依赖包；
                         systemd 单元、固定路径的脚本等）
rollback-check.sh        OTA 回滚检查（由 systemd ExecStartPre 调用）
check-display.sh         现场自检：输出分辨率、硬件解码、CMA、SoC 温度
display-agent.service    systemd 单元
```

## 装机（推荐：一行命令）

先在管理后台「程序更新」页上传本包。然后在刚刷好**公版 Armbian**、设好 root 密码的设备上以 root 运行：

```sh
curl -fsSL http://<服务器>:9000/install.sh | ENROLL_TOKEN=注册口令 sh
```

- 注册口令就是服务端 `server.json` 里的 `enroll_token`；后台「程序更新」页上有这条命令可直接复制。
- 脚本由服务端生成，已填好服务端的 HTTPS 地址（`:9001`）、证书指纹与证书；它下载最新上传的程序包、执行
  `install-agent.sh`，装完自动重启。设备随后自动注册，1~2 分钟内出现在后台设备列表。
- 建议同时在后台上传离线依赖包（`make deps` 产出的 `display-deps-<代号>-armhf.tar.gz`，代号与设备
  `/etc/os-release` 的 `VERSION_CODENAME` 一致）：装机与之后 OTA 新增的依赖都从服务端局域网安装，几秒下完、不需要外网；
  没有时从外网 apt 安装。
- 批量装机可以用服务端包里的 `make-image.sh` 做插卡即装镜像（见 `docs/deployment.md` 4.2）。
- 可选参数写在 `ENROLL_TOKEN=...` 旁边，例如 `... | ENROLL_TOKEN=xxxx HDMI_FORCE=e sh`：

  | 参数 | 默认 | 说明 |
  |---|---|---|
  | `HDMI_MODE` | `1440x900@60` | 输出模式（写进内核 `video=` 参数与 `agent.json` 的 `display_mode`） |
  | `HDMI_FORCE` | 空 | `e`：EDID 里没有该模式时强制输出 |
  | `CMA` | 按内存定 | 连续内存（硬解缓冲、叠加层都从这里分），1GB 板 256M、512MB 板 192M |
  | `NO_REBOOT` | 空 | `1`：装完不自动重启（显示参数要重启才生效） |

手工安装（设备连不到服务端的 9000 端口时）：把本包拷到设备上解开，在包目录里以 root 运行

```sh
SERVER_URL=https://<服务器>:9001 TLS_FINGERPRINT=<后台显示的证书指纹> ENROLL_TOKEN=注册口令 ./install-agent.sh
```

（再加 `SERVER_CERT=<服务端 data/tls/server.crt 的拷贝>` 才能使用服务端的离线依赖包。）

## 现场排查

```sh
/usr/local/lib/display-agent/check-display.sh   # 分辨率、硬解、CMA、温度
journalctl -u display-agent -f
```

**不知道设备 IP、屏幕又被播放画面占着时**：插上 USB 键盘，按任意键，屏幕会切到控制台并显示设备信息
（编号、IP、MAC、服务端地址、连接状态），可直接登录 root；键盘 15 分钟不动自动恢复播放。

### 分辨率不对（画面错位）

面板 EDID 报的分辨率优先级高于默认设置，必须在内核命令行里显式指定，
`install-agent.sh` 已经把下面这行写进 `/boot/armbianEnv.txt` 的 `extraargs`：

```
video=HDMI-A-1:1440x900@60
```

**改完要重启才生效。** 如果重启后 `check-display.sh` 仍报显示屏不提供 1440x900，多半是这块 HDMI
驱动板的 EDID 里压根没有这个模式，内核就忽略了该参数——在 `video=` 参数末尾加 `,e` 强制输出（重新装机时加 `HDMI_FORCE=e`）。

实在不行就让模板跟着屏幕走：在管理后台把模板画布改成屏幕的实际分辨率。分辨率不符只是缩放、不会错位，
只是非等比时会有轻微形变。

## 程序更新

之后升级**不需要再登录设备**：在后台「程序更新」页上传新版本的整包并下发（立即或定时）。
设备下载整包、校验、执行包内 `update.sh`（含安装新增的依赖），成功后切换到新版本；新版本连续 3 次启动失败自动回滚到上一个版本
（程序和脚本一起回退），且不再自动重试这个版本。失败原因会显示在后台设备列表里。

完整说明见仓库 `docs/deployment.md`。
