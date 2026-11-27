# 跨组织协作验收

测试使用独立 MySQL 8.4 库、合成的采购方和供应商、私有附件目录。普通群与 WebSocket 的既有验收见[消息平台验收](validation.md)。

| 编号 | 可观察结果 |
| --- | --- |
| A1 | 两个网关并发处理同一签认请求，共享同一关系编号和初始代际，只写入一条签认审计。 |
| A2 | 已过期邀请返回 410，第三方拿到同一邀请编号也无法签认。 |
| A3 | 访客按加入时的项目序号读取，仅见加入后的三条消息。 |
| A4 | 访客直接申请加入前的附件，返回 404。 |
| A5 | 六名访客并发竞争五个席位，恰有五个成功。 |
| A6 | 撤回后旧设备游标无法继续读取。重新授权后也需要新游标。 |
| A7 | 另一组织按序读回根消息和三条回复。 |
| A8 | 引用内部频道或加入前未授权的消息，无法发布。 |
| A9 | 私有和共享消息含同一个关键词，访客只匹配共享范围。 |
| A10 | 保留索引副本时已删除的消息立即从查询过滤，后台随后清理索引。另校验根消息到期时线程、提醒与归档同时过滤。 |
| A11 | 供应商关闭合作后，采购方仍可查询本方审计和关闭代际。 |
| A12 | 撤权后未兑换的附件和旧归档票据都返回 404，旧归档任务状态失效。 |
| A13 | 浏览器复现附件准备下载时撤权，从三台设备核对下载、搜索和重连。MySQL 同时保留旧索引和未到期附件票据。 |
| A14 | 对 100 个组织、1,000 个空间运行实际 HTTP 请求与 MySQL 撤权。超过授权界线的内容读回为零。 |

A1 至 A12 主要由 `TestSharedInvitationsOnMySQL` 验证。`TestSharedPublicationAndMentionsOnMySQL` 补充了普通群逐条发布、职责提醒、双方保留期与归档预览。`TestSharedWorkbenchOnMySQL` 和 `TestSharedCapacityOnMySQL` 分别用于 A13 和 A14。

## A13 浏览器与数据库读回

2026-11-27，Windows amd64、Go 1.26.0、MySQL 8.4.13，在独立无头 Chrome 会话中打开两个不同网关地址。采购方通过工作台创建采购空间、签发邀请和授权，上传 `spec-browser.txt` 并发送正文 `procurement browser evidence`。供应商在三台合成设备登录并读取同一条消息。测试等待下载页拿到票据，暂停它向服务端发送文件请求，同时使第三台设备离线。

采购方管理员从工作台将访客授权版本 1 撤回。MySQL 写入撤权的时间为 `2026-11-27T04:28:41.895492Z`，浏览器收到成功回执的时间为 `04:28:41.906Z`。工单回执中的授权代际为 4，列出三台曾在代际 3 成功鉴权的设备。随后三台设备分别执行以下操作。

| 客户端操作 | 发起时间 UTC | 服务与页面结果 |
| --- | --- | --- |
| 发送已拿到票据的附件请求 | 04:28:41.945 | HTTP 404，页面没有下载事件。 |
| 重新搜索 `procurement` | 04:28:41.959 | HTTP 404，搜索结果及旧时间线清空。 |
| 恢复网络并点击重新连接 | 04:28:41.986 | HTTP 200，项目目录为空，旧频道不可读。 |

核对关系 1003、频道 1005、消息序号 1 时，MySQL 仍有 1 条旧索引、1 张未到期的附件票据，成功下载的审计条数为 0。业务回执不含实际用户、令牌或密码。

## A14 容量与故障恢复

同日使用一台 Windows amd64 主机（22 个逻辑处理器）、Go 1.26.0、MySQL 8.4.13 和 12 个并发工作协程。两个测试网关通过回环地址访问同一 MySQL 实例。运行涉及 100 个组织、1,000 个空间和 1,000 组未到期的附件票据。

| 阶段 | 耗时 | 成功校验 |
| --- | ---: | --- |
| 预热、建档、取游标及票据 | 13.703 秒 | 2,000 次授权读取及签发 |
| 并发撤权、重启网关、实际请求 | 2.361 秒 | 3,000 次拒绝，异常 0，超时 0 |

撤权并验证三种读取路径的单空间 p95 为 48.883 毫秒。三种读取路径是持旧游标补拉、搜索和持旧票据下载。测试还检查所有票据在采样结束后仍未自然过期。以上为单机回环结果。

## 升级与恢复

从 v1.1.0 独立二进制写入 schema 7，运行租户预检，再向前迁移到 schema 19。使用原设备凭据回读旧消息、幂等回执、普通群成员和成员操作审计。协作入口在原租户逐个启用，尚未发布的旧群数据留在原群。

让两个独立网关共享原数据库与附件卷。停写后备份并导入空数据库，将附件复制到另一个目录。恢复节点验证主持方的原游标和附件 SHA-256，已撤权访客的游标与票据均返回 404。已撤回消息仍不出现在搜索结果，新消息沿用频道序号 3。归档恢复前后比对原始 JSON 的 SHA-256，实测为 `b6c9e2c377259e1126f6ee9c69d308a35392857c460076802fb212cd47f20c65`。

## 复现

Windows 使用 PowerShell 7，提供 MySQL 8.4 Windows 二进制目录。浏览器场景还需要 Node.js 24、Playwright 和 Chrome 或 Playwright Chromium。用于升级的旧二进制可以从 v1.1.0 构建。

```powershell
npm install --prefix ./storage/browser --no-audit --no-fund playwright@1.65.0
node ./storage/browser/node_modules/playwright/cli.js install chromium
$env:MESSAGING_PLAYWRIGHT_MODULE = (Resolve-Path ./storage/browser/node_modules/playwright).Path
$env:MESSAGING_BROWSER_TEST = '1'
$env:MESSAGING_SHARED_CAPACITY_TEST = 'full'
./scripts/test-mysql.ps1 -MySQLBin 'C:\mysql\bin' -PreviousBinary 'C:\rehearsal\messaging-v1.1.0.exe' -VerifyRestore
```

已有测试 MySQL 的 Linux 环境可设置 `MESSAGING_TEST_MYSQL_DSN` 后运行 `go test -count=1 -v ./...`。完整容量和浏览器由上述环境变量启用。浏览器使用本机 Chrome 时可将 `MESSAGING_BROWSER_BIN` 指到 Chromium 兼容的可执行文件。
