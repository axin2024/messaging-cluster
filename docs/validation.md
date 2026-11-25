# 验收用例与复现

测试使用合成租户、账号和消息。执行时须提供 MySQL 8.4 测试库。WebSocket 用例启动两个网关并通过实际回环连接交互。覆盖范围按[编号业务验收](requirements/04-验收与运营-prd.md)核对。

| 编号 | 可核对的结果 | 集成用例 |
| --- | --- | --- |
| AC-01 | 撤销手机设备后桌面连接仍能响应。 | `TestGatewayHandoffOnMySQL` |
| AC-02 | 其他租户带入目标租户字段仍读不到消息。 | `TestConversationsAndDurableMessagesOnMySQL` |
| AC-03 | 网关 A 下线后，设备在网关 B 从游标 40 补拉。 | `TestOfflineSyncAndReadAcrossGatewaysOnMySQL` |
| AC-04 | A 的迟到断开操作不影响 B 的当前连接。 | `TestGatewayHandoffOnMySQL` |
| AC-05 | 指定入群策略下，成员只读到加入后的序号 2、3。 | `TestConversationsAndDurableMessagesOnMySQL` |
| AC-06 | 退群设备请求离开后的新序号，服务返回无权。 | `TestConversationsAndDurableMessagesOnMySQL` |
| AC-07 | 同一发送标识跨网关重试或并发提交，返回相同序号。 | `TestConversationsAndDurableMessagesOnMySQL`、`TestOfflineSyncAndReadAcrossGatewaysOnMySQL` |
| AC-08 | 同一发送标识携带另一正文，服务返回冲突，原记录继续可用。 | `TestConversationsAndDurableMessagesOnMySQL` |
| AC-09 | 从 40 补拉 41、42，依次提交游标后再次拉取为空。 | `TestOfflineSyncAndReadAcrossGatewaysOnMySQL` |
| AC-10 | 保留策略只返回当前成员可见的过期区间。 | `TestRetentionReportsOnlyAuthorizedGapsOnMySQL` |
| AC-11 | 手机标记已读 42，桌面读取相同位置。 | `TestOfflineSyncAndReadAcrossGatewaysOnMySQL` |
| AC-12 | 提交过期已读位置 40 后，已读仍为 42，未读数保持。 | `TestOfflineSyncAndReadAcrossGatewaysOnMySQL` |
| AC-13 | 撤回保留原序号，清除历史中的附件引用，原读取凭据失效。 | `TestAttachmentAccessAndRetractionAcrossGatewaysOnMySQL` |
| AC-14 | 其他租户的文件无法用于本会话。 | `TestAttachmentAccessAndRetractionAcrossGatewaysOnMySQL` |
| AC-15 | 慢设备出现积压并改用补拉，健康设备仍逐条收件和确认。 | `TestRealtimeDeliveryAndSlowPeerIsolationOnMySQL` |
| AC-16 | MySQL 写入未完成时返回未确认，原标识重试只产生一笔记录。 | `TestUnconfirmedMessageDoesNotProduceReceiptOnMySQL` |

另外验证各应用和网关共享同租户配额，不同租户可以独立发送，已提交标识的重放不重复占用配额。测试文件位于 [`internal/httpapi`](../internal/httpapi)。

## 容量演练

`TestCapacityOnMySQL` 的 `smoke` 模式建立 40 条连接，持续负载 10 秒。`short` 和 `full` 都建立 1000 条连接，预热 10 秒，再用 16 个 HTTP 客户端工作协程以目标每秒 100 条短消息分别持续 60 秒和 600 秒。消息分散在一对一会话中，WebSocket 客户端在另一端确认通知。测试逐条核对持久回执、同步序号、客户端消息标识与正文，并记录请求异常和连接降级数。

稳定窗口结束后暂停 A，保持 B 可写，使受影响的设备用原令牌重连 B。测试在断开期间再发送一条消息，用原读者的持久游标取回，并报告首台设备重新接入且补拉成功和全部受影响设备重连的耗时。该演练的两个网关位于同一测试进程。部署与数据库备份恢复另用独立网关进程验证。

