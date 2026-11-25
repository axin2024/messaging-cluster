# 双网关演示

使用专用 MySQL 库和私有附件目录。按部署文档构建 `messaging`，执行 `migrate`，再用 `create-app harbor dispatch` 创建演示应用。启动两个网关，在两个进程的环境中设置 `MESSAGING_DEMO_UI=1`。示例使用本机 8080 和 8081 端口。[部署步骤](deployment.md)

把 `create-app` 输出中的 `admin_token` 传给 PowerShell 演示脚本。脚本创建 Alice、Bob 和 Carol 三个账号，返回每个用户的 ID 和随机生成的演示密码。在同一应用中仅运行一次。

```powershell
$env:MESSAGING_DEMO_ADMIN_TOKEN = Read-Host '演示应用管理令牌' -MaskInput
./examples/seed-demo.ps1
Remove-Item Env:MESSAGING_DEMO_ADMIN_TOKEN
```

## 体验实时消息和设备切换

1. 在 `http://127.0.0.1:8080/demo/` 用 Alice 登录 `desktop` 设备。在另一个浏览器标签用同一账号登录 `phone`。
2. 在 `http://127.0.0.1:8081/demo/` 用 Bob 登录。Alice 创建群聊并添加 Bob 的用户 ID。Bob 刷新会话列表后进入群聊。
3. Alice 发送文字和附件。Bob 收到实时提示后补拉同一序号，通过下载按钮申请短期读取授权。Alice 在手机标为已读，电脑应更新共享已读位置。
4. 停掉 8080 的网关。在 8081 的演示页用 Alice 和原设备键登录。页首显示新节点名称，打开原会话核对消息和设备游标。
5. Carol 登录后只有自己的会话。Alice 撤回刚发送的消息，在 Bob 的历史中确认该序号仍显示，下载按钮消失。

浏览器演示把令牌保存在页面内存，按会话重读可见历史并更新同步游标。它适用于少量会话和人工演练。用自动验收复现持久游标 `40 → 42`、发送回执丢失重试、退群后下载拒绝以及保留期缺口。[自动化验收](operations.md)
