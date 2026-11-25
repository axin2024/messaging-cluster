# 会话与消息 API

每个 HTTP 请求使用 `Authorization: Bearer <设备令牌>`。会话与成员必须属于令牌所绑定的租户和应用。路径中的 `{id}` 是会话标识，`{user}` 是用户标识。

## 私聊与群组

| 请求 | 数据与结果 |
|---|---|
| `POST /v1/conversations/direct` | 提交 `{"user_id":24}`。双方调换发起顺序时得到同一个会话。 |
| `POST /v1/conversations/group` | 提交 `{"title":"交接组","history_policy":"since_join"}`。创建者成为群主。 |
| `GET /v1/conversations` | 按会话标识分页列出曾参与的会话。 |
| `GET /v1/conversations/{id}/members` | 活跃成员可按用户标识分页读取当前成员。 |
| `PUT /v1/conversations/{id}/members/{user}` | 群主可添加成员或管理员。管理员可添加普通成员。请求体为 `{"role":"member"}` 或 `{"role":"admin"}`。 |
| `DELETE /v1/conversations/{id}/members/{user}` | 成员可退群。群主可移除其他成员，管理员可移除普通成员。 |
| `PUT /v1/conversations/{id}/owner` | 群主提交 `{"user_id":24}` 将群主角色转给活跃成员，原群主转为管理员。 |

`since_join` 策略从入群后的下一条消息起开放历史。退群时冻结可见上界，重新入群产生新的区间。`all` 策略在每次入群时从序号 1 开放历史，退群时同样冻结上界。群主需要先移交群主角色才能退群。成员和会话列表用 `after` 传入上页响应的 `next_after`。

## 发送与查阅文本消息

通过 `POST /v1/conversations/{id}/messages` 发送 `{"client_id":"desk-853","body":"桥吊已经就位"}`。`client_id` 由发送方生成，在同一发送账号和设备的所有会话中唯一。正文上限为 4000 UTF-8 字节。首次写入返回 `201`，对同一会话发送相同载荷时，重试返回 `200` 且 `duplicate` 为 `true`。同标识改正文或会话会返回 `409 client_id_payload_conflict`。

同一请求也可走 WebSocket。发送 `{"type":"message.send","request_id":"ui-31","conversation_id":42,"client_id":"desk-853","body":"桥吊已经就位"}`，提交成功后收到同 `request_id` 的 `message.accepted` 帧，`receipt` 包含消息与去重结果。消息、会话序号和分发意向在同一 MySQL 事务提交。连接在接收回执前中断时，重试需要保留原 `client_id` 和载荷。

原设备可用 `GET /v1/conversations/{id}/messages/by-client/{client_id}` 核对未知结果。

`GET /v1/conversations/{id}/messages?after=0&limit=50` 按序号返回可见消息。每次把响应的 `next_after` 保存为本地游标，`has_more` 为真时继续翻页。`GET /v1/conversations/{id}/messages/{seq}` 可读取单条消息。已参与会话的用户请求成员区间外的序号会得到 `403`。
