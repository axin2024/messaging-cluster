# 应用与设备身份

`messaging create-app TENANT APP` 幂等使用已有的租户键，同租户下同应用名重复创建返回冲突。管理令牌全局唯一，仅在创建成功时输出。它与设备会话使用不同的授权路径。两类令牌都是随机 256 位值，MySQL 保存 SHA-256 摘要。用户密码经 bcrypt 存储，可用 UTF-8，长度为 12–72 字节。

## HTTP 接口

写入 JSON 的方法使用 `Content-Type: application/json`。需要授权的请求提交 `Authorization: Bearer TOKEN`。会话与设备响应禁止缓存。

| 方法与路径 | 凭据 | 内容 |
| --- | --- | --- |
| `GET /healthz` | 无 | 进程存活 |
| `GET /readyz` | 无 | MySQL 可达、迁移版本匹配 |
| `POST /v1/admin/users` | 应用管理令牌 | `username`、`display_name`、`password` |
| `POST /v1/sessions` | 账号和密码 | `tenant`、`app`、`username`、`password`、`device_key` |
| `GET /v1/me` | 设备令牌 | 当前账号与设备 |
| `GET /v1/devices?after=ID` | 设备令牌 | 当前账号的设备，按 ID 分页 |
| `POST /v1/devices/current/rotate` | 设备令牌 | 替换当前令牌，返回新令牌 |
| `DELETE /v1/devices/{id}` | 设备令牌 | 撤销本账号指定设备，返回 204 |

租户、应用、用户名和设备键使用小写 ASCII 字母、数字、连字符和下划线，开头是小写字母或数字，最长 64 字符。提交应用管理请求时，租户和应用从已验证凭据派生。设备重复登录时保留设备 ID，轮换令牌并更新有效期。轮换和登录发放 30 天会话。已撤销或过期凭据返回 401，查询或撤销其他账号的设备按访问范围处理。

## MySQL 回归

将 `MESSAGING_TEST_MYSQL_DSN` 指向独立的 `messaging_test_*` 测试库，然后在仓库根目录执行 `go test -count=1 -v ./...`。测试会应用和重放迁移，创建两组隔离的应用和账号，验证设备轮换、撤销、重新登录以及两类令牌的权限边界。

已在 Windows、Go 1.26.0 和 MySQL 8.4.13 的独立测试环境运行。两条并发轮换请求只生成一个可用的新令牌，从电脑撤销手机后，电脑令牌仍可读到自己的会话。独立进程完成了 CLI 迁移、创建应用、健康检查、用户登录和令牌回读。
