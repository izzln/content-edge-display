# 部署与运维

面向运营方：服务端怎么装、显示屏怎么批量装、日常怎么用怎么升级。
系统设计与取舍见 [architecture.md](architecture.md)，硬件选型见 [hardware.md](hardware.md)。

## 0. 两层更新模型

| 层 | 内容 | 更新方式 |
|---|---|---|
| OS 母镜像 | Armbian + mpv + 安全加固 + 代理安装布局 + 含 `enroll_token` 的配置 | 烧录时一次性写入，之后不动 |
| 代理程序 | `display-agent` 单一静态二进制 | 管理后台上传 → 立即/定时下发 → 设备自动切换、失败自动回滚 |

设备装好后很难再物理接触，所以**除首次烧录外的一切变更都必须能远程完成**——这是整套
自注册 + OTA 设计的出发点。

## 1. 取得成品包

部署用的一切都在**每个角色一个自包含压缩包**里，拷过去解开即可安装，不需要从源码树里手工挑文件。
两种取得方式，任选其一：

### 1.1 本地构建

```sh
make package
# → bin/display-agent-<版本>-armv7.tar.gz    设备端（二进制 + 安装/加固/回滚脚本 + systemd 单元 + 配置样例）
# → bin/display-server-<版本>-<架构>.tar.gz  服务端（二进制 + systemd 单元 + 配置样例）
```

（`make build` / `make agent-arm` 仍然只产出裸二进制，OTA 上传用的就是 `bin/display-agent-armv7`。）

### 1.2 从 GitHub 下载（本机没有 Go 环境时）

仓库配了 GitHub Actions（`.github/workflows/ci.yml`）：

- **每次 push**：跑 gofmt/vet/测试，并把两个成品包作为 Actions 构建产物上传，
  在仓库 Actions 页面对应那次运行的 Artifacts 里下载（保留 90 天）；
- **推送 `v*` 标签**：自动创建 Release，把两个包作为附件挂上去。

```sh
git tag v1.2.0 && git push origin v1.2.0     # 随后在 Releases 页面下载
```

标签构建会把标签名同时用作**包名**与**二进制内置版本**，两者必定一致——
管理后台"程序更新"页填的版本号必须与二进制内置版本相同，OTA 才能正确判断设备是否已升到目标版本。

> 服务端包按 Actions 运行器的架构（amd64）构建。若你的服务器是 ARM，请在本地用
> `GOOS=linux GOARCH=arm64 make package-server` 自行构建。

两个包里都带 `INSTALL.md`，现场不用带着仓库也能装。

## 2. 服务端部署（运营方本地服务器）

推荐**自包含目录**布局：二进制、配置、内容、状态、字体全在一个目录下，整个目录拷走即可搬迁。

```sh
scp bin/display-server-*.tar.gz root@<服务器>:/root/
ssh root@<服务器>
tar xzf display-server-*.tar.gz && cd display-server-*/

apt install -y fonts-noto-cjk          # 模板中文由服务端渲染，缺字体会变方框
install -d /srv/display/fonts
install -m 0755 display-server /srv/display/
cp server.example.json /srv/display/server.json          # 按下表改写
cp /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc /srv/display/fonts/

useradd -r -s /usr/sbin/nologin display 2>/dev/null || true
chown -R display:display /srv/display
install -m 0644 display-server.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now display-server
```

```
/srv/display/
  display-server      二进制
  server.json         配置
  media/<设备ID>/     目录轮播模式要播的图片与视频（运营方放）
  fonts/              渲染用字体
  data/               服务端状态：state.json、uploads/、firmware/、rendered/（首次启动自动创建）
```

**配置里的相对路径按 `server.json` 所在目录解析**，与进程工作目录无关——systemd 启动服务时
工作目录是 `/`，若按工作目录解析，`"data"` 会悄悄落到 `/data`。要用 FHS 布局
（二进制 `/usr/local/bin`、配置 `/etc`、数据 `/var/lib`）就在配置里写绝对路径，并相应改
`display-server.service` 的 ExecStart。

