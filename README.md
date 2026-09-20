# content-edge-display

内容分布式显示系统：客户通过微信小程序上传公司宣传图片/视频，运营方审核后下发到客户专属显示屏
（本地化部署，边缘设备轮询分发）。

- 硬件：Orange Pi One（全志 H3）+ LCD 1440×900（HDMI 驱动板）
- 当前进度：M1 及其增补已实现——端到端分发、管理后台、模板与时段、设备自注册、程序 OTA

## 文档

| 文档 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 系统架构与设计取舍：分发协议、模板渲染机制、可靠性设计、里程碑 |
| [docs/deployment.md](docs/deployment.md) | 服务端部署、设备单台/母镜像批量部署、日常运维、程序 OTA、验机清单、安全基线 |
| [docs/hardware.md](docs/hardware.md) | 嵌入式硬件选型依据与候选对比，附二手盒子刷机指南 |

## 目录结构

```
cmd/display-server/   服务端：设备注册/清单下发/媒体与固件分发/心跳/管理后台
cmd/display-agent/    设备代理：自注册、轮询下载、校验、驱动 mpv、心跳、程序 OTA
internal/
  server/             HTTP API 与管理后台路由
  agent/              设备端主循环、身份、下载、更新
  player/             播放器抽象：mpv（JSON IPC）与 null（无显示环境测试用）
  render/             模板与测试卡的服务端渲染
  store/              状态持久化（设备、模板、时段、固件、更新目标）
  manifest/           播放清单结构与版本号
  sign/               设备请求 HMAC 签名（两端共用）
  web/                内嵌的管理后台单页
deploy/agent/         设备端部署资产：安装/加固/回滚脚本、systemd 单元、配置样例
deploy/server/        服务端部署资产：systemd 单元、配置样例
docs/                 设计与运维文档
bin/                  构建产物与成品包（gitignore，非源码）
```

`bin/` 与 `deploy/` 都只是**构建的输入/输出**，部署时不用进去挑文件——
`make package` 会按角色各打一个自包含的压缩包，拷过去解开即可安装。

## 快速开始

```sh
make package          # 需 Go ≥ 1.24；首次构建需联网拉依赖
# → bin/display-agent-<版本>-armv7.tar.gz    设备端一包（含二进制、安装/加固脚本、systemd 单元、INSTALL.md）
# → bin/display-server-<版本>-<架构>.tar.gz  服务端一包

# 1. 服务端：拷过去解开，按包内 INSTALL.md 安装
#    必改 server.json 的 admin_token（后台口令）与 enroll_token（设备注册口令）；
#    模板中文渲染需 CJK 字体：apt install fonts-noto-cjk 并设置 font_path

# 2. 管理后台：浏览器打开 http://<服务器>:8080/admin （输入 admin_token）
#    模板与时段 → 新建左右分屏模板 → 设为全局；设备 → 图片：为每台设备指定图片

# 3. 设备端：按 docs/deployment.md 用设备端包装好一台样机、做成母镜像批量烧录，
#    之后设备上电自动注册，升级程序在后台"程序更新"页完成，无需再登录设备
```

无显示环境下把 agent 配置成 `"player": "null"` 即可验证整条分发链路。

## 测试

```sh
make test             # go vet + go test ./...
```
