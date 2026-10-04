# 服务端安装包

```
display-server           服务端二进制
display-server.service   systemd 单元
server.json              配置，两个口令已自动生成填好
```

## 安装（自包含目录布局）

二进制、配置、内容、状态、字体全放在一个目录里，整个目录拷走即可搬迁或备份。
配置里的**相对路径按 server.json 所在目录解析**，与进程工作目录无关，
所以 systemd 启动（工作目录是 `/`）也不会把数据写到别处。

```sh
tar xzf display-server-<版本>-<架构>.tar.gz && cd display-server-<版本>

install -d /srv/display/fonts
install -m 0755 display-server /srv/display/
cp server.json /srv/display/

# 中文字体：模板与测试卡由服务端渲染，缺字体中文会变方框
apt install -y fonts-noto-cjk
cp /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc /srv/display/fonts/

# 视频转码：上传的视频统一转成 1440×900 以内、≤4Mbps、30fps 的 H.264。
# 不装的话后台不能上传视频（未转码的原片码率过高，会让设备过热关机）。
apt install -y ffmpeg

# PDF：上传的 PDF 逐页渲染成图片轮播。不装的话后台不能上传 PDF。
apt install -y poppler-utils

#   server.json 里的 admin_token / enroll_token 已由 make 生成填好；
#   若是从 GitHub Releases 下载的包，里面是占位值 change-me，服务端会拒绝启动，
#   需在构建机上执行 make tokens 生成后替换。
#   data/ 会在首次启动时自动创建

useradd -r -s /usr/sbin/nologin display 2>/dev/null || true
sudo -u display ffmpeg -version | head -1   # 必须以服务的运行用户能跑通（见下方说明）
sudo -u display pdftoppm -v 2>&1 | head -1  # 同上
chown -R display:display /srv/display
install -m 0644 display-server.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now display-server
```

装好后目录长这样：

```
/srv/display/
  display-server      二进制
  server.json         配置
  fonts/              渲染用字体
  data/               服务端状态：state.json、cache.json、packages/（程序包）、rendered/、incoming/（待转码原片）、tls/（证书）
  data/media/<设备ID>/  该设备要播的图片、视频、PDF（后台上传，也可直接拷进来）
```

> **"明明装了 ffmpeg，后台却说不可用"**：服务由 systemd 以 `display` 用户启动，PATH 只有
> `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin`，**不是你登录 shell 的环境**。常见原因：
> snap 版 ffmpeg（在 `/snap/bin`，且系统用户没有家目录会运行失败）、装在 `/root` 或某个家目录下
> （`display` 用户无权读取）、装在 `/opt/...` 等不在上述 PATH 里的位置。
> 后台顶部和服务日志（`journalctl -u display-server`）会写明具体原因。推荐用 `apt install ffmpeg`
> （装到 `/usr/bin`）；装在别处就在 `server.json` 里写 `"ffmpeg_path": "/绝对路径/ffmpeg"`，
> 并确认 `sudo -u display /绝对路径/ffmpeg -version` 能跑通。装好后重启服务端（`systemctl restart display-server`）生效。

> 偏好 FHS 布局（二进制 `/usr/local/bin`、配置 `/etc`、数据 `/var/lib`）也可以：
> 在 server.json 里写绝对路径，并相应改 `display-server.service` 的 ExecStart。

## 端口与证书

| 端口 | 协议 | 用途 |
|---|---|---|
| 9001（`listen`） | HTTPS | 管理后台、设备通信 |
| 9000（`bootstrap_listen`） | HTTP | 只提供新设备一键装机脚本与程序包下载，其余请求 301 到 HTTPS |

防火墙两个端口都要放行。首次启动在 `data/tls/` 生成自签证书，日志里打印证书指纹
（`journalctl -u display-server | grep fingerprint`）。设备固定这个指纹，所以 **`data/tls/` 必须随 `state.json` 一起备份**——
丢了重新生成指纹就变了，已装设备全部拒绝连接。

管理后台：浏览器打开 `https://<服务器>:9001/admin`，输入 `admin_token`。浏览器会提示证书不受信任
（自签证书），选"高级 → 继续访问"；或把 `data/tls/server.crt` 导入管理电脑并设为信任。

## 新设备装机

先在后台「程序更新」页上传设备端程序包（`display-agent-<版本>-armv7.tar.gz`），然后在刷好公版 Armbian 的设备上以 root 运行
（这条命令在「程序更新」页可直接复制）：

```sh
curl -fsSL http://<服务器>:9000/install.sh | ENROLL_TOKEN=<server.json 里的 enroll_token> sh
```

完整说明见仓库 `docs/deployment.md`。
