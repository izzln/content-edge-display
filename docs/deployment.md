# 部署与运维

面向运营方：服务端怎么装、显示屏怎么批量装、日常怎么用怎么升级。
系统设计与取舍见 [architecture.md](architecture.md)，硬件选型见 [hardware.md](hardware.md)。

## 0. 两层更新模型

| 层 | 内容 | 更新方式 |
|---|---|---|
| OS 母镜像 | Armbian + GStreamer + 安全加固 + 代理安装布局 + 含 `enroll_token` 的配置 | 烧录时一次性写入，之后不动 |
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

两个包里都带 `INSTALL.md`，现场不用带着仓库也能装。服务端包里的 `server.json`
已填好自动生成的口令，设备端包里带匹配的 `enroll-token`（见 2.1）。

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
  media/<设备ID>/     该设备的播放内容（后台上传，也可直接拷进来）
  fonts/              渲染用字体
  data/               服务端状态：state.json、firmware/、rendered/、incoming/（首次启动自动创建）
```

**配置里的相对路径按 `server.json` 所在目录解析**，与进程工作目录无关——systemd 启动服务时
工作目录是 `/`，若按工作目录解析，`"data"` 会悄悄落到 `/data`。要用 FHS 布局
（二进制 `/usr/local/bin`、配置 `/etc`、数据 `/var/lib`）就在配置里写绝对路径，并相应改
`display-server.service` 的 ExecStart。

`server.json` 关键字段：

| 字段 | 说明 |
|---|---|
| `admin_token` | 管理后台口令，由 `make` 生成并已填好（见 2.1）。**留空则所有写接口一律拒绝**，避免管理面裸奔 |
| `enroll_token` | 设备自注册口令，由 `make` 生成并已填好；设备端包里的 `enroll-token` 与之匹配 |
| `media_root` | 各设备播放内容的根目录，下面按设备 ID 分子目录；后台上传的图片/视频落在这里 |
| `data_dir` | 服务端状态：`state.json`、上传图片、渲染结果、固件 |
| `font_path` | CJK 字体文件路径（是文件不是目录），缺失则中文渲染成方框 |
| `timezone` | 时段计划与测试卡显示所用的时区（IANA 名称，如 `Asia/Shanghai`、`Asia/Tokyo`），留空取服务器系统时区。全部时区数据已内嵌，不依赖系统 tzdata。样例里是 `Asia/Shanghai`，在其他地区部署时记得改 |
| `poll_interval_s` | 设备轮询间隔，默认 10 秒（1~300）。决定后台改动多快上屏、多快发现设备离线（约 3 个周期没来即离线，至少 30 秒）。**所有设备照这里的值执行**：随每个响应下发，改完重启服务端，设备下一次请求就跟上，不用逐台改 |
| `heartbeat_interval_s` | 设备心跳间隔，默认 60 秒（10~3600）。心跳只上报温度、硬解、输出分辨率等健康数据，不影响在线判断；同样由设备照办 |
| `ffmpeg_path` | 可选，ffmpeg 路径；留空在**服务进程的** PATH 里找（systemd 下只有 `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin`）。不可用时不能上传视频（见 5.1） |

`media_root` 与 `data_dir` 及其子目录在服务端启动时自动创建，不用手工 mkdir。

图片停留时长不在这里配置——它是版式的一部分，写在模板里，在管理后台改。

管理后台：浏览器打开 `http://<服务器>:9000/admin`，首次访问输入 `admin_token`。

### 2.1 两个口令从哪来

`make package` 会在本地生成一次并长期沿用，写在 `.secrets/tokens.env`（已 gitignore，
`make clean` 不会删）。8 位字母数字，含大小写与数字，不带符号——这两个值要写进 JSON、
经环境变量传给脚本、还要在浏览器里手敲，带符号只会徒增转义和输入错误。

```sh
make tokens     # 查看当前口令；文件不存在时生成
```

打出来的**服务端包里的 `server.json` 已经填好这两个口令**，设备端包里也带了一份
匹配的 `enroll-token`，所以装机时不用再想口令怎么定、也不用手工对齐两边。

