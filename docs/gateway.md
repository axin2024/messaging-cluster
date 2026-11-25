# 长连接网关

## 接入

客户端连接 `GET /v1/gateway`，声明子协议 `messaging.v1`。原生客户端在升级请求发送 `Authorization: Bearer <设备令牌>`。浏览器先用设备令牌调用 `POST /v1/gateway/tickets` 获取 30 秒有效的一次性票据，再将 `ticket.<票据>` 加入子协议列表。每张票据只能建立一次连接，浏览器连接使用和当前页面相同的来源。

```js
const response = await fetch("/v1/gateway/tickets", {
  method: "POST",
  headers: { Authorization: `Bearer ${deviceToken}` },
});
if (!response.ok) throw new Error(`Gateway ticket failed (${response.status})`);
const { ticket } = await response.json();
const gateway = new URL("/v1/gateway", window.location.href);
gateway.protocol = gateway.protocol === "https:" ? "wss:" : "ws:";
const socket = new WebSocket(gateway, ["messaging.v1", `ticket.${ticket}`]);
```

服务器返回 `ready` 帧，携带 `device_id`、`connection_epoch`、`node` 和 `connected_at`。客户端可以发送 `{"type":"ping","request_id":"health"}` 并收到带相同 `request_id` 的 `pong`。网关每 15 秒发一个 WebSocket 控制 ping，等待 5 秒后关闭未应答的连接。

## 重连与设备状态

每个网关进程取得独立的 MySQL 节点实例标识和 12 秒租约，每约 4 秒续约。使用同一设备重新接入时，数据库在同一事务中增加连接代数、结束旧连接记录并登记新连接。旧网关随后关闭旧连接。断开操作只影响发起断开的连接代数。同一账号的其他设备有独立代数。

使用当前设备令牌调用 `GET /v1/devices/{id}/presence` 可查看自己的设备是否在线。节点租约过期、设备凭据撤销或失效时，在线状态随数据库记录更新。`GET /v1/devices/{id}/connections` 按代数倒序返回同一设备的连接记录，每页最多 50 条，用 `before` 传入响应中的 `next_before` 继续查询。

WebSocket 客户端可发送 `message.send` 帧。服务器在消息事务中检查连接代数，接管后的旧连接无法继续写入。持久回执和历史游标详见[会话与消息 API](conversations.md)。

## 实时通知

`ready` 之后，服务器可异步发送下列事件。客户端按 `type` 分派，用 `request_id` 匹配 `message.accepted`、`pong` 和错误回执。

| 事件 | 客户端操作 |
|---|---|
| `notification.message` | 从已持久保存的设备游标拉取对应会话的新消息。 |
| `notification.read` | 重新读取对应会话的共享已读位置。 |
| `notification.retraction` | 拉取撤回记录，更新本地对应消息。 |
| `notification.resync_required` | 用信箱、会话同步和撤回记录补齐积压。重连后继续接收通知。 |

前三种事件带有 `delivery_id`、`conversation_id` 和 `seq`。收到后发送 `{"type":"notification.ack","delivery_id":123}`，成功时服务器继续处理后续帧。返回 `notification_unconfirmed` 时可以在当前连接上重试确认。业务游标仍需通过 `PUT /v1/conversations/{id}/sync` 更新，客户端按会话 ID 和序号合并拉取结果。

每条连接最多保留 16 条未确认提示。继续写入会使该连接收到一次 `notification.resync_required`，其余会话连接仍能收件。客户端新接入或重连后先读取信箱，拉取 MySQL 中已提交而本地尚未应用的记录。通知可以合并处理，展示顺序使用会话内序号。[离线同步与已读](sync.md)
