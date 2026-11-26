# 自托管部署

在同一个 MySQL 8.4 实例中创建专用数据库和专用账号。构建服务后，先运行迁移，再部署两个网关进程。两个进程使用相同的数据库和私有附件目录，节点名称与 HTTP 端口各自独立。

```sh
go build -o ./bin/messaging ./cmd/messaging
./bin/messaging migrate
./bin/messaging create-app harbor dispatch
```

将 `create-app` 输出的应用管理令牌存入服务端密钥存储。用户设备令牌由会话接口签发，浏览器使用一次性票据接入。公开环境通过 HTTPS 提供接口，反向代理需转发 WebSocket 握手并允许长连接。反向代理与网关的网络权限应限制在部署环境内。[身份 API](identity.md) · [网关协议](gateway.md)

| 环境变量 | 用途 | 默认值 |
| --- | --- | --- |
| `MESSAGING_MYSQL_DSN` | Go MySQL 驱动 DSN，包含数据库名。跨主机访问时指定部署环境的 TLS。 | 必填 |
| `MESSAGING_HTTP_ADDR` | 网关 HTTP 监听地址。 | `127.0.0.1:8080` |
| `MESSAGING_NODE_NAME` | 节点日志与诊断名称，使用小写键名。 | `gateway` |
| `MESSAGING_ATTACHMENT_DIR` | 网关独占写权限的私有附件目录。通过多个网关服务时，挂载可共享的同一存储。 | `storage/attachments` |
| `MESSAGING_DEMO_UI` | 设置为 `1` 时提供同源 `/demo/` 浏览器客户端。 | 关闭 |

```sh
MESSAGING_NODE_NAME=gateway-a MESSAGING_HTTP_ADDR=127.0.0.1:8080 ./bin/messaging serve
MESSAGING_NODE_NAME=gateway-b MESSAGING_HTTP_ADDR=127.0.0.1:8081 ./bin/messaging serve
```

在两个终端运行以上命令。`/healthz` 检查 HTTP 进程，`/readyz` 在数据库版本匹配且网关持有有效租约时返回 HTTP 200。连接上限为每进程 4096，数据库池上限为每进程 32。MySQL 连接规划应覆盖全部网关、管理客户端和运维连接。

## 升级

切断写流量并停止全部网关。备份数据库、附件卷和部署配置后，使用新二进制执行一次 `migrate`。确认返回预期版本后启动全部网关，并用旧设备令牌、历史序号和原消息请求标识做读回。变更前保留旧二进制及与旧 schema 匹配的备份。

数据库版本 5 到 6 通过向前迁移。已有文本、附件引用、设备令牌与消息幂等标识沿用原值。启用版本 6 后的新事件参与实时投递。回滚时恢复升级前成对的数据库与附件快照，并使用匹配版本的二进制启动。[升级演练](operations.md)

`v1.0.0` 使用 schema 6。升级至 `v1.1.0` 时迁移返回 `schema_version: 7`，新增群成员操作表。旧群成员、角色、可见区间和历史消息保留，新的成员变更开始记录操作者。升级后用旧群主凭据变更一项测试角色，并回读新操作记录。

## 备份与恢复

同一次快照包含数据库和私有附件目录。停写期间生成数据库备份，记录当前提交、schema 版本、UTC 时间、备份文件摘要和所用附件卷。在独立存储保管并限制访问权限，备份可能含正文、凭据摘要和上传文件。

以下示例通过 MySQL 客户端交互输入密码。主机、账号、数据库和路径按本机配置替换。

```sh
mysqldump -h 127.0.0.1 -u backup_operator -p --single-transaction --hex-blob --no-tablespaces --set-gtid-purged=OFF messaging > messaging.sql
tar -C /srv/messaging -cf attachments.tar attachments
```

先将备份载入新建的空库，并解包到新挂载的私有目录。启动隔离网关时设置新的 DSN、附件路径和 HTTP 端口。在切回写流量前，校验历史消息序号和设备游标，用原设备令牌申请新的下载授权，下载已知样本并比对 SHA-256，再发一条消息验证序号继续递增。

使用群主或管理员账号核对一个已完成批次的原回执，以及一个尚未执行批次的版本和选集。在隔离网关用原操作者凭据执行未开始的备份样本，核对当前群成员以及操作记录数量。

```sh
mysql -h 127.0.0.1 -u restore_operator -p messaging_restore < messaging.sql
tar -C /srv/messaging-restore -xf attachments.tar
```

## 当前部署边界

网关共享 MySQL 单实例与同一附件卷。MySQL 自身的高可用、跨机房灾备和私有卷复制由部署者配置。实时通知为补拉提示，客户端需独立保存同步游标。租户限流使用数据库秒窗口，恢复时重新发起相同的消息标识可得到原回执。[限流与诊断](operations.md) · [同步 API](sync.md)
