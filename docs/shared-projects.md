# 跨组织协作空间

采购方与供应商保留各自的租户、应用和设备身份。双方使用同一套自托管网关。系统通过项目邀请把两边的应用加入同一合作关系，主持方可以另外保留内部频道。

## 跑一遍采购协作

1. 初始化 MySQL 后运行 `messaging migrate`，分别运行 `messaging create-app harbor dispatch` 和 `messaging create-app supplier dispatch`。记录各自的组织 ID 和应用管理令牌。两边分别通过 `POST /v1/admin/users` 创建采购员和供应商账号，记录用户 ID。
2. 设置 `MESSAGING_DEMO_UI=1` 后启动网关，打开 `/demo/project.html`。用采购方应用管理令牌连接管理端。创建空间，向 `supplier/dispatch` 发邀请，核对签认截止、合作到期、席位、写入权限和消息保留天数。
3. 用供应商应用管理令牌打开第二个标签页并签认邀请。如果主持方提出共享加入之前的历史，供应商要主动同意。采购方刷新关系，建立共享频道，用不同的可见范围创建内部频道。双方管理员各自给本组织用户分配席位。
4. 两个用户分别在项目成员页登录。在共享频道查看接收组织和历史范围，可以发消息、上传附件、回复、引用、按职责选提醒对象、搜索。已登录设备使用独立设备键。服务器会在重新连接时校验当前授权。
5. 管理员从席位页面核对用户版本和上次成功鉴权。当发现访客不应再访问时，主持方可紧急撤权。关停整个项目之前，两边可以各自预览和下载自己发出的消息归档，核对 SHA-256。关闭合作后，可继续查各自的审计和任务元数据。

本地回环地址和 HTTPS 支持浏览器附件摘要校验。启用第二个节点时，两边的 DSN 和私有附件目录指向相同存储，HTTP 端口和节点名保持独立。已有租户通过 CLI 启用协作，见[升级步骤](deployment.md#升级)。

## 接口

管理员调用使用应用管理令牌，成员调用使用设备会话令牌。两者都通过 `Authorization: Bearer <token>` 传递。写入接口中的 `Idempotency-Key` 用于基于同一请求幂等重试。

| 功能 | 接口 |
| --- | --- |
| 开空间、邀请、签认 | `POST /v1/admin/shared-spaces`、`POST /v1/admin/shared-spaces/{space}/invitations`、`POST /shared-spaces/{space}/partners/{org}/accept` |
| 合作关系、状态、审计 | `GET /v1/admin/partner-links`、`PUT /v1/admin/partner-links/{link}/state`、`GET /v1/admin/partner-links/{link}/audit` |
| 席位和频道管理 | `GET/POST /v1/admin/partner-links/{link}/grants`、`POST /v1/admin/partner-links/{link}/channels` |
| 项目和频道目录 | `GET /v1/shared-projects`、`GET /v1/partner-links/{link}/channels` |
| 频道消息和讨论 | `GET/POST /v1/shared-channels/{channel}/messages`、`GET /v1/shared-channels/{channel}/threads/{root}` |
| 搜索和提醒 | `GET /v1/partner-links/{link}/search?q=keyword`、`GET /v1/shared-channels/{channel}/mention-candidates?side=partner&duty_role=dispatch`、`GET /v1/shared-channels/{channel}/mentions` |
| 发布历史正文 | `GET /v1/shared-channels/{channel}/publication-preview?group_id=12&source_seq=3`、`POST /v1/shared-channels/{channel}/publications` |
| 共享资料 | `POST /v1/shared-channels/{channel}/messages/{seq}/attachments/{attachment}/grant`，然后 `GET /v1/shared-attachment-content`，带上 `X-Attachment-Token` |
| 归档 | `GET /v1/admin/partner-links/{link}/export-preview`，`POST /v1/admin/partner-links/{link}/exports`，按 `Location` 查询任务 |
| 删除本组织消息 | `DELETE /v1/admin/partner-links/{link}/channels/{channel}/messages/{seq}` |

签认的最小范围包含 `project:read`。允许写入时加上 `thread:write`。消息保留为 1 到 3,650 天，设为 0 时以合作期限限制在线访问。接受方回传邀请中的保留天数。以下为一份两边约定保留 30 天的合成签认请求。

```json
{
  "invitation_id": 9,
  "policy_rev": 1,
  "scope": ["project:read", "thread:write"],
  "share_history": false,
  "retention_days": 30
}
```

成功返回如下内容。邀请过期返回 410，作用域越界或已撤权资源返回 404，签认版本或请求内容冲突返回 409。

```json
{
  "link_id": 6,
  "invitation_id": 9,
  "space_id": 4,
  "grant_epoch": 1,
  "state": "active",
  "scope": ["project:read", "thread:write"],
  "share_history": false,
  "retention_days": 30,
  "expires_at": "2027-04-01T00:00:00Z"
}
```

归档任务生成本组织发出的可见消息及附件摘要清单，每段最多 200 条。预览返回 `item_count` 和 `snapshot_project_seq`，提交申请时携带该快照序号。下载前先向任务地址的 `/grant` 申请票据，再带 `X-Export-Token` 从 `/content` 下载。响应头 `X-Archive-SHA256` 对应原始 JSON 字节。归档最长有效 24 小时，并受合作、撤权和消息保留期限制。归档失败可从原任务地址的 `/retry` 重试，归档分段需携带原 `snapshot_project_seq` 与前段的 `through_project_seq`。

## 运行边界

- 新账户的时间线默认从加入后的项目序号读起。内部频道由主持方读取。共享频道要开放加入前的历史，需要双方签认。
- 冻结期间可继续读取有权访问的内容，共享写入暂停。双方只能解除各自发起的冻结。关闭后即时退出，重新合作要签发新的合作关系。
- 删除和保留期到达时，读取路径立即过滤消息，后台分批清理检索和到期正文。项目附件票据跟随原消息权限，已下载的文件由接收方管理。原附件也可能被普通群消息引用。
- 普通群正文发布独立保存目标消息和来源审计。需要共享原附件时，由有权人员选定文件并重新附加。普通群的参与者继续按原权限访问。
- 搜索使用当前空间内的字面子串匹配。项目工作台通过 HTTP 刷新频道和补拉，普通群继续使用原 WebSocket 网关。跨实例同步和存储高可用由后续方案及部署者处理。
