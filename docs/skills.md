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
| `satchel-diag` | 无 SSH 排查节点故障时用：固定只读诊断集（server diag）、一键取证与证据包查询（server evidence / evidence list / evidence get），以及确实需要时在远程节点执行受控命令（server exec）。 |
| `satchel-network` | 管理节点网络探测（端口扫描、站点 TLS/Reality 校验、最多 200 域名 16 并发延迟批量探测）、纯 Go WARP WireGuard 凭据与出站/防送中快捷路由规划，以及返程路由（kind ReturnRoute）的查询与测路长任务。 |
| `satchel-proxy` | 管理 sing-box 代理入站（Inbound）、出站（Outbound）、路由规则（RoutingRule）、密钥生成、无状态建站向导、出站实时探活、中转落地规划、路由预设与多机批量规划时用：所有配置变更一律走 plan / apply。 |
| `satchel-servers` | 加服务器（节点）、取安装命令、看节点是否在线，以及给节点上的凭据临时限速、踢线、看连接与流量、重启节点内核时用：安装命令要 https 主控地址，token 原文要请人取。 |
| `satchel-settings` | 看与改系统设置时用：日常运维档的设置、logo 与登录壁纸的图片上传、写前快照与回滚；主控地址与门这两组要请人改。 |
| `satchel-troubleshoot` | 主控出问题时用：看日志、内置定时任务的运行记录、长任务、审计与安全事件，主控起不来或被挡在门外时请人在主控本机处理。 |
| `satchel-tunnels` | 部署与切换服务器 443 偷取模式（tunnel 与 fallback）、编排任意门单跳端口转发、复用已有 direct 入站按域名/IP 分流以及 N 台服务器首尾相接链式隧道时用：零新增表，全部写操作一律经 plan/apply 原子落地，严守同机 443 防抢占与 sing-box v1.14.4 规范。 |
| `satchel-upgrade` | 升级主控时用：检查更新、升到所选渠道的最新版、更新 CDN 的开关、升级失败时的自动回退。 |
| `satchel-websites` | 管理节点上的 nginx 站点（kind Website）：查站点、扫盘区分已纳管与未纳管文件、查模板目录、用无状态向导生成静态/普通反代/WSS反代/伪装站配置并经 plan/apply 声明式落盘，严守未纳管零覆盖与同机七组/443 双闸门。 |

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
| `apply` | `satchel-basics`、`satchel-network`、`satchel-proxy`、`satchel-servers`、`satchel-tunnels`、`satchel-websites` |
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
| `evidence get` | `satchel-diag` |
| `evidence list` | `satchel-diag` |
| `explain` | `satchel-basics`、`satchel-settings` |
| `inbound batch list` | `satchel-proxy` |
| `inbound batch plan` | `satchel-proxy` |
| `inbound build` | `satchel-proxy` |
| `inbound examples` | `satchel-proxy` |
| `inbound get` | `satchel-proxy` |
| `inbound keygen` | `satchel-proxy` |
| `inbound list` | `satchel-proxy` |
| `inbound protocols` | `satchel-proxy` |
| `job get` | `satchel-backup`、`satchel-basics`、`satchel-database`、`satchel-diag`、`satchel-troubleshoot`、`satchel-upgrade` |
| `job list` | `satchel-basics`、`satchel-troubleshoot` |
| `login` | `satchel-access`、`satchel-backup`、`satchel-basics`、`satchel-database`、`satchel-servers`、`satchel-settings`、`satchel-troubleshoot` |
| `logout` | `satchel-basics` |
| `logs files list` | `satchel-troubleshoot` |
| `logs list` | `satchel-troubleshoot` |
| `mcp init` | `satchel-access`、`satchel-basics` |
| `mcp status` | `satchel-access` |
| `mcp stdio` | `satchel-access` |
| `outbound batch list` | `satchel-proxy` |
| `outbound batch plan` | `satchel-proxy` |
| `outbound get` | `satchel-proxy` |
| `outbound landing plan` | `satchel-proxy` |
| `outbound list` | `satchel-proxy` |
| `outbound probe` | `satchel-proxy` |
| `overview` | `satchel-basics` |
| `plan` | `satchel-basics`、`satchel-network`、`satchel-proxy`、`satchel-servers`、`satchel-websites` |
| `plans get` | `satchel-basics` |
| `plans list` | `satchel-basics` |
| `return-route get` | `satchel-network` |
| `return-route list` | `satchel-network` |
| `return-route test` | `satchel-network` |
| `routing get` | `satchel-proxy` |
| `routing list` | `satchel-proxy` |
| `routing preset builtin` | `satchel-proxy` |
| `routing preset delete` | `satchel-proxy` |
| `routing preset list` | `satchel-proxy` |
| `routing preset save` | `satchel-proxy` |
| `schedule list` | `satchel-troubleshoot` |
| `schedule runs list` | `satchel-troubleshoot` |
| `security ban` | `satchel-access` |
| `security bans list` | `satchel-access`、`satchel-troubleshoot` |
| `security events list` | `satchel-troubleshoot` |
| `security unban` | `satchel-access`、`satchel-troubleshoot` |
| `serve` | `satchel-backup`、`satchel-database`、`satchel-troubleshoot` |
| `server add` | `satchel-servers` |
| `server config files` | `satchel-servers` |
| `server config read` | `satchel-servers` |
| `server config status` | `satchel-servers` |
| `server config test` | `satchel-servers` |
| `server connections` | `satchel-servers` |
| `server diag` | `satchel-diag` |
| `server domain-latency` | `satchel-network` |
| `server evidence` | `satchel-diag` |
| `server exec` | `satchel-diag` |
| `server get` | `satchel-servers` |
| `server install-command` | `satchel-servers` |
| `server kick` | `satchel-servers` |
| `server limit clear` | `satchel-servers` |
| `server limit set` | `satchel-servers` |
| `server list` | `satchel-servers` |
| `server logs files` | `satchel-servers`、`satchel-troubleshoot` |
| `server logs list` | `satchel-servers`、`satchel-troubleshoot` |
| `server nginx clear-stream` | `satchel-servers` |
| `server nginx install` | `satchel-servers` |
| `server nginx servers` | `satchel-servers` |
| `server nginx uninstall` | `satchel-servers` |
| `server nics` | `satchel-servers` |
| `server probe-master-url` | `satchel-servers` |
| `server restart-kernel` | `satchel-servers` |
| `server scan` | `satchel-network` |
| `server service control` | `satchel-servers` |
| `server service status` | `satchel-servers` |
| `server speed` | `satchel-servers` |
| `server steal plan` | `satchel-servers`、`satchel-tunnels` |
| `server steal status` | `satchel-servers`、`satchel-tunnels` |
| `server switch-listen-port` | `satchel-servers` |
| `server system-info` | `satchel-servers`、`satchel-troubleshoot` |
| `server token revoke` | `satchel-servers` |
| `server token rotate` | `satchel-servers` |
| `server token show` | `satchel-servers` |
| `server traffic` | `satchel-servers` |
| `server traffic-stats set` | `satchel-servers` |
| `server uninstall` | `satchel-servers` |
| `server update apply` | `satchel-servers` |
| `server update batch` | `satchel-servers` |
| `server update check` | `satchel-servers` |
| `server warp authorize` | `satchel-network` |
| `server warp install` | `satchel-network` |
| `server warp outbound-plan` | `satchel-network` |
| `server warp remove` | `satchel-network` |
| `server warp status` | `satchel-network` |
| `settings batch-threshold set` | `satchel-settings` |
| `settings gates set` | `satchel-settings`、`satchel-troubleshoot` |
| `settings image upload` | `satchel-settings` |
| `settings master-url migrate` | `satchel-settings` |
| `settings master-url set` | `satchel-servers`、`satchel-settings` |
| `settings rollback` | `satchel-settings` |
| `settings set` | `satchel-servers`、`satchel-settings`、`satchel-troubleshoot` |
| `settings show` | `satchel-backup`、`satchel-servers`、`satchel-settings`、`satchel-troubleshoot`、`satchel-upgrade` |
| `settings snapshots list` | `satchel-settings` |
| `settings turnstile test` | `satchel-settings` |
| `settings update-cdn set` | `satchel-upgrade` |
| `setup init` | `satchel-backup` |
| `setup restore` | `satchel-backup` |
| `setup status` | `satchel-backup` |
| `snapshot get` | `satchel-basics` |
| `snapshot list` | `satchel-basics` |
| `snapshot rollback` | `satchel-basics`、`satchel-network`、`satchel-tunnels`、`satchel-websites` |
| `token create` | `satchel-access` |
| `token list` | `satchel-access`、`satchel-basics` |
| `token revoke` | `satchel-access`、`satchel-basics` |
| `token update` | `satchel-access`、`satchel-basics` |
| `tunnel chain delete-plan` | `satchel-tunnels` |
| `tunnel chain list` | `satchel-tunnels` |
| `tunnel chain plan` | `satchel-tunnels` |
| `tunnel delete-plan` | `satchel-tunnels` |
| `tunnel list` | `satchel-tunnels` |
| `tunnel multiplex plan` | `satchel-tunnels` |
| `tunnel plan` | `satchel-tunnels` |
| `update apply` | `satchel-basics`、`satchel-upgrade` |
| `update check` | `satchel-basics`、`satchel-upgrade` |
| `version` | `satchel-basics` |
| `website build` | `satchel-websites` |
| `website get` | `satchel-websites` |
| `website list` | `satchel-websites` |
| `website scan` | `satchel-websites` |
| `website templates` | `satchel-websites` |
| `website validate` | `satchel-network` |
| `whoami` | `satchel-access`、`satchel-basics` |
