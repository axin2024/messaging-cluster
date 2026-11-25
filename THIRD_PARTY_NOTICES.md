# 第三方声明

项目源码以 [Apache License 2.0](LICENSE) 发布。下表列出 `messaging` 服务构建直接使用的第三方模块和随 Go 编译的运行时。版本以 `go.mod`、`go.sum` 和构建工具链为准。

| 组件 | 版本 | 许可正文 |
| --- | --- | --- |
| coder/websocket | v1.8.15 | [ISC](licenses/coder-websocket.txt) |
| Go MySQL Driver | v1.10.2 | [MPL-2.0](licenses/go-sql-driver-mysql.txt) |
| golang-migrate | v4.20.1 | [MIT](licenses/golang-migrate.txt) |
| Go `x/crypto` | v0.58.0 | [BSD-3-Clause](licenses/go-x-crypto.txt) · [PATENTS](licenses/go-x-crypto-patents.txt) |
| filippo.io/edwards25519 | v1.2.0 | [BSD-3-Clause](licenses/edwards25519.txt) |
| Go 运行时及标准库 | Go 1.26.0 | [Go LICENSE](licenses/go-runtime.txt) |

第三方模块按 `go.sum` 获取。MySQL 8.4 作为独立运行时依赖由部署者安装。随源码提供的示例消息、账号和附件内容均为合成夹具，使用本项目许可。
