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

使用当前设备令牌调用 `GET /v1/devices/{id}/presence` 可查看自己的设备是否在线。节点租约过期、设备凭据撤销或失效时，在线状态随数据库记录更新。`GET /v1/devices/{id}/connections` 按代数倒序返回同一设备的连接记录，每页最多 50 条，用 `before` 传入响应中的 `next_before` 继续查询。消息游标和重连补拉在离线同步里程碑中交付。
