<!-- 由 go generate ./internal/command/ 从命令表生成，不要手改。 -->
# 命令 × scope 对照表

每条命令所需的权限范围（scope）、危险类与 confirm 口径、是否人类专属，以及 REST 映射，都来自 `internal/command` 的命令表；CLI、REST、MCP 三个投影从同一张表构造。危险类命令须带字符串 `confirm`（object 填对象名、count 填本次数量）；人类专属命令对任何令牌拒绝、MCP 上不可用。

## 经主控的命令

| 命令 | 类别 | scope | 危险类 | confirm | 人类专属 | 列表 | REST |
|---|---|---|---|---|---|---|---|
| `audit list` | read | read | — | — | 否 | 是 | `GET /api/v1/audit` |
| `explain` | read | read | — | — | 否 | 否 | `GET /api/v1/explain/{target}` |
| `whoami` | read | read | — | — | 否 | 否 | `GET /api/v1/whoami` |

## 本地命令（不经主控，只在 CLI 里）

| 命令 | 说明 |
|---|---|
| `__verify`（隐藏） | 用发布公钥验签（内部命令） |
| `db migrate` | 执行全部未应用的迁移，然后比对库结构与注册表 |
| `db status` | 列出已应用与待应用的迁移，并比对库结构（不写盘） |
| `db unlock` | 清除上一次迁移被中断后残留的迁移锁 |
| `serve` | 启动主控（只在 Linux 上） |
| `version` | 打印版本号、commit 与构建时间 |