`server.json` 关键字段：

| 字段 | 说明 |
|---|---|
| `admin_token` | 管理后台口令。**未配置时所有写接口一律拒绝**，避免管理面裸奔 |
| `enroll_token` | 设备自注册口令，需与母镜像里 `agent.json` 的同名字段一致 |
| `media_root` | 目录轮播模式的内容目录，下面按设备 ID 分子目录 |
| `data_dir` | 服务端状态：`state.json`、上传图片、渲染结果、固件 |
| `font_path` | CJK 字体文件路径（是文件不是目录），缺失则中文渲染成方框 |
| `timezone` | 时段计划所用时区，默认取系统时区 |
| `devices` | **可留空**。静态配置的设备，见 2.1 |
| `image_duration_s` | 可选，图片停留秒数的初始默认值（默认 10）。**日常在管理后台改**，后台的设置优先 |

管理后台：浏览器打开 `http://<服务器>:8080/admin`，首次访问输入 `admin_token`。

### 2.1 `devices` 数组是做什么的

它是**静态配置的设备**，在自注册机制之前是登记设备的唯一方式，现在留作两种用途：
调试时用固定的 id/secret 起一台设备；或给某台设备钉死一个你自己指定的密钥。

批量部署不需要它——设备凭 `enroll_token` 自注册，信息写进 `data_dir/state.json`。
两类设备在后台都能看到，区别是：

| | 静态配置（`devices`） | 自注册 |
|---|---|---|
| 来源 | `server.json` | 设备首次上电时写入 `state.json` |
| 后台改名/删除 | 不行（返回 409，请改配置文件） | 可以 |
| 适用 | 调试、少数需要固定密钥的机器 | 批量部署的常规路径 |

`devices` 与 `enroll_token` 至少要有一个，否则没有任何设备能接入，服务端会拒绝启动。

## 3. 设备端：单台部署（样机、调试、也是制作母镜像的第一步）

目标硬件：Orange Pi One（全志 H3，1GB，百兆网，HDMI）+ LCD 1440×900（HDMI 驱动板）。

### 3.1 烧录 Armbian

