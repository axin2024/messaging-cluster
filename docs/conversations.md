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

## 群成员批次与操作记录

表中的路径接在 `/v1/conversations/{id}` 后。批次仅向当前群主和管理员开放，实际执行权限在事务内从登录设备和当前群角色重新检查。

| 请求 | 数据与结果 |
|---|---|
| `POST /membership-batches` | 保存 1 至 30 个不同用户的有序操作。响应包含批次 `id`、会话 ID、操作者、设备、群角色、版本和逐项结果。初次保存返回版本 1，各项为 `not_started`。 |
| `GET /membership-batches/{client_id}` | 批次建立者在自己的任一授权设备上查原批次。 |
| `POST /membership-batches/{client_id}/execute` | 批次建立者执行已保存选集。响应为版本 2，各项为 `succeeded` 或 `failed`。失败原因包含 `forbidden` 和 `not_found`。 |
| `GET /membership-operations?before=123` | 当前群主和管理员按倒序读取本群操作记录。每页最多 20 条，用 `next_before` 读取更早的记录。首屏省略 `before`。 |

```json
{
  "client_id": "shift-roster-0830",
  "items": [
    {"user_id": 24, "action": "set", "role": "member"},
    {"user_id": 30, "action": "remove"}
  ]
}
```

同一群、同一账号的 `client_id` 对应同一份选集。相同重放读回原批次，改动选集或顺序返回 `409 client_id_payload_conflict`。执行时业务拒绝记录在该项，其余合法变更一并提交。事务失败时，整次变更回滚，查回的批次仍为版本 1。若执行回执丢失，先查询原标识，只有仍为版本 1 才需要执行。要重新尝试部分失败项，用新 `client_id` 保存新选集。

批次中对自己的变更会被拒绝。角色变更、退群和群主转让继续使用原接口，并在同一事务中记录实际操作者。旧版本的成员历史仍可用，从 schema 7 启用后开始积累操作记录。

## 发送与查阅文本消息

通过 `POST /v1/conversations/{id}/messages` 发送 `{"client_id":"desk-853","body":"桥吊已经就位"}`。`client_id` 由发送方生成，在同一发送账号和设备的所有会话中唯一。正文上限为 4000 UTF-8 字节。首次写入返回 `201`，对同一会话发送相同载荷时，重试返回 `200` 且 `duplicate` 为 `true`。同标识改正文或会话会返回 `409 client_id_payload_conflict`。

同一请求也可走 WebSocket。发送 `{"type":"message.send","request_id":"ui-31","conversation_id":42,"client_id":"desk-853","body":"桥吊已经就位"}`，提交成功后收到同 `request_id` 的 `message.accepted` 帧，`receipt` 包含消息与去重结果。消息、会话序号和分发意向在同一 MySQL 事务提交。连接在接收回执前中断时，重试需要保留原 `client_id` 和载荷。

原设备可用 `GET /v1/conversations/{id}/messages/by-client/{client_id}` 核对未知结果。

`GET /v1/conversations/{id}/messages?after=0&limit=50` 按序号返回可见消息。每次把响应的 `next_after` 保存为本地游标，`has_more` 为真时继续翻页。`GET /v1/conversations/{id}/messages/{seq}` 可读取单条消息。已参与会话的用户请求成员区间外的序号会得到 `403`，请求可见但已过期的序号会得到 `410 history_expired`。设备游标和缺口的细节见[离线同步与已读](sync.md)。

HTTP 和 WebSocket 发消息都接受 `attachment_ids`。发送附件、读取文件和同步撤回事件的细节见[附件与撤回](media.md)。
