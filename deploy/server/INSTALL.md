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

#   server.json 里的 admin_token / enroll_token 已由 make 生成填好；
#   若是从 GitHub Releases 下载的包，里面是占位值 change-me，服务端会拒绝启动，
#   需在构建机上执行 make tokens 生成后替换。
#   media/ data/ 会在首次启动时自动创建

useradd -r -s /usr/sbin/nologin display 2>/dev/null || true
chown -R display:display /srv/display
install -m 0644 display-server.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now display-server
```

装好后目录长这样：

```
/srv/display/
  display-server      二进制
  server.json         配置
  media/<设备ID>/     该设备要播的图片与视频（后台上传，也可直接拷进来）
  fonts/              渲染用字体
  data/               服务端状态：state.json、firmware/、rendered/、incoming/（待转码原片）
```

管理后台：浏览器打开 `http://<服务器>:9000/admin`，输入 `admin_token`。

> 偏好 FHS 布局（二进制 `/usr/local/bin`、配置 `/etc`、数据 `/var/lib`）也可以：
> 在 server.json 里写绝对路径，并相应改 `display-server.service` 的 ExecStart。

完整说明见仓库 `docs/deployment.md`。