1. 从 [Armbian 官网](https://www.armbian.com/orange-pi-one/) 下载 Orange Pi One 的
   **Bookworm CLI（minimal 或 standard）** 镜像；
2. 用 balenaEtcher 写入 TF 卡（建议**工业级/高耐久** TF 卡，≥16GB）；
3. 首次上电走初始化向导（设 root 密码，普通用户可跳过），配好网络。

### 3.2 固定 HDMI 输出为 1440×900 并禁用息屏

编辑 `/boot/armbianEnv.txt`（`install-agent.sh` 会自动追加，手工部署时可自己加）：

```
extraargs=video=HDMI-A-1:1440x900@60 consoleblank=0
```

- `video=HDMI-A-1:1440x900@60`：内核 KMS 强制输出该模式，**不依赖显示器 EDID**
  （廉价 HDMI 驱动板的 EDID 常不可靠，强制指定最稳）；
- `consoleblank=0`：禁用控制台自动息屏。

重启后 `cat /sys/class/drm/card*-HDMI-A-1/modes` 首行应为 `1440x900`。

### 3.3 安装 mpv 与代理

```sh
# 开发机：拷一个包过去即可
scp bin/display-agent-*-armv7.tar.gz root@<设备IP>:/root/

# 设备上（root）
tar xzf display-agent-*-armv7.tar.gz && cd display-agent-*/
SSH_ALLOW_FROM=<服务器IP> SSH_PUBKEY="ssh-ed25519 AAAA... ops" ./harden.sh
SERVER_URL=http://<服务器IP>:8080 ENROLL_TOKEN=<server.json 的 enroll_token> ./install-agent.sh
systemctl start display-agent
journalctl -u display-agent -n 20     # 应看到注册成功
```

设备会自动注册并出现在管理后台（在线），编号规则见 4.1。

mpv 在无桌面环境下经 DRM 直接输出。如报 DRM 相关错误，在 `/etc/display-agent/agent.json`
的 `mpv_extra_args` 中加 `["--vo=gpu", "--gpu-context=drm"]`。H3 的硬解（Cedrus/v4l2）
视内核版本而定，`--hwdec=auto-safe` 不可用时自动回退软解——1440×900 的 H.264 软解 H3 也够用，
但投放视频仍建议控制在 **H.264 / ≤1440×900 / ≤30fps**。

## 4. 设备端：母镜像批量部署

所有设备烧**同一个镜像**，首次上电自动获得唯一编号并注册。

### 4.1 设备编号规则

代理首次启动按以下优先级确定编号，并持久化到 `/var/lib/display-agent/identity.json`：

1. `agent.json` 里显式的 `device_id`（手工配置场景）；
2. **主机名**——烧录时在 Armbian Imager 的"自定义设置"里填写 hostname（如 `scr-0017`）即为设备编号；
3. 主机名是默认值（`orangepione` 等）时，取 SoC 序列号后 8 位 → `opi-1a2b3c4d`（再退回 eth0 MAC）。

> Armbian Imager 的自定义对官方镜像有效；对克隆的母镜像是否生效**需实机验证一次**。
> 不生效也没关系：规则 3 保证编号唯一，在管理后台把名称改成"3 楼大堂"即可。

密钥在首启随机生成，只存在于设备与服务端两处；`enroll_token` 仅用于首次注册。
同 ID 不同密钥的注册会被拒绝（409），防止冒名顶替。

### 4.2 制作母镜像

先按第 3 节把一台样机完整装好并验证通过，然后清理成"出厂状态"：

```sh
systemctl stop display-agent
rm -f /var/lib/display-agent/identity.json /var/lib/display-agent/current.json
rm -rf /var/lib/display-agent/media/* /var/lib/display-agent/playlist.m3u
rm -f /usr/local/lib/display-agent/pending-verify
rm -f /etc/ssh/ssh_host_*                                        # 首启重新生成
journalctl --rotate && journalctl --vacuum-time=1s
systemctl enable armbian-resize-filesystem 2>/dev/null || true   # 克隆机首启自动扩展分区
apt-get clean
poweroff
```

然后在管理后台**删除样机注册的那台设备**（它的密钥已随 identity.json 删除）。

### 4.3 读出并收缩镜像（开发机/Linux）

```sh
sudo dd if=/dev/sdX of=display-golden.raw bs=4M status=progress
sudo pishrink.sh -z display-golden.raw display-golden.img   # https://github.com/Drewsif/PiShrink
```

### 4.4 批量烧录与上线

1. 用 Armbian Imager / balenaEtcher 烧 `display-golden.img.gz`；若 Imager 支持，为每张卡填 hostname 作为编号；
2. 插卡、接屏、接网、上电；
3. 1~2 分钟内设备出现在管理后台设备列表（在线）；
4. 后台改名、设属性（如 `room=302`）、绑定图片 → 屏幕在一个轮询周期内更新；
5. 点【测试】确认是哪块屏。

## 5. 日常运维（管理后台）

### 5.1 内容

- **模板与时段**页：新建左右分屏模板 → 【设为全局】。所有设备默认显示该模板；
- **设备**页每行【图片】：为该设备指定图片区域要显示的图（上传/选择）；
- **设备**页每行【属性】：`room=302` 之类的键值对，模板的 attribute 区域按 key 取值显示；
- **模板与时段**页下方：按顺序配置 `{模板, 星期, 起止时间}`，支持跨午夜；无命中回落全局模板；
- **模板与时段**页顶部的"图片停留 N 秒"：目录轮播模式下每张图片的停留时长，对所有设备生效，
  改完一个轮询周期内到位（视频按自身长度播完；模板模式是单张静态图，不受此项影响）；
- 生效延迟 ≈ 设备的 `poll_interval_s`（局域网建议 5~10s，304 轮询开销可忽略）。

### 5.2 现场定位

**设备**页每行【测试】→ 选 1/5/15 分钟：该屏全屏显示"测试"卡片（含设备名与属性），到期自动恢复。
这条路径与正常内容走同一套分发管线，因此测试成功本身就验证了整条链路。

### 5.3 程序 OTA

```sh
make agent-arm      # 版本号取自 git describe，也可 make agent-arm VERSION=1.2.0
```

**程序更新**页要选的文件是 **`display-agent-armv7` 这个裸二进制**，不是 `.tar.gz` 成品包：

| 来源 | 路径 |
|---|---|
| 本地构建 | `bin/display-agent-armv7`（`make agent-arm` 或 `make package` 都会产出） |
| 从 GitHub 下载 | 把 `display-agent-<版本>-armv7.tar.gz` 解开，取里面的 `display-agent-armv7` |

版本号必须与二进制内置版本**完全一致**（`./display-agent-armv7 -version` 可核对；
标签构建时就是标签名）。服务端在上传时会读取二进制的构建信息校验三件事，任何一项不符都当场拒绝、
不会下发到设备：是不是 Go 二进制（挡住误传 tar.gz）、目标平台是不是 linux/arm（挡住误传本机架构的
`bin/display-agent`）、内置版本与填写的版本号是否一致。

填好后点【下发】，选择立即或定时、全部或指定设备。

设备端流程：轮询取到指令 → 断点续传下载 → sha256 校验 → `current` 符号链接原子切换 →
进程退出由 systemd 拉起新版本 → 首个心跳成功即确认。若新版本连续 3 次启动失败，
`rollback-check.sh`（systemd `ExecStartPre`，用系统 sh 执行、不依赖新二进制）自动切回上一版本。

后台设备列表显示"程序版本 → 目标版本"，两者一致即完成。

**OTA 能改什么、不能改什么**——设备装好后很难再物理接触，这条边界决定了哪些改动要提前想清楚：

| | 内容 |
|---|---|
| 仅 OTA 代理即可 | 播放逻辑、mpv 启动参数、播放列表行为、清单新字段的解析、下载与缓存策略、心跳内容 |
| 还需同时更新服务端 | 清单生成、模板渲染、管理后台界面（服务端在机房，更新它不用去现场） |
| **OTA 改不了，需要 SSH** | `/etc/display-agent/agent.json`（OTA 只替换二进制）、systemd 单元、系统软件包（mpv、内核、DRM 驱动） |

所以新增设备端配置项时，务必让"缺省值即可用"——否则这批设备就得逐台登录。

**播放能力现状**：图片与视频都已支持（H.264 MP4 等，见 `internal/manifest` 的扩展名表），
目录轮播模式下图文混排、视频播完自动切下一条、列表循环都已实测可用；视频不需要任何升级。
模板渲染的是静态图，因此"模板里嵌视频"目前不支持，那需要服务端配合，不是仅升级代理能做到的。

## 6. 服务端升级与数据迁移

服务端的全部可变状态只有两处：**`data_dir`** 和 **`media_root`**。自包含布局下它们都在
`/srv/display` 里，所以升级和搬迁都很直接。

| 路径 | 内容 | 要不要保留 |
|---|---|---|
| `data/state.json` | 设备（含自注册设备的密钥）、属性、模板、时段、全局设置、固件元数据、更新目标 | **必须** |
| `data/uploads/` | 后台上传的图片 | **必须** |
| `data/firmware/` | 上传的代理程序 | 建议（否则待下发的更新目标会失效） |
| `data/rendered/` | 模板/测试卡的渲染结果 | 不必，缺了会自动重新渲染 |
| `media/` | 目录轮播模式的内容 | **必须** |
| `server.json` | 配置 | **必须**（`enroll_token` 变了设备就注册不进来） |

### 6.1 原地升级

```sh
systemctl stop display-server
cp display-server /srv/display/display-server     # 只换二进制
systemctl start display-server
```

`state.json` 的字段是增量演进的，新版本读旧文件时缺失字段取零值，不需要迁移脚本。
保险起见升级前先 `cp -a /srv/display/data /srv/display/data.bak`。

### 6.2 换一台服务器

```sh
systemctl stop display-server                      # 可选，见下
tar czf display-backup.tar.gz -C /srv display
# 在新机器上解开到同样的 /srv/display，装好 systemd 单元后启动
```

`state.json` 是原子写入（临时文件 + rename），所以**热备份也是一致的**——不停机拷贝拿到的
要么是旧版本要么是新版本，不会拿到写坏的半个文件。停机只是为了避免拷贝过程中运营方刚好在改配置。

> **搬迁前务必确认一件事**：设备端 `agent.json` 里的 `server_url` 是写死在每台设备上的，
> OTA 只替换二进制、改不了它。服务器换 IP 就意味着要逐台 SSH。
> 所以**从一开始就用域名而不是 IP**（例如 `http://display.lan:8080`），
> 搬迁时只改 DNS 指向即可，设备无感。

### 6.3 定期备份

```sh
tar czf /backup/display-$(date +%F).tar.gz -C /srv display --exclude='display/data/rendered'
```

排除 `rendered/` 可以显著减小体积，它会按需重新生成。

## 7. 验机清单（每台设备交付前）

| 检查项 | 方法 |
|---|---|
| HDMI 输出 1440×900 | `cat /sys/class/drm/card*-HDMI-A-1/modes` 首行为 1440x900 |
| 网络连通 | `curl -sI http://<服务器>:8080` 有响应 |
| 代理运行 | `systemctl status display-agent` active (running) |
| 软看门狗 | `systemctl show display-agent -p WatchdogTimestamp` 持续更新 |
| 播放验证 | 后台点【测试】，2 分钟内屏幕出现测试卡 |
| 心跳可见 | 管理后台该设备 online、程序版本正确 |
| 断电恢复 | 拔电重启后 1 分钟内自动恢复播放上次内容（无需人工干预） |
| 断网兜底 | 拔网线，播放不中断；插回后心跳恢复 |

## 8. 安全基线（`harden.sh` 做了什么，为什么）

- **关闭系统自动更新**并 `apt-mark hold` 内核/dtb 包：屏幕设备要的是十年如一日，
  一次内核升级就可能打碎显示输出或硬解；
- **nftables 入站默认 DROP**，仅放行 lo、已建立连接、ICMP 与来自运维地址的 SSH：
  设备不对外提供任何服务，唯一入站就是运维；
- **SSH 保留，但仅密钥登录 + 仅运维地址可达**。设备装好后难以物理接触，OTA 万一出问题时
  SSH 是唯一的远程救援通道，关掉等于放弃远程修复能力；限定源地址后攻击面可以忽略；
- 禁用 avahi / bluetooth 等无关服务；锁定 root 口令。

> 执行 `harden.sh` 后，**先用另一个终端确认密钥 SSH 能登录，再断开当前会话**。

## 9. 当前已知简化

- 图片展示时长在**管理后台**设置（服务端 `image_duration_s` 只是初始默认值，设备端 agent.json
  的同名字段只是收到第一份清单之前的兜底）；同一份清单里的图片共用一个时长，暂不支持逐条目时长——mpv 的 m3u 不支持
  逐条目选项。单张静态图（模板模式恒为此情形）用 `inf`，不会周期性重载；
- 清单更新时 mpv `loadlist replace` 立即切换列表（"播完当前项再切"留待优化）；
- SoC 硬件看门狗（`/dev/watchdog`）与只读根文件系统在 M4 实现；当前已有 systemd 软看门狗
  （进程假死 90s 内重启）、mpv 进程自动拉起，以及播放列表巡检（mpv 未加载期望内容时自动重推）。