```sh
MESSAGING_CAPACITY_TEST=full go test -count=1 -timeout 20m -run '^TestCapacityOnMySQL$' -v ./internal/httpapi
```

Windows 脚本可以为当前次运行启动新的 MySQL 实例。

```powershell
$env:MESSAGING_CAPACITY_TEST = 'full'
pwsh -File ./scripts/test-mysql.ps1 -MySQLBin 'C:\mysql\bin'
Remove-Item Env:MESSAGING_CAPACITY_TEST
```

### 2026-11-25 正式实测

测试使用单台 Windows amd64 主机、Intel Core Ultra 7 155H（22 个逻辑处理器）、Go 1.26.0 和 MySQL 8.4.13。两个网关各持 500 条 WebSocket 连接，共用上限为 32 的 Go 数据库连接池。MySQL 的连接上限为 48，InnoDB 缓冲池为 32 MiB。负载包含 16 个发送工作协程、53 字节正文和每条一端接收的通知。

| 阶段 | 提交 / 持久确认 | 实测耗时 | 实测吞吐 | 回执 p50 / p95 / p99 |
| --- | ---: | ---: | ---: | --- |
| 10 秒预热 | 1,000 / 1,000 | 10.009 秒 | 99.91 条/秒 | 12.748 / 41.104 / 49.060 毫秒 |
| 600 秒稳定负载 | 60,000 / 60,000 | 600.092 秒 | 99.98 条/秒 | 18.369 / 129.025 / 218.454 毫秒 |

每个稳定窗口请求按每秒 100 条计划发出，最大发起滞后 551 毫秒。请求错误、意外断开和降级连接均为 0。两个阶段共收到并确认 61,000 条通知，逐条读回 61,000 条已确认消息，缺失为 0。稳定窗口结束时，Go 测试进程的已分配堆为 40.6 MiB，运行时 `Sys` 为 156.8 MiB，含压测客户端和两个网关。关闭 A 后，首台设备 421 毫秒恢复连接并核对补拉消息，500 台受影响设备在 4.974 秒内串行完成重连。

上述数据对应单主机回环压力。两个独立网关进程的服务验证见下节。

## 独立部署验证

2026-11-25 在 Windows amd64、Go 1.26.0、MySQL 8.4.13 上从空库执行迁移、应用初始化和登录。九个 MySQL 集成用例全部通过。以旧二进制 `d29cab8` 写入 schema 5，再升级至 schema 6，使用原设备令牌读回历史。同一消息标识重试仍返回序号 1，新消息使用序号 2。

在群聊中提交第 41 条消息后关闭网关，发送端未读取原回执。重试返回同一序号，手机在第二网关从 40 依次取回 41、42。手机标为已读后，桌面读到位置 42，桌面的旧请求未降低该位置。手机退群后读取新消息 45 被拒，补拉仅返回离群前可见的 43、44。

两个独立网关进程共享一个数据库和私有附件卷。向 A 提交消息，在 B 读取并写入同步与已读位置。停写后备份数据库和附件，导入空库并从新目录启动恢复节点。使用原设备令牌读回消息、序号和附件引用，同步及已读位置保持在 1。恢复前后通过授权下载得到同一 SHA-256，恢复后的新消息使用序号 2。可用 `scripts/test-mysql.ps1` 的 `-VerifyRestore` 复现。

在 Chrome 158 使用三个合成账号和四个客户端标签页实测浏览器演示。Alice 建群、添加 Bob 并发送带附件的消息。Bob 的桌面端在另一网关收到实时通知，下载的文件与原件 SHA-256 相同。桌面标为已读后，Bob 的手机端读到同一已读位置。Carol 的会话列表中没有该群。Alice 撤回后，Bob 的桌面端看到原序号上的撤回状态，下载入口随之消失。
