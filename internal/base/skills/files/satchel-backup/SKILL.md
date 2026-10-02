---
name: satchel-backup
description: "备份与恢复主控时用：生成、列出、上传备份；下载与恢复要请人做；新装的主控用初始化向导建管理员或从备份恢复。"
---

# satchel-backup

目标：让主控随时有一份能用的备份；要恢复时把步骤与后果讲清楚，交给人执行。

这一份里的命令（初始化向导除外）都只对管理员开放。

## 备份里有什么

一个 ZIP：数据库的一致拷贝、`database.json`、`config.yaml`、`master.key`（主控通信密钥）、订阅文件与规则模板。会话不在里面，恢复后所有人要重新登录。备份放在主控数据目录的 `backups/`，最多留 7 份（手动的、上传的、内置任务每天生成的、恢复前与升级前自动生成的都算）。

## 生成与查看

1. 看已有的备份，从新到旧：`satchel backup list`。
2. 生成一份（长任务，令牌要能操作）：

   ```
   satchel backup create
   ```

   CLI 默认跟到结束；MCP 上最多等 60 秒，没结束就用 `satchel job get <job_id>` 跟到 `done` 或 `failed`。
3. 同一时刻只能有一次备份、上传或恢复，其余的是 `conflict`；数据库迁移或主控升级期间也会被拒绝（`unavailable` 或 `conflict`），照 `next` 等它结束。
4. 用 PostgreSQL 时备份要主控上有 `pg_dump`（主版本不低于数据库），没有时报 `unavailable` 并给出安装命令，转告人去装。

## 上传一份备份

把本地的备份文件传进主控的 `backups/`（先校验，最大 4 GiB）：

```
satchel backup upload --file <本地备份文件>
```

它只能经 CLI：文件在 CLI 这一端读取，MCP 上传不了文件。校验不过（打不开、不认识、比这个主控新）是 `bad_request`。

## 下载与恢复：要请人做

- 下载：这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel backup download <备份名> --output <本地路径> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。备份里有主控通信密钥与全部数据，下载下来的文件要妥善保管。
- 恢复：这一步只能由人做：请用户在同样的终端里执行 `sudo satchel backup restore <备份名> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署同样换成 `docker compose exec satchel satchel`。

恢复之前先把后果告诉人：

- 整个主控回到备份的那一刻，之后的写入全部丢掉；恢复前会自动存一份 `before-restore-<时间>.zip`，可以再恢复回来。
- 主控回应之后退出，由 systemd 或 Docker 拉起，启动时换库；直接在前台跑 `satchel serve` 的要手动再启动。
- 不支持跨驱动恢复：SQLite 的备份恢复不到 PostgreSQL 上，反过来也不行（`conflict`）。
- 用 PostgreSQL 时恢复要主控上有 `psql`；恢复会把整个 schema 换成备份里的样子。

## 恢复之后

- 恢复码全部换新：开了两步验证的账号各生成一批新的，明文在主控数据目录的 `recovery-codes/` 下（0600）。提醒人拿到码登录后删掉那个文件。
- 所有人要重新登录；两步验证的密钥也回到备份那一刻。
- 结果在 `satchel settings show` 的 `status` 里的 `last_restore`；审计里也有一条。

## 新装的主控

新装的主控库里还没有用户，先走初始化向导。它不要身份，MCP 上做不了；初始化之前谁都能建管理员，所以请人在主控本机或内网完成，再把主控暴露到公网。

1. 看状态与可走的路：`sudo satchel setup status`。
2. 建第一个管理员：这一步要在终端里输入密码，请人在主控本机执行 `sudo satchel setup init --username <管理员用户名>`（可选 `--email`），密码读两遍。
3. 或者不建管理员，直接把一份备份恢复进空库：请人在主控本机执行 `sudo satchel setup restore --file <备份文件>`，主控随后重启。之后的情况与上一节「恢复之后」相同。

库里已有用户后这三条里的 `setup init` 与 `setup restore` 都是 `conflict`。

## 注意

- 动备份之前先 `satchel backup list` 确认现状；要删备份没有命令，7 份之外的旧备份会自动轮换掉。
- 换数据库的驱动用在线迁移，见 `satchel-database`。
