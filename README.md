# content-edge-display

内容分布式显示系统：客户通过微信小程序上传公司宣传图片/视频，运营方审核后下发到客户专属显示屏（本地化部署，边缘设备轮询分发）。

- 架构设计与硬件选型：[docs/architecture.md](docs/architecture.md)
- 硬件：Orange Pi One（全志 H3）+ LCD 1440×900（HDMI 驱动板）
- 当前进度：**M1 已实现** —— 服务端 API + 设备端播放代理，端到端打通

## 组件

| 目录 | 说明 |
|---|---|
| `cmd/display-server` | 服务端：设备自注册、manifest 下发（ETag/304）、媒体/渲染图/固件分发（Range 续传）、心跳、**管理后台**（`/admin`） |
| `cmd/display-agent` | 设备代理：唯一编号与自注册、轮询下载、sha256 校验、原子切换、mpv 循环播放、心跳、**程序 OTA（失败自动回滚）**、systemd 软看门狗 |
| `internal/render` | 模板/测试卡服务端渲染（设备端零改动，渲染图走普通内容管线） |
| `web/admin` | 管理后台：设备在线/最后在线、测试屏、属性、**全局模板 + 每设备图片**、**时段计划**、**程序更新（立即/定时下发）** |
| `deploy/` | 母镜像制作与批量部署（`golden-image.md`）、安装/加固/回滚脚本、systemd 单元、示例配置 |

## 快速开始

```sh
make build

# 1. 服务端（参照 deploy/server.example.json 写 server.json：admin_token、enroll_token；
#    模板中文渲染需 CJK 字体: apt install fonts-noto-cjk 并配置 font_path）
./bin/display-server -config server.json

# 2. 管理后台：浏览器打开 http://<服务器>:8080/admin （输入 admin_token）
#    模板与时段 → 新建左右分屏模板 → 设为全局；设备 → 图片：为每台设备的图片区域选图

# 3. 设备端：按 deploy/golden-image.md 制作母镜像批量烧录；设备上电自动注册出现在后台
make agent-arm   # 交叉编译 ARMv7 二进制（版本号取 git describe），也是后台"程序更新"页上传的文件
```

开发/无显示环境下可用 `"player": "null"` 运行 agent 验证分发链路。

## 测试

```sh
make test
```
