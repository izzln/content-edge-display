# content-edge-display

内容分布式显示系统：客户通过微信小程序上传公司宣传图片/视频，运营方审核后下发到客户专属显示屏
（本地化部署，边缘设备轮询分发）。

- 硬件：Orange Pi One（全志 H3）+ LCD 1440×900（HDMI 驱动板）
- 当前进度：M1 及其增补已实现——端到端分发、管理后台、模板与时段、设备自注册、程序 OTA

## 文档

| 文档 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 系统架构与设计取舍：分发协议、模板渲染机制、可靠性设计、里程碑 |
| [docs/deployment.md](docs/deployment.md) | 服务端部署、设备一键装机、日常运维、整包 OTA、现场救援、验机清单、通信安全 |
| [docs/hardware.md](docs/hardware.md) | 嵌入式硬件选型依据与候选对比，附二手盒子刷机指南 |

## 目录结构

```
cmd/display-server/   服务端：设备注册/清单下发/媒体与固件分发/心跳/管理后台
cmd/display-agent/    设备代理：自注册、轮询下载、校验、驱动 GStreamer 播放、心跳、程序 OTA
internal/
  server/             HTTP API 与管理后台路由
  agent/              设备端主循环、身份、下载、更新
  player/             播放器：GStreamer 播放进程（cedrus 硬解 + KMS 图层直出）与 null（测试用）
  render/             模板与测试卡的服务端渲染
  transcode/          上传素材归一化：视频转码（ffmpeg）、图片缩放
  store/              状态持久化（设备、模板、时段、固件、更新目标）
  manifest/           播放清单结构与版本号
  sign/               设备请求 HMAC 签名（两端共用）
  web/                内嵌的管理后台单页
deploy/agent/         设备端部署资产：安装/加固/回滚脚本、systemd 单元、配置样例
deploy/server/        服务端部署资产：systemd 单元、配置样例
scripts/              构建辅助脚本（口令生成）
docs/                 设计与运维文档
bin/                  构建产物与成品包（gitignore，非源码）
```

`bin/` 与 `deploy/` 都只是**构建的输入/输出**，部署时不用进去挑文件——
`make package` 会按角色各打一个自包含的压缩包，拷过去解开即可安装。

## 快速开始

```sh
make package          # 需 Go ≥ 1.24；首次构建需联网拉依赖，并生成两个口令（见下）
# → bin/display-agent-<版本>-armv7.tar.gz    设备端程序包（程序 + 安装/更新/回滚脚本 + systemd 单元）：装机与 OTA 都用它
# → bin/display-server-<版本>-<架构>.tar.gz  服务端一包
# 本机没有 Go 环境时可从 GitHub Actions 产物或 Release 下载这两个包；
# 但公开仓库的 CI 产物里不含真实口令（占位值），需自行生成，见 docs/deployment.md 2.1

# 1. 服务端：拷过去解开，按包内 INSTALL.md 安装
#    server.json 里的 admin_token / enroll_token 已由 make 生成填好（存于 .secrets/tokens.env，勿提交）
#    模板中文渲染需 CJK 字体：apt install fonts-noto-cjk 并设置 font_path
#    视频转码需 ffmpeg：apt install ffmpeg（不装视频不转码，高码率原片会让设备过热）

# 2. 管理后台：浏览器打开 https://<服务器>:9001/admin （自签证书，首次选"继续访问"；输入 admin_token）
#    首启已自动建好"左右分屏"模板并设为全局默认，直接在 设备 → 内容 里为每台设备
#    上传要播的图片/视频即可（视频自动转码；属性在左还是在右用"左右对调"开关切换）

# 3. 设备端：后台"程序更新"页上传设备端程序包；设备刷公版 Armbian 后以 root 运行
#      curl -fsSL http://<服务器>:9000/install.sh | ENROLL_TOKEN=<enroll_token> sh
#    装完自动重启并注册；之后升级在后台"程序更新"页完成（整包 OTA），无需再登录设备
```

无显示环境下把 agent 配置成 `"player": "null"` 即可验证整条分发链路。

## 测试与 CI

```sh
make test             # go vet + go test ./...
```

`.github/workflows/ci.yml`：每次 push 跑 gofmt/vet/测试并上传成品包；推送 `v*` 标签自动发布
Release 并附上两个包（包名与二进制内置版本一致，可直接用于后台 OTA 下发）。
