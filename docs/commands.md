<!-- 由 go generate ./internal/command/ 从命令表生成，不要手改。 -->
# 命令 × scope 对照表

每条命令所需的权限范围（scope）、危险类与 confirm 口径、是否人类专属、是否不要身份，以及 REST 映射，都来自 `internal/command` 的命令表；CLI、REST、MCP 三个投影从同一张表构造。危险类命令须带字符串 `confirm`（object 填对象名、count 填本次数量）；人类专属命令对任何令牌拒绝、MCP 上不可用，请求里要带当场验证的 `verify-password` / `verify-code` / `verify-user`；不要身份的命令只有初始化向导的两条，MCP 上同样不可用。

## 经主控的命令

| 命令 | 类别 | scope | 危险类 | confirm | 人类专属（当场验证） | 不要身份 | 列表 | REST |
|---|---|---|---|---|---|---|---|---|
| `account recovery-codes regenerate` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/account/recovery-codes/regenerate` |
| `account set-password` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/account/set-password` |
| `account show` | read | read | — | — | 否 | 否 | 否 | `GET /api/v1/account/show` |
| `account totp confirm` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/account/totp/confirm` |
| `account totp disable` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/account/totp/disable` |
| `account totp setup` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/account/totp/setup` |
| `audit list` | read | read | — | — | 否 | 否 | 是 | `GET /api/v1/audit` |
| `explain` | read | read | — | — | 否 | 否 | 否 | `GET /api/v1/explain/{target}` |
| `mcp status` | read | read | — | — | 否 | 否 | 是 | `GET /api/v1/mcp/status` |
| `settings master-url set` | master_settings | operate | — | — | 是 | 否 | 否 | `POST /api/v1/settings/master-url/set` |
| `settings rollback` | master_settings | operate | — | — | 否 | 否 | 否 | `POST /api/v1/settings/rollback/{snapshot}` |
| `settings set` | master_settings | operate | — | — | 否 | 否 | 否 | `POST /api/v1/settings/set` |
| `settings show` | read | read | — | — | 否 | 否 | 否 | `GET /api/v1/settings/show` |
| `settings snapshots list` | read | read | — | — | 否 | 否 | 是 | `GET /api/v1/settings/snapshots` |
| `setup init` | action | operate | — | — | 否 | 是 | 否 | `POST /api/v1/setup/init` |
| `setup status` | read | read | — | — | 否 | 是 | 否 | `GET /api/v1/setup/status` |
| `token create` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/token/create` |
| `token list` | read | read | — | — | 否 | 否 | 是 | `GET /api/v1/token` |
| `token revoke` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/token/revoke/{id}` |
| `token update` | action | operate | — | — | 是 | 否 | 否 | `POST /api/v1/token/update/{id}` |
| `whoami` | read | read | — | — | 否 | 否 | 否 | `GET /api/v1/whoami` |

## 本地命令（不经主控，只在 CLI 里）

| 命令 | 说明 |
|---|---|
| `__verify`（隐藏） | 用发布公钥验签（内部命令） |
| `admin reset-password` | 在主控本机重置一个管理员账号的密码（不经主控、不进审计） |
| `db migrate` | 执行全部未应用的迁移，然后比对库结构与注册表 |
| `db status` | 列出已应用与待应用的迁移，并比对库结构（不写盘） |
| `db unlock` | 清除上一次迁移被中断后残留的迁移锁 |
| `login` | 验过一把令牌后，把主控地址与它存进登录文件，之后的命令默认连那个主控 |
| `logout` | 删掉登录文件（令牌在服务端仍然有效，吊销用 token revoke） |
| `mcp init` | 把一个 AI runtime 接上主控：签一把令牌（或用已有的），写进它的 MCP 配置与环境变量 |
| `mcp stdio` | stdio 方式的 MCP 垫片：连上主控的 /mcp，把它的两个工具转给本地 runtime |
| `serve` | 启动主控（只在 Linux 上） |
| `version` | 打印版本号、commit 与构建时间 |
