# 分布式即时通信平台

Messaging Cluster 是用 Go 和 MySQL 构建的自托管即时通信服务。项目的首个公开版本包含租户应用、长连接网关、私聊与群组、会话内排序、离线补拉、多端已读、附件治理和运维容量。完整范围见[产品总览](docs/requirements/01-产品总览-prd.md)及[业务功能](docs/requirements/02-业务与交互-prd.md)。

设备在线时，WebSocket 推送新消息、已读更新和撤回提示。离线或降级时，客户端用每台设备的同步游标续拉，账号级已读位置在多端共享。管理员可以检查通知积压和节点状态，运维可以按租户设置发送配额。

## 运行服务

准备 Go 1.26 和 MySQL 8.4。在 MySQL 中先建空库和该库的应用账号。设置 `MESSAGING_MYSQL_DSN` 为 MySQL 驱动的连接字符串，`MESSAGING_HTTP_ADDR` 默认为 `127.0.0.1:8080`。公开环境使用 HTTPS，数据库跨主机访问时按部署环境配置 TLS。

```sh
go run ./cmd/messaging migrate
go run ./cmd/messaging create-app harbor dispatch
go run ./cmd/messaging serve
```

创建应用的命令输出一次性提供的管理令牌。将令牌保存在服务端密钥配置中，用它调用 `POST /v1/admin/users` 创建用户。然后向 `POST /v1/sessions` 提交租户、应用、用户名、密码和设备键。返回的设备令牌可用于查询身份、轮换当前设备凭据和撤销同一账号的其他设备。

用不同的 `MESSAGING_NODE_NAME` 和 `MESSAGING_HTTP_ADDR` 启动第二个进程。两个进程使用同一个 `MESSAGING_MYSQL_DSN`，并将 `MESSAGING_ATTACHMENT_DIR` 指向同一个私有存储卷。单节点的附件目录默认为 `storage/attachments`。

[身份与设备 API](docs/identity.md) · [长连接网关](docs/gateway.md) · [会话与消息 API](docs/conversations.md) · [离线同步与已读](docs/sync.md) · [附件与撤回](docs/media.md) · [限流与诊断](docs/operations.md) · [Apache License 2.0](LICENSE)
