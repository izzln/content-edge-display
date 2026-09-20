# 服务端安装包

本包内含运营方本地服务器所需的全部文件。

```
display-server           服务端二进制
display-server.service   systemd 单元
server.example.json      配置样例
```

## 安装

```sh
tar xzf display-server-<版本>-<架构>.tar.gz && cd display-server-<版本>

# 1. 中文字体（模板与测试卡由服务端渲染，缺字体中文会变方框）
apt install -y fonts-noto-cjk

# 2. 二进制与配置
install -m 0755 display-server /usr/local/bin/
mkdir -p /etc/display-server /var/lib/display-server
cp server.example.json /etc/display-server/server.json
#   必改：admin_token（管理后台口令）、enroll_token（设备注册口令）、font_path

# 3. 服务
install -m 0644 display-server.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now display-server
```

管理后台：浏览器打开 `http://<服务器>:8080/admin`，输入 `admin_token`。

完整说明见仓库 `docs/deployment.md`。
