# 离线同步与已读

设备从任意网关读取同一份 MySQL 状态。每个请求使用 `Authorization: Bearer <设备令牌>`。

| 请求 | 结果 |
|---|---|
| `GET /v1/inbox` | 按会话标识分页返回信箱。`high_seq` 是账号最后有权查看的消息序号，`sync_seq` 是当前设备确认的同步位置，`read_seq` 是账号共享的已读位置。 |
| `GET /v1/conversations/{id}/sync` | 从设备确认的位置续拉，支持用 `after` 指定起点。`limit` 默认 50，上限 100。 |
| `PUT /v1/conversations/{id}/sync` | 提交 `{"seq":42}` 确认当前设备已存好的位置。其它设备保留各自的同步位置。 |
| `GET /v1/conversations/{id}/read` | 查询账号共享的 `read_seq` 和当前未读数量。本人发送的消息不计入未读。 |
| `PUT /v1/conversations/{id}/read` | 提交 `{"seq":42}` 推进已读位置，返回当前账号共享的水位和未读数量。较旧的提交保持已有位置。 |

一次拉取返回 `items`、`next_after`、`has_more`、`visible` 和 `expired`。区间端点均包含在内。`visible` 表示成员授权覆盖的序号范围，`expired` 表示该范围内位于当前游标之后且正文已过期的缺口。存在可读取消息时，`earliest_available` 返回其中最早的序号。入群前及退群期间无权查看的区间从授权范围和过期缺口中排除。

重连后先查信箱，再从 `sync_seq` 补拉到 `high_seq`。客户端按会话和序号去重，保存正文、缺口及返回的 `next_after`，随后才确认同步。确认丢失时服务器可再次返回同一页。`has_more` 为真时，客户端用 `next_after` 继续拉下一页。确认同步和标记已读分别提交，二者都只允许向前推进到成员可见的序号。

## 保留期清理

应用管理员使用应用管理令牌向 `POST /v1/admin/conversations/{id}/retention` 提交 RFC 3339 时间，例如 `{"before":"2025-01-01T00:00:00Z"}`。运行清理时可以按业务保留期计算该截止时间，批次从最早保留的消息开始清空更早的正文，每次最多处理 1000 条。`has_more` 为真时以同一个截止时间继续调用。未发生变更的重试不增加审计记录。

`GET /v1/admin/conversations/{id}/retention` 按 `after` 分页返回实际应用的时间、截止时间及序号区间，可用于导出执行记录。过期消息仍保留序号和去重元数据。原设备核对发送结果会得到 `expired: true` 和空正文。
