---
name: satchel-database
description: "看或更换主控的数据库时用：看当前的数据库、试连 PostgreSQL、从 SQLite 在线迁移。"
---

# satchel-database

目标：说清主控现在用的是什么数据库；人要换到 PostgreSQL 时，先试连确认目标库可用，再把在线迁移的步骤与影响交给人执行。

这一份里的命令都只对管理员开放。

## 看当前的数据库

```
satchel database show
```

输出里有驱动（sqlite 或 postgres）、SQLite 的库文件或 PostgreSQL 的连接参数（密码只报配没配），以及是否被 `SATCHEL_DATABASE_*` 环境变量覆盖。

## 试连一个 PostgreSQL：要请人做

试连是只读的，报告 PostgreSQL 的版本、目标库当前的 schema 是不是空的，以及这台主控上的 `pg_dump` 与 `psql` 够不够迁过去之后做备份与恢复。它要在终端里输入两遍目标库的密码：AI 的 shell 没有终端，MCP 上也传不了密码，所以请人在自己的终端里执行，例如：

```
sudo satchel database test --host 127.0.0.1 --port 5432 --name satchel --user satchel --sslmode disable
```

- 它不是人类专属：主控本机的终端（经 socket）或带着令牌的远程 CLI 都行；把主机、库名、用户换成人给的值。
- `--sslmode` 是 disable、allow、prefer、require、verify-ca、verify-full 之一；不给时用默认值。
- 用一键脚本的 Docker 部署时，compose 里带一个可选的 PostgreSQL（`docker compose --profile postgres up -d`），主控走 host 网络，主机写 `127.0.0.1`。
- 工具不够时，按输出里给的安装命令请人装好 PostgreSQL 的客户端工具再试。

## 从 SQLite 在线迁移到 PostgreSQL：要请人做

迁移之前先把前提与影响讲给人：

- 前提：当前是 SQLite（`satchel database show`）；没有用 `SATCHEL_DATABASE_*` 环境变量配库；没有备份、上传或恢复在进行；目标库连得上，并且当前 schema 里一张表都没有（先用上一节的试连确认）。
- 迁移期间主控只能查 job：别的命令与登录一律 `unavailable`。耗时与数据量成正比，挑低峰时做。
- 只支持 SQLite 到 PostgreSQL 一个方向。迁移之后原来的 `satchel.db` 留在数据目录里，但不再是最新的。
- 迁过去之后，备份要 `pg_dump`、恢复要 `psql`。

执行：这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel database migrate --host 127.0.0.1 --port 5432 --name satchel --user satchel --verify-user <管理员账号>`，它会先要两遍目标库的密码，再当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。

## 迁移之后

1. 它是长任务，CLI 默认跟到结束。中途想看进度（阶段、当前表、完成的表数、已拷的行数）：`satchel job get <job_id>`。
2. 成功后几秒内主控退出，由 systemd 或 Docker 拉起，连上 PostgreSQL。会话、令牌、两步验证与设置的版本都原样过去，不用重新登录。直接在前台跑 `satchel serve` 的要手动再启动。
3. 主控起来之后用 `satchel database show` 确认驱动已是 postgres。
4. 提交点（改写 `database.json`）之前任何一步失败：目标库回到空的，主控照常用 SQLite；`satchel job get <job_id>` 里有失败原因。

## 注意

- 不要建议手改 `database.json` 或环境变量来换库：在线迁移会拷数据并核对行数，手改只会让主控连上一个空库。
- 要回到 SQLite 只能人手工把 `database.json` 改回去，迁移之后写进 PostgreSQL 的数据不会在 SQLite 里；这种情况交给人判断。