| 口令 | 用途 | 改了会怎样 |
|---|---|---|
| `admin_token` | 登录管理后台 | 重新登录即可，无其他影响 |
| `enroll_token` | 设备自注册的凭证，烧进母镜像 | **已注册的设备不受影响**（它们用的是各自的密钥），但用旧母镜像新刷的设备注册不进来 |

所以 `.secrets/tokens.env` 要保管好：丢了不影响现有设备运行，但要加新设备就得重做母镜像。

> **公开仓库的注意事项**：CI 构建（GitHub Actions）**不会**把真实口令打进产物——
> 否则它们会随 Release 附件公开。从 Releases 下载的包里是占位值 `change-me`，
> 服务端带着占位值会直接拒绝启动并提示你生成。自己 `make package` 出来的包才含真实口令，
> 因此那个包本身也要当作机密对待，别到处发。

### 2.2 设备是怎么登记的

没有"在配置里登记设备"这回事——设备一律凭 `enroll_token` 自注册，信息写进
`data_dir/state.json`：首次上电时自己确定编号、生成随机密钥、向服务端注册，
随后出现在后台设备列表里。同一编号用不同密钥再注册会被拒（409），
防止冒名顶替；这种请求会显示在后台该设备上，核对无误可一键「接受新密钥」。详见 4.1。

## 3. 设备端：单台部署（样机、调试、也是制作母镜像的第一步）

目标硬件：Orange Pi One（全志 H3，1GB，百兆网，HDMI）+ LCD 1440×900（HDMI 驱动板）。

### 3.1 烧录 Armbian

