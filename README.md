# content-edge-display

内容分布式显示系统：运营方在本地服务器的管理后台为每块显示屏编排图片/视频/PDF，屏上叠加房间号等属性，
设备经局域网轮询分发、断网照常播放。

- 硬件：Orange Pi One（全志 H3）+ LCD 1440×900（HDMI 驱动板）
- 已实现：端到端分发、管理后台（效果预览、口令管理）、模板（含节日底图、可拖动调整的版式）、上传转码与文件缓存区、
  设备一键装机/插卡即装与自注册、整包 OTA（离线依赖包、失败自动回滚）、HTTPS 指纹固定、设备访问凭据统一管理、
  插键盘现场救援；客户小程序与审核流程在规划中

## 文档

| 文档 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 架构与设计取舍：分发协议、模板渲染与硬件图层、底图、上传处理、装机与 OTA、安全与访问、可靠性、规划 |
| [docs/deployment.md](docs/deployment.md) | 部署与运维：成品包、服务端配置、设备装机、日常使用、OTA、排障、备份迁移、验机、无外网运行 |
| [docs/hardware.md](docs/hardware.md) | 硬件选型依据与候选对比，附二手盒子刷机指南 |

## 目录结构

```
cmd/display-server/   服务端入口
cmd/display-agent/    设备代理入口
internal/
  server/             HTTP API、清单生成、上传处理、程序包分发、管理后台路由
  agent/              设备端：注册、轮询下载、心跳、OTA、时钟校准、救援控制台
  player/             播放器：GStreamer 播放进程（cedrus 硬解 + DRM 图层直出）与 null（测试用）
  render/             模板与测试卡的服务端渲染
  transcode/          视频转码（ffmpeg）、图片缩放、PDF 逐页渲染（poppler）
  store/              状态持久化（设备、模板、程序包、更新目标）
  manifest/           清单与报文结构、版本号（两端共用）
  agentpkg/           设备端程序包的解包与校验（两端共用）
  sign/               设备请求 HMAC 签名与证书指纹（两端共用）
  fsutil/             原子写入、硬链接等文件操作
  web/                内嵌的管理后台单页
deploy/agent/         设备端程序包内容：安装/更新/回滚/自检脚本、systemd 单元
deploy/server/        服务端包内容：systemd 单元、配置样例、插卡即装镜像脚本
scripts/              构建辅助脚本（口令生成、离线依赖包）
bin/                  构建产物（gitignore）
```

## 快速开始

```sh
make package     # 需 Go ≥ 1.24；首次运行生成两个口令（.secrets/tokens.env，勿提交）
# → bin/display-server-<版本>-<架构>.tar.gz   服务端：解开后按包内 INSTALL.md 安装
# → bin/display-agent-<版本>-armv7.tar.gz     设备端程序包：在后台「管理」页上传，装机与 OTA 都用它
make deps        # 可选（需 docker + qemu）：离线依赖包，后台上传一次，装机与 OTA 从局域网装 GStreamer 等依赖
# → bin/display-deps-<代号>-armhf.tar.gz
```

服务端装好后打开 `https://<服务器>:9001/admin`；新设备刷公版 Armbian 后运行后台「管理」页给出的一键装机命令，
批量时用服务端包里的 `make-image.sh` 做插卡即装镜像。
完整步骤见 [docs/deployment.md](docs/deployment.md)。本机没有 Go 时可从 GitHub Actions 产物或 Release 下载两个包
（其中的口令是占位值，需自行生成，见 deployment.md 2.1）。

无显示环境下把 `agent.json` 的 `player` 设为 `"null"` 即可验证整条分发链路。

## 测试与 CI

```sh
make test        # go vet + go test ./...
```

`.github/workflows/ci.yml`：每次 push 跑 gofmt/vet/测试并上传成品包；推送 `v*` 标签自动发布 Release
并附上两个包与各 Debian 版本的离线依赖包（包名与程序内置版本一致，可直接在后台上传下发）。
