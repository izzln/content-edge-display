# 设备端安装包

本包内含装一台显示屏设备所需的全部文件，解开后在设备上（root）执行即可，无需其他文件。

```
display-agent-armv7      设备代理二进制（ARMv7，静态编译）
enroll-token             设备自注册口令，与服务端包里的 server.json 匹配
install-agent.sh         安装脚本：建立 OTA 布局、写配置、装 systemd 单元、固定 1440×900
check-display.sh         现场自检：输出分辨率、硬件解码、SoC 温度
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

# 2. 安装代理（注册口令默认取包内 enroll-token，无需手工填）
#    server_url 写死在设备上、OTA 改不了，所以请用域名而不是 IP
#    每次执行都会重写 /etc/display-agent/agent.json；装完**不会**自动设为开机自启
SERVER_URL=http://display.lan:9000 ./install-agent.sh

# 3. 重启让 HDMI 模式生效，然后手工起一次看效果
reboot
systemctl start display-agent
journalctl -u display-agent -n 20     # 应看到 registered / playlist confirmed loaded

# 4. 自检：输出分辨率是否 1440x900、是否在硬解、温度是否正常
/usr/local/lib/display-agent/check-display.sh

# 5. 确认画面无误后再设为开机自启
systemctl enable display-agent
```

### 分辨率不对（画面错位）

面板 EDID 报的分辨率优先级高于默认设置，必须在内核命令行里显式指定，
`install-agent.sh` 已经把下面这行写进 `/boot/armbianEnv.txt` 的 `extraargs`：

```
video=HDMI-A-1:1440x900@60
```

**改完要重启才生效。** 如果重启后 `check-display.sh` 仍报别的分辨率，多半是这块 HDMI
驱动板的 EDID 里压根没有 1440x900 这个模式，内核就忽略了该参数——加 `,e` 强制输出：

```sh
HDMI_FORCE=e ./install-agent.sh && reboot
```

实在不行就让模板跟着屏幕走：在管理后台把模板画布改成屏幕的实际分辨率。
叠加图本来就会按设备实际输出分辨率缩放，不会错位，只是非等比时会有轻微形变。

设备会自动获得唯一编号并向服务端注册，1~2 分钟内出现在管理后台设备列表中。

之后升级代理程序**不需要再登录设备**：在管理后台"程序更新"页上传新版本二进制并下发即可
（本包内的 `display-agent-armv7` 就是可上传的文件）。

完整说明见仓库 `docs/deployment.md`。
