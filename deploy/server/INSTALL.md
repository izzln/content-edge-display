# 服务端安装包

```
display-server           服务端二进制
display-server.service   systemd 单元
server.example.json      配置样例
```

## 安装（自包含目录布局）

二进制、配置、内容、状态、字体全放在一个目录里，整个目录拷走即可搬迁或备份。
配置里的**相对路径按 server.json 所在目录解析**，与进程工作目录无关，
所以 systemd 启动（工作目录是 `/`）也不会把数据写到别处。

```sh
tar xzf display-server-<版本>-<架构>.tar.gz && cd display-server-<版本>

install -d /srv/display/fonts
install -m 0755 display-server /srv/display/
cp server.example.json /srv/display/server.json

# 中文字体：模板与测试卡由服务端渲染，缺字体中文会变方框
apt install -y fonts-noto-cjk
cp /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc /srv/display/fonts/

# 必改 server.json：admin_token（后台口令）、enroll_token（设备注册口令）
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
  media/<设备ID>/     目录轮播模式要播的图片与视频（运营方放）
  fonts/              渲染用字体
  data/               服务端状态：state.json、uploads/、firmware/、rendered/
```

管理后台：浏览器打开 `http://<服务器>:8080/admin`，输入 `admin_token`。

> 偏好 FHS 布局（二进制 `/usr/local/bin`、配置 `/etc`、数据 `/var/lib`）也可以：
> 在 server.json 里写绝对路径，并相应改 `display-server.service` 的 ExecStart。

完整说明见仓库 `docs/deployment.md`。
