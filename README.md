# Satchel（百宝袋）

Agent-first 的多服务器代理管理系统。主控、CLI 与 MCP 在这一个仓库、一个二进制 `satchel` 里；节点守护是 [satchel-agent](https://github.com/satchel/satchel-agent)。

**状态：M0 骨架阶段，还没有可用功能。**

## 构建

```sh
go build ./cmd/satchel
./satchel version
```

## 代码分层

六层，依赖只向下：契约（`pkg/api/v1`、`internal/command`）→ 投影（`internal/projection`）→ 横切（`internal/middleware`）→ 业务（`internal/service`）→ 仓储（`internal/core`）→ 基础设施（`internal/base`）。每个目录的 `doc.go` 写了该层的职责；引用规则由 `internal/layering_test.go` 钉住，违反即 `go test` 失败。

## 许可证

GPL-3.0，见 [LICENSE](LICENSE)。
