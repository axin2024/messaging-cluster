# 限流与诊断

## 发送配额

默认每个租户每秒可以提交 250 条新消息。MySQL 按服务端秒分桶，同一租户下的所有应用、HTTP 接口和网关共享配额。重放已经提交的同一设备 `client_id` 返回原回执，写入失败会回滚本次配额占用。运维可为现有租户设置 1 到 10000 之间的配额。

```sh
go run ./cmd/messaging set-tenant-rate harbor 500
```

超过配额时，HTTP 返回 `429 tenant_rate_limited` 和 `Retry-After: 1`，WebSocket 返回 `tenant_rate_limited` 错误。客户端可以等待后保持原 `client_id` 重试。收到 `message_unconfirmed` 或发送后连接断开时，同样使用原标识查询或重放，直到获得该笔消息的持久回执。[会话与消息 API](conversations.md)

## 应用诊断

使用应用管理令牌查询以下接口。数据范围由令牌绑定的租户和应用确定。

| 接口 | 返回内容 |
|---|---|
| `GET /v1/admin/diagnostics?window_minutes=15` | 租户配额，应用在窗口内提交的消息和各阶段通知计数，待路由事件数与最大等待时间，以及相关网关的租约、活跃连接、积压、重连和心跳超时统计。支持 1 至 1440 分钟，默认为 15 分钟。 |
| `GET /v1/admin/conversations/{id}/messages/{seq}/delivery` | 消息落库、完成路由、首次写入连接和首次确认的时间，以及各投递状态的计数。使用持久回执中的会话 ID 和序号查询。 |

从 `pending_outbox` 和 `oldest_outbox_ms` 可判断是否积压在路由前。`queued` 表示已匹配在线设备，`offered` 表示网关已领取，`sent` 表示帧已写入连接，`acked` 表示客户端已确认通知，`resync` 表示该次实时投递已结束，应通过同步 API 补齐。连接拥塞、失效或通知超时都可能触发补拉。设备同步位置与阅读位置分别查业务 API。[长连接协议](gateway.md) · [同步 API](sync.md)

从数据库版本 5 升级时，已有消息保留原序号，可继续通过同步读取。旧消息的诊断返回 `live_delivery: false`，升级后的事件进入实时路由。

## 运行集成验证

在 Windows 上，下面的脚本用 MySQL 8.4 的 `bin` 目录启动仅监听回环地址的临时实例，执行集成测试和独立服务启动检查。运行结束后会停止测试进程，日志保存在 `storage/mysql-*` 下。

```powershell
pwsh -File ./scripts/test-mysql.ps1 -MySQLBin 'C:\mysql\bin'
```

给脚本传入 `-PreviousBinary` 可额外演练升级。可使用 `v1.0.0`（schema 6）构建的二进制，或使用提交 `d29cab8`（schema 5）测试跨版本迁移。脚本先运行旧服务创建账号、消息与群组，再启动升级后的服务，通过原设备令牌验证历史、重放、后续发送和新群成员操作记录。

加上 `-VerifyRestore` 可启动两个独立网关，以同一私有附件卷完成跨进程下载，再将数据库和附件备份恢复到独立路径。使用原设备令牌验证历史、同步游标、已读状态、附件摘要、已执行批次的原回执和待执行批次的恢复。[部署与备份](deployment.md)
