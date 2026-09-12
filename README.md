# content-edge-display

内容分布式显示系统：客户通过微信小程序上传公司宣传图片/视频，运营方审核后下发到客户专属显示屏（本地化部署，边缘设备轮询分发）。

- 架构设计与硬件选型：[docs/architecture.md](docs/architecture.md)
- 硬件：Orange Pi One（全志 H3）+ LCD 1440×900（HDMI 驱动板）
- 当前进度：**M1 已实现** —— 服务端 API + 设备端播放代理，端到端打通

## 组件

| 目录 | 说明 |
|---|---|
| `cmd/display-server` | 服务端：manifest 下发（ETag/304）、媒体分发（Range 续传）、心跳、**管理后台**（`/admin`） |
| `cmd/display-agent` | 设备代理：轮询下载、sha256 校验、原子切换、mpv 循环播放、心跳、systemd 软看门狗 |
| `internal/render` | 模板/测试卡服务端渲染（设备端零改动，渲染图走普通内容管线） |
| `web/admin` | 内嵌管理后台：设备在线状态与最后在线时间、测试屏下发、设备属性、显示模板 |
| `deploy/` | systemd 单元、示例配置、Orange Pi One 部署手册 |

## 快速开始（M1）

```sh
make build

# 1. 服务端（参照 deploy/server.example.json 写 server.json；
#    模板/测试屏中文渲染需 CJK 字体: apt install fonts-noto-cjk 并配置 font_path）
./bin/display-server -config server.json

# 2. 管理后台：浏览器打开 http://<服务器>:8080/admin （输入 admin_token）
#    - 设备在线状态/最后在线时间、测试屏（现场定位）、设备属性、显示模板

# 3. 投放内容：目录轮播模式直接往设备媒体目录放文件（H.264 MP4 / JPEG/PNG）
mkdir -p data/media/dev-001 && cp intro.mp4 data/media/dev-001/

# 4. 设备端（Orange Pi One 上，参照 deploy/orange-pi-one.md）
make agent-arm   # 交叉编译 ARMv7 二进制
```

开发/无显示环境下可用 `"player": "null"` 运行 agent 验证分发链路。

## 测试

```sh
make test
```
