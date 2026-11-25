# 附件与撤回

服务用 `MESSAGING_ATTACHMENT_DIR` 存放二进制数据，默认目录为 `storage/attachments`。多个网关应挂载同一块持久存储卷。将文件目录放在服务专用的私有路径，由应用检查权限后读取。备份和恢复时同时保留 MySQL 及附件存储卷。

## 上传和发送

1. 向 `POST /v1/attachments` 提交 `filename`、`content_type`、`size` 和 `sha256`。长度按字节计算，文件上限为 10 MiB，摘要用 64 位十六进制字符串。返回的 `id` 可用于上传。
2. 用同一账号向 `PUT /v1/attachments/{attachment}/content` 发送原始文件字节。通过长度和摘要校验后得到 `ready_at`。对已完成的上传重复调用返回已有记录。
3. 发送消息时传入 `attachment_ids`，最多 8 个。附件就绪时允许发送空正文。每则消息只可引用同租户、同应用内由发送者上传的就绪附件。重试消息时保留原客户端号、正文和附件顺序。

以上请求使用 `Authorization: Bearer <设备令牌>`。同一文件可在多条消息中引用，每条消息有各自的读取授权。

## 下载

用设备令牌调用 `POST /v1/conversations/{id}/messages/{seq}/attachments/{attachment}/grants`。返回的读取令牌有效期为 60 秒。随后调用 `GET /v1/attachment-content`，请求头使用 `Authorization: Attachment <读取令牌>`。支持 HTTP `Range`，同一凭据可在有效期内进行分段读取。

每次下载都检查消息、设备凭据代数、当前成员记录和消息可见序号。退群、设备撤销或轮换、撤回、消息过期都会使对应的旧令牌失效。重新加入后需要重新签发读取令牌。服务以附件方式返回字节，客户端负责决定是否打开文件。

## 撤回

活跃成员可对自己发送的可见消息调用 `DELETE /v1/conversations/{id}/messages/{seq}`。群主与管理员也可撤回群内可见消息。撤回保留原序号，清空正文和历史响应中的附件引用，返回包含操作者和时间的事件。重复撤回返回同一事件。其他仍可见的消息对同一文件的引用保持有效。

已经越过该序号的客户端用 `GET /v1/conversations/{id}/retractions?after={事件ID}` 补拉撤回事件，根据事件的 `seq` 替换本地消息并清理附件缓存。分页将返回的 `next_after` 设为新的事件游标。收到空页即完成当次补拉。

撤回和保留期操作收回文件下载权限，附件卷仍保存原有对象。文件卷的生命周期由部署方管理。