1. 从 [Armbian 官网](https://www.armbian.com/orange-pi-one/) 下载 Orange Pi One 的
   **Bookworm CLI（minimal 或 standard）** 镜像；
2. 用 balenaEtcher 写入 TF 卡（建议**工业级/高耐久** TF 卡，≥16GB）；
3. 首次上电走初始化向导（设 root 密码，普通用户可跳过），配好网络。

### 3.2 固定 HDMI 输出为 1440×900 并禁用息屏

面板的 EDID 往往把 1920×1080 报成首选模式，**要在两个地方都指定**，`install-agent.sh` 会一并写好：

| 位置 | 内容 | 管什么 |
|---|---|---|
| `/boot/armbianEnv.txt` 的 `extraargs` | `video=HDMI-A-1:1440x900@60 consoleblank=0` | 内核控制台的输出模式；`consoleblank=0` 禁用息屏 |
| `/etc/display-agent/agent.json` | `"display_mode": "1440x900@60"` | 播放进程设置的显示模式 |

只改第一处是不够的：**播放进程自己设置显示模式，不理会内核的 `video=` 参数**，不指定就用 EDID 的
首选模式，于是控制台是 1440×900、一播放内容又变回 1080p。显示屏不提供 `display_mode` 时，
代理退回首选模式（日志里会提示）——设一个显示屏不认的模式只会黑屏。

驱动板 EDID 里压根没有 1440×900 时，内核会忽略 `video=`，该模式也不会出现在可用列表里。
这时加 `,e` 强制输出：`HDMI_FORCE=e ./install-agent.sh`（即 `video=HDMI-A-1:1440x900@60,e`）。

改完要**重启**，然后运行 `/usr/local/lib/display-agent/check-display.sh` 确认（见 3.4）。

> 即便最终输出分辨率与模板画布不一致，画面也不会错位：设备端会按实际输出分辨率
> 重新缩放叠加图，媒体区按同一比例换算。只是非等比时属性文字会有轻微形变，
> 所以仍应把输出模式配对。

### 3.3 安装 GStreamer 与代理

```sh
# 开发机：拷一个包过去即可
scp bin/display-agent-*-armv7.tar.gz root@<设备IP>:/root/

# 设备上（root）
tar xzf display-agent-*-armv7.tar.gz && cd display-agent-*/
SSH_ALLOW_FROM=<服务器IP> SSH_PUBKEY="ssh-ed25519 AAAA... ops" ./harden.sh
SERVER_URL=http://display.lan:9000 ./install-agent.sh   # 注册口令取包内 enroll-token
reboot                                                  # 让 HDMI 模式生效
systemctl start display-agent
journalctl -u display-agent -n 20     # 应看到注册成功
/usr/local/lib/display-agent/check-display.sh            # 见 3.4
systemctl enable display-agent        # 确认无误后再设为开机自启
```

`install-agent.sh` 的两个行为要知道：

- **每次运行都会重写** `/etc/display-agent/agent.json`（改服务端地址重跑一遍即可，不用先删文件）。
  设备编号与密钥在 `/var/lib/display-agent/identity.json`，不受影响；
- **装完不会 enable 服务**，要人工确认画面无误后自己 `systemctl enable display-agent`。
  做母镜像时别忘了这一步，否则烧出来的设备开机不播放（见 4.2）。

设备会自动注册并出现在管理后台（在线），编号规则见 4.1。

`install-agent.sh` 会装好播放所需的 GStreamer 组件（`python3-gst-1.0`、`gstreamer1.0-plugins-good/bad`、
`gstreamer1.0-libav` 等）。播放进程在无桌面环境下经 DRM/KMS 直接出画面，不需要 X11/Wayland，
也不经 GPU。

### 3.4 确认分辨率、硬件解码与温度

```sh
/usr/local/lib/display-agent/check-display.sh
```

逐项检查并给出处理办法，有问题时退出码为 1：

| 项 | 怎么判断 | 不对时的现场表现 |
|---|---|---|
| 内核输出模式 | `/sys/class/drm/card*-HDMI-A-1/modes` 里有没有 1440×900 | 播放时退回首选模式，画面按比例缩放 |
| 硬件解码条件 | cedrus 已加载、`gst-inspect-1.0 v4l2slh264dec` 存在、`kmssink` 与 Python 绑定可用 | 视频退化成软解：发热、卡顿，严重时过热关机 |
| 实际播放状态 | 代理每次心跳写的 `/var/lib/display-agent/status.json`：解码器、输出分辨率 | — |
| SoC 温度 | `/sys/class/thermal/thermal_zone0/temp` | 85°C 起降频，再高关机 |

解码方式、输出分辨率、温度也随心跳上报，**管理后台设备列表里直接能看到**：软解与 80°C 以上标红，
输出分辨率不是 1440×900 时标黄。不用登录设备就能发现哪台硬解失效或过热。

#### 硬件解码怎么走

H3 的硬件解码器（主线内核驱动 cedrus）支持 H.264 和 H.265，它是"无状态"解码器，要通过
V4L2 Request API 驱动。GStreamer 的 v4l2codecs 插件（`gstreamer1.0-plugins-bad`，Debian 自带）
原生支持，内核驱动正常时会注册 `v4l2slh264dec`，并且优先级高于软解，播放时自动选用。
（FFmpeg 上游没有这部分支持，所以 mpv 只能软解——这是播放器改用 GStreamer 的原因。）

`v4l2slh264dec` 不出现时，按顺序查：`ls /dev/video* /dev/media*` 有没有 cedrus 的设备、
`dmesg | grep -i cedrus` 有没有报错（如 CMA 内存不足）、`gstreamer1.0-plugins-bad` 装了没有；
改完后 `rm -rf ~/.cache/gstreamer-1.0` 让 GStreamer 重新扫描插件。

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

#### 注册成功，但轮询/心跳报 401 unauthorized

签名请求带时间戳，服务端只认与自己相差 ±5 分钟以内的（防重放），注册请求不签名所以不受影响。
服务端日志（`journalctl -u display-server | grep 'auth rejected'`）和设备日志都会写明原因：

| 原因 | 说明 | 处理 |
|---|---|---|
| `clock skew: device time …, server time …` | 设备时钟偏了。Orange Pi One **没有电池供电的 RTC**，断电重启后要等 NTP 校准，本地化部署常常连不上外网 NTP；手工 `date -s` 时把北京时间当 UTC 设也会差 8 小时 | 代理按服务端时间签名，不受影响（见下） |
| `bad signature` | 设备密钥与服务端登记的不一致 | 见下一节"另一把密钥" |
| `unknown device` | 设备在后台被删除了 | 代理会自动重新注册 |

代理从服务端每个响应的 `Date` 头学到时钟偏差，签名时按服务端时间来，**认证不依赖设备时钟**；
偏差超过 1 分钟会在日志里提示一次。

想让设备日志时间也准，见第 9 节"无外网运行"里的局域网 NTP 配置（只影响日志时间，不影响播放与认证）。

#### 设备报"在服务端登记的是另一把密钥"

说明设备手上的密钥与服务端记录的不一样，通常是 `/var/lib/display-agent/identity.json` 丢了：
重装系统、换了 SD 卡、手工删过这个目录、或者母镜像清理后没在后台删掉样机。设备会继续播放
本地缓存，同时把新密钥报给服务端。

处理：管理后台该设备上会出现红色提示 →【核对并处理】→ 对照硬件序列号 / MAC，以及设备日志里
`agent: new identity … key=xxxxxxxx` 的指纹 → 一致就点**接受新密钥**。设备一分钟内重新注册成功，
**属性、播放列表、专属模板都保留**（不要用"删除设备"来解决，那会把这些配置一起删掉）。
不一致说明是另一台机器撞了编号（给其中一台改主机名）或有人冒充，点忽略。

设备日志里每次启动都会打印身份来源，便于判断：

```
agent: using existing identity device_id=scr-0017 key=a80470cb (/var/lib/display-agent/identity.json)   # 正常
agent: new identity device_id=scr-0017 key=d9e2ca41 (…)                                                # 首次启动，或身份文件丢了
```

代理已做的防护：身份文件写入后 fsync（防断电丢失）；文件损坏时另存为 `identity.json.bad-<时间>`
留作证据而不是悄悄覆盖；同一缓存目录只允许一个代理进程（服务在跑时再手工启动一个会直接报错退出）；
`agent.json` 里的相对路径按配置文件所在目录解析（不会因为启动方式不同而用到两份身份文件）。

### 4.2 制作母镜像

先按第 3 节把一台样机完整装好并验证通过，然后清理成"出厂状态"：

```sh
systemctl enable display-agent    # install-agent.sh 不会自动 enable；母镜像里必须开着，否则烧出来的设备开机不播放
systemctl stop display-agent
rm -f /var/lib/display-agent/identity.json /var/lib/display-agent/current.json
rm -rf /var/lib/display-agent/media/* /var/lib/display-agent/status.json /var/lib/display-agent/gstplayer.py
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
4. 后台设属性（如 `room=302`）、在【内容】里上传要播的图片/视频 → 屏幕在一个轮询周期内更新；
5. 点【测试】确认是哪块屏。

## 5. 日常运维（管理后台）

### 5.1 内容

服务端首次启动会自动建好一个"左右分屏"模板（左边房间号、右边播放内容）并设为全局默认，
所以开箱就能用，不需要先去建模板。

- **设备**页每行【内容】：这台设备要显示什么都在这一个对话框里——
  - 模板：跟随全局，或给这台设备单独指定一个（可以"复制全局模板再改"）；
  - **左右对调**：同一个模板，把属性放到另一边。属性在左还是在右不需要建两个模板；
  - 播放内容：点选或把文件拖进上传框，选中的文件**立即出现在列表里**、各自显示上传进度，
    逐个上传；上传、转码过程中就可以拖动条目调整顺序（电脑上按住左侧把手拖；iPhone/iPad
    上按住把手直接拖，或在条目上长按"拿起"再拖，与系统列表一致；键盘焦点在把手上时可用 ↑↓）。
    **增删与排序即时生效**，不用另点保存；模板和左右对调要点【保存模板设置】。
    按顺序循环播放，图片停留时长跟着模板走，视频播完即切，一律静音；
    条目之间约 0.6 秒**淡出到黑再淡入**（设备端在模板图层上做，不需要重新编码，模板属性区不受影响）；
- **设备**页每行【属性】：`room=302` 之类的键值对，模板的 attribute 区域按 key 取值显示；
- **模板与时段**页：改模板（表单，或【JSON】做更复杂的版式）、【设为全局】、配置时段计划
  `{模板, 星期, 起止时间}`，支持跨午夜；无命中回落全局模板；
- 生效延迟 ≈ 服务端 `server.json` 的 `poll_interval_s`（默认 10 秒，304 轮询开销可忽略）。

**设备列表各列**：

| 列 | 内容 | 多久更新 |
|---|---|---|
| 设备 | 编号、主机名/IP、程序版本，以及**解码方式 · SoC 温度 · 实际输出分辨率**（≥80°C 标红，输出不是 1440×900 标黄） | 设备每次心跳上报（`heartbeat_interval_s`，默认 60 秒） |
| 当前显示 | **等待刷新**（后台改了内容，设备还没来取）→ **正在刷新**（设备在下载新内容）→ **已显示最新内容**；离线设备显示"离线"。下面一行是内容来源与模板名；等待/正在刷新时再显示"设备 N 秒前联系过"，超过 2 个轮询周期没来就标黄"设备 N 秒未响应" | 设备每次轮询（`poll_interval_s`，默认 10 秒） |

页面本身每 10 秒自动刷新一次（打开对话框时暂停），所以看到的状态最多再晚 10 秒；"N 秒前联系过"每秒走字。

**在线/离线**：设备的任何请求（轮询、心跳、下载文件）都算联系过；约 3 个轮询周期（默认 30 秒）
没有任何请求就显示离线（轮询间隔由服务端规定，设备照办）。

**上传与转码**：

| | 处理 | 为什么 |
|---|---|---|
| 图片 | png/jpg ≤ 20MB；超过 1440×900 的**自动等比缩小** | 设备只有 1GB 内存，解码后的位图是 宽×高×4 字节；在服务端缩一次，所有设备都省 |
| 视频 | mp4/mov/mkv/webm ≤ 500MB，**一律转码**为 H.264（High@4.0，x264 `fastdecode`）、1440×900 以内、≤ 30fps、码率约 2.5Mbps（上限 4Mbps）、去掉音轨、faststart | 设备是软解（见 3.4），原片动辄 1080p、10~20Mbps，软解不动、硬撑就发热，到 85°C 降频、更高直接关机。H.265 原片也照收——反正会被转成 H.264 |

转码在**后台**进行：上传后立即返回，列表里显示"转码中 xx%"，完成后按你在列表里排好的位置加入播放
（未完成前不会下发给设备）；失败会显示原因，可删除重传。转码是串行的，同时上传多个视频会排队。

服务端控制台（`journalctl -u display-server -f`）记录的是**服务端自己在做什么**，不刷设备心跳。
服务端与设备端的日志一律是英文；后台界面上给运营方看的提示（如拒收原因）是中文：

```
upload started: device scr-0017, promo.mov (about 186.4MB)
upload done: device scr-0017, promo.mov (186.4MB in 21.3s), queued for transcoding as promo.mp4
transcode started: device scr-0017, promo.mp4 (source 186.4MB, 62s)
transcoding: device scr-0017, promo.mp4 50% (41s elapsed)
transcode done: device scr-0017, promo.mp4 (186.4MB -> 19.2MB in 1m22s), added to playlist
admin PUT /devices/scr-0017/media -> 200 (2ms)
new content pushed: device scr-0017, version 3f9a…, 4 file(s) + template overlay
device scr-0018 online (192.168.1.58, agent 1.3.0)
```

拒收（文件损坏、超限、ffmpeg 不可用）与转码失败也都会写明原因。

服务端需要装 `ffmpeg`（`apt install ffmpeg`，或在 `server.json` 里用 `ffmpeg_path` 写绝对路径），
并且要**以服务的运行用户能跑通**：`sudo -u display ffmpeg -version`。服务由 systemd 以 `display`
用户、精简 PATH 启动，snap 版（`/snap/bin`，系统用户没有家目录会运行失败）、装在 `/root` 或家目录下、
装在 `/opt` 等位置的 ffmpeg 在你的 shell 里能用，服务却用不了。不可用时后台顶部与
`journalctl -u display-server` 会写明是哪一种原因；装好后重启服务端（`systemctl restart display-server`）生效。
**没装时不能上传视频**（图片照常），管理后台顶部会醒目提示。未转码的原片码率控制不住，
下发到设备就是过热隐患，所以宁可当场拒收。

**模板**：至多有一个"播放内容"区域（设备只有一个视频图层）。
只剩一个模板时不能删除——系统始终需要一个全局默认模板；删掉当前的全局模板时，
全局会自动改指向剩下的模板，设备不会因此没有内容可显示。

### 5.2 现场定位

**设备**页每行【测试】→ 选 1/5/15 分钟：该屏全屏显示"测试"卡片（含设备编号与属性），到期自动恢复。
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
| 仅 OTA 代理即可 | 播放逻辑（含随代理分发的播放进程 `gstplayer.py`）、播放列表行为、清单新字段的解析、下载与缓存策略、心跳内容 |
| 还需同时更新服务端 | 清单生成、模板渲染、管理后台界面（服务端在机房，更新它不用去现场） |
| **OTA 改不了，需要 SSH** | `/etc/display-agent/agent.json`（OTA 只替换二进制）、systemd 单元、系统软件包（GStreamer、Python、内核、DRM 驱动） |

所以新增设备端配置项时，务必让"缺省值即可用"——否则这批设备就得逐台登录。

**播放能力现状**：模板媒体区里图文混排、视频播完自动切下一条、列表循环都已实测可用。

## 6. 服务端升级与数据迁移

服务端的全部可变状态只有两处：**`data_dir`** 和 **`media_root`**。自包含布局下它们都在
`/srv/display` 里，所以升级和搬迁都很直接。

| 路径 | 内容 | 要不要保留 |
|---|---|---|
| `data/state.json` | 设备（含自注册设备的密钥）、属性、模板、时段、全局设置、固件元数据、更新目标 | **必须** |
| `data/firmware/` | 上传的代理程序 | 建议（否则待下发的更新目标会失效） |
| `data/rendered/` | 模板/测试卡的渲染结果 | 不必，缺了会自动重新渲染 |
| `media/` | 各设备的播放内容 | **必须** |
| `server.json` | 配置（含两个口令） | **必须**。另外把 `.secrets/tokens.env` 也备份到构建机之外 |

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
> 所以**从一开始就用域名而不是 IP**（例如 `http://display.lan:9000`），
> 搬迁时只改 DNS 指向即可，设备无感。

### 6.3 定期备份

```sh
tar czf /backup/display-$(date +%F).tar.gz -C /srv display --exclude='display/data/rendered'
```

排除 `rendered/` 可以显著减小体积，它会按需重新生成。

## 7. 验机清单（每台设备交付前）

| 检查项 | 方法 |
|---|---|
| 分辨率 / 温度 | `check-display.sh` 全部通过；管理后台该设备显示 1440×900、温度正常（播放视频时也低于 80°C） |
| 开机自启 | `systemctl is-enabled display-agent` 为 enabled（安装脚本不会自动 enable） |
| 网络连通 | `curl -sI http://<服务器>:9000` 有响应 |
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

## 9. 无外网运行

安装过程可以联网（装 ffmpeg、字体、GStreamer 等），**安装完成后整个系统只需要局域网**：服务端、设备端、
管理后台都不访问外网——没有云服务、在线授权、CDN 或外部字体，管理后台的页面资源全部内嵌在服务端里。

### 9.1 已经处理掉的离线问题

| 情况 | 不处理会怎样 | 现在的做法 |
|---|---|---|
| 设备没有 RTC、连不上外网 NTP，时钟偏了 | 签名请求全部 401，设备收不到任何更新 | 设备按服务端响应里的时间签名，认证不依赖设备时钟 |
| 局域网 DNS 是路由器转发到运营商，外网一断域名就解析不了 | 设备找不到服务端 | 设备记住上次解析成功的地址（`cache_dir/server-addr`），解析失败时用它 |
| 服务端暂时连不上（服务器关机、交换机故障） | 注册重试的等待超过看门狗 90 秒，代理连同播放进程被杀，屏幕每隔一分半黑一下 | 等待期间持续喂看门狗；屏幕一直播放本地缓存 |
| 设备开机时网络还没就绪（网线没插、DHCP 拿不到地址） | 服务要等 network-online 超时（最长约 2 分钟），这期间黑屏 | 不再等网络，开机立即播放本地缓存，网络就绪后再注册、拉清单 |
| 服务端卡住或网络时断时续 | 一个请求能挂 10 分钟，看门狗把代理杀掉 | 清单/心跳/注册 30 秒超时；大文件下载按"60 秒没收到数据"判定停滞，下次从断点续传 |
| 环境变量里带着指向外网的代理 | 局域网请求被送去连不上的代理 | 设备端一律直连 |
| 服务器没装 tzdata 包，`server.json` 写了 `timezone` | 服务端拒绝启动 | 时区数据内嵌在服务端程序里 |

### 9.2 仍需注意的地方

- **服务器时钟**：时段计划、测试屏到期、定时下发程序都按服务器时间计算。服务器有主板 RTC，断网后
  时间仍会走，但会慢慢漂移，且没人察觉。管理后台每次打开都会拿浏览器所在电脑的时间比对，相差 2 分钟
  以上会在顶部提示。可在服务器上跑一个局域网 NTP 服务，让设备日志时间也一并准确（可选，不影响播放与认证）：
  ```sh
  # 服务器（安装时联网装好）：apt install chrony，然后在 /etc/chrony/chrony.conf 加
  allow 192.168.0.0/16
  local stratum 10
  # 设备：/etc/systemd/timesyncd.conf 的 [Time] 下写 NTP=<服务器IP>，然后 systemctl restart systemd-timesyncd
  ```
  服务器时间偏了且没有可用的时间源时，手工校准：`date -s "2026-10-02 09:30:00"` 后 `hwclock -w`。
- **server_url 用的域名**：设备**第一次注册时**必须能解析（局域网 DNS 静态记录，或在母镜像的
  `/etc/hosts` 里写死）。之后 DNS 失效会用缓存的地址；但服务器换了 IP 而 DNS 又不可用时，设备找不到新地址。
- **安装完成后不能再装软件**：ffmpeg、CJK 字体、GStreamer 要在安装时装好（`install-agent.sh`
  已包含设备端所需的全部软件包）。
- **程序升级**：在一台能联网的机器上 `make package`（或下载 Release），把二进制拷到局域网里的电脑，
  再从管理后台上传、下发即可——OTA 只在局域网内进行。

## 10. 当前已知简化

- 图片展示时长写在**模板**里，同一份清单里的图片共用一个时长；单张静态图一直显示，不会周期性重载；
- 模板里的"播放内容"区域靠两个硬件图层叠加（见架构文档 5.2）。本地用真实 GStreamer 验证了排期、
  淡入淡出与解码器上报（fakesink 代替显示、软解代替 cedrus），**样机上必须确认：图层叠加与透明洞、
  cedrus 硬解（后台显示"硬解 v4l2slh264dec"）、温度，再对整批设备下发 OTA**；
- 清单更新时先把当前内容淡出再换（"播完当前项再切"留待优化）；
- SoC 硬件看门狗（`/dev/watchdog`）与只读根文件系统在 M4 实现；当前已有 systemd 软看门狗
  （进程假死 90s 内重启），以及播放进程守护（退出或 10 秒 ping 不回就重启，并重新下发当前画面）。
