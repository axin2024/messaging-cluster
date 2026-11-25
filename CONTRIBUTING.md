# 参与开发

准备 Go 1.26、MySQL 8.4 和只用于测试的空数据库。`go test ./...` 执行编译与静态前置检查。将测试库 DSN 写入 `MESSAGING_TEST_MYSQL_DSN` 后运行以下命令，执行数据库集成用例。

```sh
gofmt -w ./cmd ./internal
go vet ./...
go test -count=1 -v ./...
```

Windows 开发者可以通过 `scripts/test-mysql.ps1` 创建隔离的数据库实例。缺陷报告请提供标明版本的合成输入、步骤、期望结果、实际结果和相关会话序号。清除样例中的令牌、DSN、附件内容和个人信息。

修改网关时考虑断线、重复帧、乱序通知、设备接管与旧代数。新增媒体适配器时注明私有存储、写入校验、授权读取及清理要求。新增数据库迁移采用向前迁移，在空库与上一个兼容 schema 分别验证。对协议有影响的变更应同步更新相应 API 文档和兼容说明。

[本地集成测试](docs/operations.md) · [需求与验收](docs/requirements/04-验收与运营-prd.md) · [安全反馈](SECURITY.md)
