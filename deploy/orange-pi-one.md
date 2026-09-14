# Orange Pi One 部署手册（Armbian + display-agent）

目标硬件：Orange Pi One（全志 H3，1GB，百兆网，HDMI）
显示屏：LCD 1440×900（HDMI 驱动板）

## 1. 烧录 Armbian

1. 从 [Armbian 官网](https://www.armbian.com/orange-pi-one/) 下载 Orange Pi One 的
   **Armbian Bookworm/最新 CLI（minimal 或 standard）** 镜像；
2. 用 balenaEtcher 写入 TF 卡（建议**工业级/高耐久** TF 卡，≥16GB）；
3. 首次上电走 Armbian 初始化向导（root 密码、普通用户可跳过），配置好网络（有线 DHCP 或静态 IP）。

## 2. 固定 HDMI 输出为 1440×900 并禁用息屏

编辑 `/boot/armbianEnv.txt`，追加/合并：

```
extraargs=video=HDMI-A-1:1440x900@60 consoleblank=0
```

- `video=HDMI-A-1:1440x900@60`：内核 KMS 强制输出 1440×900@60，不依赖显示器 EDID
  （部分廉价 HDMI 驱动板 EDID 不可靠，强制模式最稳）；
- `consoleblank=0`：禁用控制台自动息屏。

重启后用 `cat /sys/class/drm/card*-HDMI-A-1/modes` 确认 1440x900 生效。

## 3. 安装 mpv

```sh
apt update && apt install -y mpv
```

mpv 在无桌面环境下经 DRM 直接输出（`--vo=gpu` 自动选择 `gpu-context=drm`）。
如首次运行报 DRM 相关错误，在 agent.json 的 `mpv_extra_args` 中加：

```json
"mpv_extra_args": ["--vo=gpu", "--gpu-context=drm"]
```

H3 的视频硬解（Cedrus/v4l2）视 Armbian 内核版本而定；`--hwdec=auto-safe`
已是默认参数，不可用时自动回退软解——1440×900 内的 H.264 软解 H3 也能胜任，
但服务端投放的视频请控制在 **H.264 / ≤1440×900 / ≤30fps**（后续 M2 转码环节会强制归一）。

## 4. 部署 display-agent

在开发机上交叉编译并拷贝：

```sh
make agent-arm
scp bin/display-agent-armv7 root@<设备IP>:/usr/local/bin/display-agent
```

设备上：

```sh
mkdir -p /etc/display-agent
# 参照 deploy/agent.example.json 写 /etc/display-agent/agent.json
# device_id/secret 需与服务端 server.json 中该设备一致
cp display-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now display-agent
```

## 5. 验机清单（每台设备交付前）

| 检查项 | 方法 |
|---|---|
| HDMI 输出 1440×900 | `cat /sys/class/drm/card*-HDMI-A-1/modes` 首行为 1440x900 |
| 网络连通 | `curl -sI http://<服务器>:8080` 有响应 |
| 代理运行 | `systemctl status display-agent` active (running) |
| 软看门狗 | `systemctl show display-agent -p WatchdogTimestamp` 持续更新 |
| 播放验证 | 服务端设备目录放一张图+一段 H.264 视频，2 分钟内屏幕轮播 |
| 心跳可见 | 服务端 `GET /api/v1/admin/devices` 中该设备 online=true |
| 断电恢复 | 拔电重启后 1 分钟内自动恢复播放（无需人工干预） |
| 断网兜底 | 拔网线，播放不中断；插回后心跳恢复 |

## 6. M1 已知简化（后续里程碑处理）

- 图片展示时长为全局统一值（`image_duration_s`），暂不支持逐条目时长；
- 清单更新时 mpv `loadlist replace` 立即切换列表（"播完当前项再切"留待优化）；
- 管理后台的测试屏/模板/属性变更经设备轮询生效，延迟 ≈ `poll_interval_s`
  （局域网建议设 5~10s，304 轮询开销可忽略）；
- SoC 硬件看门狗（`/dev/watchdog`）、只读根文件系统、OTA 在 M4 实现；
  当前已有 systemd 软看门狗（进程假死 90s 内自动重启）+ mpv 进程自动拉起。
