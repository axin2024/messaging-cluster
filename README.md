# 分布式即时通信平台

Messaging Cluster 是用 Go 和 MySQL 构建的自托管即时通信服务。项目的首个公开版本包含租户应用、长连接网关、私聊与群组、会话内排序、离线补拉、多端已读、附件治理和运维容量。完整范围见[产品总览](docs/requirements/01-产品总览-prd.md)及[业务功能](docs/requirements/02-业务与交互-prd.md)。

目前本地开发版本可运行租户应用、设备身份 API 和 WebSocket 网关。同一设备可以切换网关，账号的其他设备保持各自的连接。消息投递、离线补拉和容量验收列在后续里程碑中。

## 运行服务

准备 Go 1.26 和 MySQL 8.4。在 MySQL 中先建空库和该库的应用账号。设置 `MESSAGING_MYSQL_DSN` 为 MySQL 驱动的连接字符串，`MESSAGING_HTTP_ADDR` 默认为 `127.0.0.1:8080`。公开环境使用 HTTPS，数据库跨主机访问时按部署环境配置 TLS。

```sh
go run ./cmd/messaging migrate
go run ./cmd/messaging create-app harbor dispatch
go run ./cmd/messaging serve
```

创建应用的命令输出一次性提供的管理令牌。将令牌保存在服务端密钥配置中，用它调用 `POST /v1/admin/users` 创建用户。然后向 `POST /v1/sessions` 提交租户、应用、用户名、密码和设备键。返回的设备令牌可用于查询身份、轮换当前设备凭据和撤销同一账号的其他设备。

用不同的 `MESSAGING_NODE_NAME` 和 `MESSAGING_HTTP_ADDR` 启动第二个进程。两个进程使用同一个 `MESSAGING_MYSQL_DSN`，客户端可连接任一进程。

[身份与设备 API](docs/identity.md) · [长连接网关](docs/gateway.md) · [Apache License 2.0](LICENSE)
