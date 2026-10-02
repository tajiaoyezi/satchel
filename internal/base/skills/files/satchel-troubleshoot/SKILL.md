---
name: satchel-troubleshoot
description: "主控出问题时用：看日志、内置定时任务的运行记录、长任务、审计与安全事件，主控起不来或被挡在门外时请人在主控本机处理。"
---

# satchel-troubleshoot

目标：先用只读命令定位主控的问题，给出结论与修复建议；要动手的地方说明要做什么，人类专属与要在主控本机做的交给人。

看日志、定时任务、长任务、审计与安全事件的命令都只对管理员开放，令牌是普通用户签的会得到 `forbidden`。后面「主控起不来」「管理员忘了密码」两节是在主控本机执行的本地命令，不经主控，也就没有这道权限检查。

## 看日志

1. 从新到旧看主控日志（只扫当前文件末尾 50000 行，走分页）：

   ```
   satchel logs list
   satchel logs list --level warn
   ```

   `--level` 只看这一级及更高的（debug、info、warn、error）。每一项拆成 `time`、`level`、`msg` 与 `attrs`，拆不开的行给 `raw`。
2. 找含某段文本的行（区分大小写）：`satchel logs list --grep <文本>`。
3. 当前文件到 50 MB 会轮转，旧文件最多留 3 个。列出当前文件与轮转下来的旧文件：`satchel logs files list`。`logs list` 只看当前文件。

## 看内置定时任务

- `satchel schedule list` 列出内置任务、间隔与最近一次运行的结果。
- `satchel schedule runs list --status error` 只看失败的运行记录；`satchel schedule runs list --task backup_local` 只看一个任务的。
- 常用的任务名：`backup_local`（每天一份本机备份）、`db_health`（每分钟检查数据库，变坏时记 error 日志）、`audit_cleanup`、`security_event_cleanup`、`job_cleanup`。

## 看长任务

- `satchel job list --status failed` 列出失败的长任务；`satchel job get <job_id>` 看一个的状态、开始与结束时间、退出码与结果。
- 主控重启时没跑完的 job 标为 `failed`（`update apply` 的 job 例外：由升级之后起来的进程写成 `done` 或 `failed`）。

## 看审计与安全事件

- 审计记录按时间倒序：`satchel audit list --command token --since 2026-09-19T00:00:00Z`。`--actor` 精确匹配执行者，`--command` 按命令名前缀匹配，`--since` 是 RFC 3339 时间。被权限、confirm 或当场验证拒绝的命令也有记录。
- 安全事件按时间倒序：`satchel security events list --kind probe`。`--kind` 是 login_fail、login_locked、verify_fail、verify_locked、probe（令牌校验失败）、ban、ban_manual、unban 之一，`--ip` 只看一个来源 IP。
- 一个 IP 反复出现 `probe` 通常是某个客户端配着失效的令牌在重试；达到上限会被自动封禁，见 `satchel-access` 的 IP 封禁。

## 主控起不来

这一节的命令要人在主控本机、用 root 执行（主控以 root 运行，数据目录是 `/var/lib/satchel`）；它们是本地命令，不经主控，AI 经 MCP 或令牌做不了。请人按顺序做：

1. 看服务的日志，找启动失败的那一行：systemd 用 `journalctl -u satchel -n 200`，OpenRC 看 `/var/log/satchel.log`，Docker 用 `docker compose logs satchel`；主控自己的日志文件在 `/var/lib/satchel/logs/satchel.log`。
2. 看迁移状态与库结构的比对（只读）：

   ```
   sudo satchel db status
   ```

3. 日志或 `db migrate` 报迁移锁的 `conflict`（上一次迁移被打断留下的），确认没有别的迁移在跑之后清掉它，再执行迁移：

   ```
   sudo satchel db unlock
   sudo satchel db migrate
   ```

   `db migrate` 报 `schema_mismatch` 时库结构与这个版本的不一致：把列出的差异原样交给人，不要建议手改库。
4. 要在前台看完整的启动过程：先停掉服务（`systemctl stop satchel` 或 `rc-service satchel stop`），再执行 `sudo satchel serve`，看完按 Ctrl-C 停下，再启动服务。同一个数据目录只能跑一个主控，服务没停时 `serve` 会报 `conflict`。
5. Docker 部署：容器在反复重启时 `docker compose exec` 进不去。在 compose 文件所在的目录（一键脚本是 `/opt/satchel`）先 `docker compose stop satchel`，再用 `docker compose run --rm --entrypoint satchel satchel db status` 这样的写法，把最后的 `db status` 换成上面的命令；处理完 `docker compose up -d`。

## 管理员忘了密码

这一步只能由人做：请用户在主控本机用 root 执行 `sudo satchel admin reset-password <用户名> --confirm <用户名>`，新密码在终端里读两遍；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。

- 它直接开数据目录里的库，主控在不在跑都行；只对管理员账号。
- 它作废这个账号的全部会话，不动两步验证；不经主控，所以不进审计。

## 被门或 IP 封禁挡在外面

症状：经 TCP 的请求（网页、远程 CLI、MCP）被拒，或网页一律 404；主控本机经 unix socket 的 CLI 不受门、限流与封禁影响。

1. 能用 CLI 时先看原因：`satchel settings show` 里 `status` 的 `master_local_only`、`silent_mode`，`satchel security bans list` 看被封的 IP。
2. 改回门的设置。这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel settings gates set --set master_local_only=false --set silent_mode=false --resource-version <N> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。`<N>` 是 `sudo satchel settings show` 里的 `metadata.resourceVersion`。
3. 解封一个 IP。这一步只能由人做：请用户在同样的终端里执行 `sudo satchel security unban <ip> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署同样换成 `docker compose exec satchel satchel`。
4. 静默模式也可以重启主控后，在开放期（`silent_mode_timeout` 分钟）里进去改。

## 注意

- 先诊断、后动手；每个写操作之前说明要做什么。
- 日志、审计与安全事件里可能有 IP 与账号名，给人看结论时只摘要需要的部分。
- 不要建议删日志文件或改数据目录里的文件来「修好」问题，交给人判断。
