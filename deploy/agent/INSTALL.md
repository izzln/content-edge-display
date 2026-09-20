# 设备端安装包

本包内含装一台显示屏设备所需的全部文件，解开后在设备上（root）执行即可，无需其他文件。

```
display-agent-armv7      设备代理二进制（ARMv7，静态编译）
install-agent.sh         安装脚本：建立 OTA 布局、写配置、装 systemd 单元、固定 1440×900
harden.sh                安全加固：关自动更新、nftables 入站白名单、SSH 仅密钥
rollback-check.sh        OTA 回滚检查（由 systemd ExecStartPre 调用）
display-agent.service    systemd 单元
agent.example.json       配置样例（install-agent.sh 会据此生成 /etc/display-agent/agent.json）
```

## 安装

```sh
tar xzf display-agent-<版本>-armv7.tar.gz && cd display-agent-<版本>

# 1. 安全加固（母镜像制作时执行一次；加固后请先另开一个终端确认密钥 SSH 能登录再断开）
SSH_ALLOW_FROM=<服务器IP> SSH_PUBKEY="ssh-ed25519 AAAA... ops" ./harden.sh

# 2. 安装代理（ENROLL_TOKEN 取自服务端 server.json 的同名字段）
SERVER_URL=http://<服务器IP>:8080 ENROLL_TOKEN=<enroll_token> ./install-agent.sh

# 3. 启动并确认
systemctl start display-agent
journalctl -u display-agent -n 20     # 应看到 registered / playlist confirmed loaded
```

设备会自动获得唯一编号并向服务端注册，1~2 分钟内出现在管理后台设备列表中。

之后升级代理程序**不需要再登录设备**：在管理后台"程序更新"页上传新版本二进制并下发即可
（本包内的 `display-agent-armv7` 就是可上传的文件）。

完整说明见仓库 `docs/deployment.md`。
