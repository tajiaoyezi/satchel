<!-- 由 go generate ./internal/command/ 从命令表与 internal/base/skills/files/ 生成，不要手改。 -->
# 命令 × skills 对照表

skills 是写给 AI 的操作手册：源文件在 `internal/base/skills/files/`，编进 satchel 二进制，`satchel mcp init` 把它们装进 runtime 的 skills 目录。命令表里每一条非隐藏命令都要被至少一个 skill 讲到；skills 里写的每一条 satchel 命令行都要在这个二进制的 CLI 上解析得了。这两条由 `go test ./internal/base/skills/` 与 CI 守着（master-skills）。

## skills

| skill | 什么时候用 |
|---|---|
| `satchel-access` | 管令牌、AI 接入、账号与 IP 封禁时用：看令牌与谁在连，签发吊销令牌与接入 runtime 要请人做。 |
| `satchel-backup` | 备份与恢复主控时用：生成、列出、上传备份；下载与恢复要请人做；新装的主控用初始化向导建管理员或从备份恢复。 |
| `satchel-basics` | 用 Satchel 之前先读：开局看总览与身份，怎么学命令、看输出与错误，危险操作与只有人能做的操作怎么处理，MCP 上怎么调用。 |
| `satchel-database` | 看或更换主控的数据库时用：看当前的数据库、试连 PostgreSQL、从 SQLite 在线迁移。 |
| `satchel-settings` | 看与改系统设置时用：日常运维档的设置、写前快照与回滚；主控地址与门这两组要请人改。 |
| `satchel-troubleshoot` | 主控出问题时用：看日志、内置定时任务的运行记录、长任务、审计与安全事件，主控起不来或被挡在门外时请人在主控本机处理。 |
| `satchel-upgrade` | 升级主控时用：检查更新、升到所选渠道的最新版、更新 CDN 的开关、升级失败时的自动回退。 |

## 命令

| 命令 | 讲到它的 skill |
|---|---|
| `account recovery-codes regenerate` | `satchel-access` |
| `account set-password` | `satchel-access` |
| `account show` | `satchel-access` |
| `account totp confirm` | `satchel-access` |
| `account totp disable` | `satchel-access` |
| `account totp setup` | `satchel-access` |
| `admin reset-password` | `satchel-troubleshoot` |
| `audit list` | `satchel-troubleshoot` |
| `backup create` | `satchel-backup`、`satchel-basics`、`satchel-upgrade` |
| `backup download` | `satchel-backup` |
| `backup list` | `satchel-backup`、`satchel-basics` |
| `backup restore` | `satchel-backup` |
| `backup upload` | `satchel-backup` |
| `database migrate` | `satchel-database` |
| `database show` | `satchel-basics`、`satchel-database` |
| `database test` | `satchel-database` |
| `db migrate` | `satchel-troubleshoot` |
| `db status` | `satchel-troubleshoot` |
| `db unlock` | `satchel-troubleshoot` |
| `explain` | `satchel-basics`、`satchel-settings` |
| `job get` | `satchel-backup`、`satchel-basics`、`satchel-database`、`satchel-troubleshoot`、`satchel-upgrade` |
| `job list` | `satchel-basics`、`satchel-troubleshoot` |
| `login` | `satchel-access`、`satchel-backup`、`satchel-basics`、`satchel-database`、`satchel-settings`、`satchel-troubleshoot` |
| `logout` | `satchel-basics` |
| `logs files list` | `satchel-troubleshoot` |
| `logs list` | `satchel-troubleshoot` |
| `mcp init` | `satchel-access`、`satchel-basics` |
| `mcp status` | `satchel-access` |
| `mcp stdio` | `satchel-access` |
| `overview` | `satchel-basics` |
| `schedule list` | `satchel-troubleshoot` |
| `schedule runs list` | `satchel-troubleshoot` |
| `security ban` | `satchel-access` |
| `security bans list` | `satchel-access`、`satchel-troubleshoot` |
| `security events list` | `satchel-troubleshoot` |
| `security unban` | `satchel-access`、`satchel-troubleshoot` |
| `serve` | `satchel-backup`、`satchel-database`、`satchel-troubleshoot` |
| `settings gates set` | `satchel-settings`、`satchel-troubleshoot` |
| `settings master-url set` | `satchel-settings` |
| `settings rollback` | `satchel-settings` |
| `settings set` | `satchel-settings` |
| `settings show` | `satchel-backup`、`satchel-settings`、`satchel-troubleshoot`、`satchel-upgrade` |
| `settings snapshots list` | `satchel-settings` |
| `settings update-cdn set` | `satchel-upgrade` |
| `setup init` | `satchel-backup` |
| `setup restore` | `satchel-backup` |
| `setup status` | `satchel-backup` |
| `token create` | `satchel-access` |
| `token list` | `satchel-access`、`satchel-basics` |
| `token revoke` | `satchel-access`、`satchel-basics` |
| `token update` | `satchel-access`、`satchel-basics` |
| `update apply` | `satchel-basics`、`satchel-upgrade` |
| `update check` | `satchel-basics`、`satchel-upgrade` |
| `version` | `satchel-basics` |
| `whoami` | `satchel-access`、`satchel-basics` |
