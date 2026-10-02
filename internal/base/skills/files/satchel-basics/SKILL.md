---
name: satchel-basics
description: "用 Satchel 之前先读：开局看总览与身份，怎么学命令、看输出与错误，危险操作与只有人能做的操作怎么处理，MCP 上怎么调用。"
---

# satchel-basics

目标：用 `satchel` 命令看清主控的状态，按需学会要用的命令，并知道哪些事要交给人来做。别的 skill 都默认你读过这一份。

## 开局

1. 先看总览与自己的身份：

   ```
   satchel overview
   satchel whoami
   ```

   `overview` 有服务器、用户、告警、待办四个分区，各自随对应功能出现，现在都是空的。`whoami` 报出你是谁（`actor`、`actor_kind`、`role`）、令牌的 id，以及权限范围（`scopes`）与打开的危险类（`danger`）。后面能做什么、不能做什么，以它为准。
2. 主控自身的状态（下面四条只对管理员开放，普通用户签的令牌会得到 `forbidden`）：
   - 当前的数据库：`satchel database show`
   - 备份：`satchel backup list`
   - 长任务：`satchel job list`
   - 主控版本与能不能升级：`satchel update check`。它要先从更新 CDN 或 GitHub 取到最新版本，取不到时整条返回 `unavailable`，这时也看不到主控的当前版本。
3. `satchel version` 只报本机这个 CLI 的版本，不是主控的版本。

## 学命令

- 不知道有哪些命令、某条命令怎么写时，按需看帮助，不要凭印象猜 flag：

  ```
  satchel --help
  satchel backup --help
  satchel token update --help
  ```

- `satchel explain` 不带参数列出全部命令与 kind；带参数解释一条命令或一个 kind：参数、类别、权限范围、是不是危险类与人类专属、confirm 填什么，例如 `satchel explain "settings set"`、`satchel explain SystemSettings`。
- CLI 上的 `satchel explain` 按本机这个二进制作答。本机 CLI 与主控的版本可能不同（例如主控刚升级过），这时用 MCP 的 `satchel_explain` 核对：它报的是主控自己的命令表。

## 输出与错误

- 输出是 JSON：`satchel mcp init` 给 runtime 配了环境变量 `SATCHEL_OUTPUT=json`。没配时在命令后面加 `--json`。
- 失败时输出四字段错误：`code`（机器可读的错误码）、`reason`（原因）、`state`（相关的当前状态）、`next`（建议的下一步）。先看 `code` 与 `next`，照 `next` 做。
- 退出码：0 成功；1 一般失败；2 用法错误（`usage`）；3 认证失败（`unauthenticated`）；4 权限不足（`forbidden`）；5 对象不存在（`not_found`）；6 版本冲突（`version_conflict`）；7 危险操作没带对 confirm（`confirm_required`）；8 部分失败（`partial_failure`）；9 只有人能做（`human_required`）。

## 长任务

- `backup create`、`database migrate`、`update apply` 是长任务：受理后立刻返回 job，工作在主控里接着跑。CLI 默认跟到结束；带 `--no-wait` 只拿到 job，例如 `satchel backup create --no-wait`。
- 之后用 `satchel job get <job_id>` 查结局（`status` 是 queued、running、done、failed、unknown 之一），用 `satchel job list` 看最近的长任务。
- MCP 上的 `satchel_run` 最多等 60 秒，到时返回当时的 job，再用 `job get` 跟。
- 数据库迁移或主控升级期间，别的命令会返回 `unavailable`：照错误里的 `next` 等一会再试，或用 `satchel job list` 看进度。

## 危险操作

- 危险类命令要令牌打开了对应的危险类，并且每次都带 `--confirm`。填什么看 `satchel explain <命令>` 给的 confirm 口径：object 填对象名（例如 `update apply` 填版本号），count 填本次受影响的数量。
- 例：`satchel update apply 0.1.1 --confirm 0.1.1`。没带或填错是 `confirm_required`。
- 权限不够（`forbidden`）时，把缺什么告诉人，由人决定要不要给令牌开权限；不要换别的路绕过去。

## 只有人能做的操作

- 人类专属命令对任何令牌都拒绝（`human_required`），MCP 上也做不了。遇到时请人在主控本机执行，原话这样写（换成这一步的具体命令）：

  这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel token revoke <id> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。

- 这个用户以前用 `satchel login` 存过登录文件的，先请人执行 `satchel logout`：带着令牌就不是本机管理员了。
- 例外：`account set-password`、两步验证与恢复码这几条命令在主控本机做不了，见 `satchel-access`。
- `satchel explain` 里有 `password` 类型参数的命令（例如 `database test`、`setup init`）要在终端里输入密码：AI 的 shell 没有终端，MCP 上也传不了密码，同样请人来执行。

## MCP 上怎么调用

- `satchel_run` 的参数数组就是命令行去掉开头的 `satchel`，按词拆开。例如命令行 `satchel job get job-0123456789abcdef` 对应参数数组 `["job", "get", "job-0123456789abcdef"]`。危险类可以把 `--confirm` 写在数组里，也可以填 `satchel_run` 的 `confirm` 参数。输出恒为 JSON。
- MCP 上做不了的：
  - 本地命令（`version`、`db`、`serve`、`admin reset-password`），以及 `login`、`logout`、`mcp` 下的命令。看谁在连，改用 `satchel token list`。
  - 初始化向导 `setup` 下的命令、上传与下载类命令、人类专属命令。
  - `--help`：看说明改用 `satchel_explain`，`target` 填命令路径或 kind 名。
  - `--server`、`--token`、`--data-dir`：MCP 的身份只来自这次连接。

## 远程用 CLI

- CLI 连哪个主控、带哪把令牌，按「`--server` 与 `--token` → 环境变量 `SATCHEL_SERVER` 与 `SATCHEL_TOKEN` → 登录文件」的顺序取；都没有就连本机的 unix socket。
- 常用的机器由人执行 `satchel login --server https://panel.example.com`，令牌从终端读，存进登录文件；`satchel logout` 只删登录文件，令牌在服务端仍然有效。
- 除了 `login`、`mcp init`、`mcp stdio`，本地命令不接受 `--server` 与 `--token`。

## 注意

- skills 只是说明，不是权限：令牌的权限范围、危险类与人类专属始终由主控判定。
- 写操作之前先说明要做什么、为什么；拿不准的先问人。
