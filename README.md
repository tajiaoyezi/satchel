# Satchel（百宝袋）

Agent-first 的多服务器代理管理系统。主控、CLI 与 MCP 在这一个仓库、一个二进制 `satchel` 里；节点守护是 [satchel-agent](https://github.com/satchel/satchel-agent)。

**状态：M1 主控基础阶段。主控能起来（`serve`）、REST / CLI / MCP 三个投影同构上线，初始化向导、网页登录与两步验证可用，业务命令随后续 change 加入。**

## 构建

```sh
go build ./cmd/satchel
./satchel version
```

## 运行

```sh
./satchel serve --data-dir ./data        # 只在 Linux 上；起主控：迁移 → 监听 TCP 与 unix socket → 等 SIGINT / SIGTERM
./satchel whoami --data-dir ./data       # 在主控本机经 socket 调它：root 或运行主控的 OS 用户就是本机管理员
./satchel explain "audit list"           # 不需要主控在跑：命令表与 kind 清单编在二进制里
```

`serve` 读数据目录下的 `config.yaml`（可不存在，只认两个键）和环境变量，环境变量覆盖文件；非 Linux 上 `serve` 以 `unsupported_platform` 拒绝（那四个平台只保证客户端子命令）。

| 变量 | 说明 | 默认值 |
|---|---|---|
| `SATCHEL_DATA_DIR` | 数据目录（等价于 `--data-dir`） | `/var/lib/satchel` |
| `SATCHEL_CONFIG` | 配置文件路径（等价于 `serve --config`） | `<数据目录>/config.yaml` |
| `SATCHEL_LISTEN` | TCP 监听地址，对应 `config.yaml` 的 `listen` | `0.0.0.0:12889` |
| `SATCHEL_LOG_LEVEL` | 日志级别 `debug` / `info` / `warn` / `error`，对应 `log_level` | `info` |
| `SATCHEL_DATABASE_DRIVER` 等 | 数据库连接，见「数据库」一节 | SQLite |
| `SATCHEL_OUTPUT` | 设为 `json` 时 CLI 默认 JSON 输出（等价于 `--json`） | 文本 |

主控同时监听 TCP 与数据目录下的 unix socket `satchel.sock`（0600）。**身份只从连接判定**：经 socket 进来、对端是 root 或运行主控的那个 OS 用户 → 本机管理员（全部权限，socket 上带的 cookie 不看）；TCP 上带有效会话 cookie → 登录的用户（管理员全部权限，普通用户只有 `read` + `operate`、没有危险类）；其余一律没有身份（令牌随 m1-04 交付）。没有身份能到的只有：`GET /api/v1/healthz`、`/public/<file>`（数据目录 `public/` 里的文件，目录不列、`..` 出不去）、初始化向导的 `setup status` / `setup init`，以及下面的三个会话入口。收到 SIGINT / SIGTERM 后停止接受新连接、等进行中的请求最多 10 秒、关库、删 socket、退出码 0。

### 初始化、登录与账号

- **初始化向导**：空库时先建第一个管理员。`satchel setup status` 报告是否已初始化与可走的路（本版本只有「建管理员」；恢复备份随 m1-07、导入 mmwx 随 M9）；`satchel setup init --username <名>`（可选 `--email`）在终端里读两遍密码，或在网页 / REST 上 `POST /api/v1/setup/init`，成功顺手下发会话 cookie。用户名 3 到 32 个字符、小写字母 / 数字 / `_` / `-`、以字母或数字开头；密码至少 8 个字符（bcrypt 存哈希）。库里已有用户后 `setup init` 是 `conflict`；两个并发的 init 只有一个成功。**初始化之前谁都能建这个管理员**（向导本来就不要身份），所以先在本机或内网完成 `setup init`，再把主控暴露到公网；来源 IP、Turnstile 与封禁随 m1-05 交付。
- **登录与会话**：`POST /api/v1/session`（`username` / `password` / 可选 `remember_me`）成功后下发 cookie `satchel_session`（HttpOnly、SameSite=Strict、Path=/，经 TLS 到达时带 Secure）：默认 24 小时，记住我 30 天。令牌是随机串，库里只存它的 SHA-256；`DELETE /api/v1/session` 登出。浏览器发来的写请求（身份来自会话 cookie 的，以及没有身份的登录入口与向导；`/api/v1/…` 与 `/mcp` 都算）要过同源检查：`Origin` 的 host 等于主控地址；没 `Origin` 时 `Sec-Fetch-Site` 不能是跨站；两个头都没有的非浏览器客户端放行。经 socket 与令牌来的请求不受影响。账号停用是 `forbidden`；用户名或密码不对都是同一条 `unauthenticated`。
- **两步验证与恢复码**：`satchel account totp setup` 给出密钥与 otpauth URL（扫进验证器），`account totp confirm --code <6 位>` 启用并一次性给出 8 枚恢复码（每枚 8 个十六进制字符，库里只存哈希）。开了两步验证后登录分两步：密码正确得到 5 分钟有效、只能用一次的 `pending` 票据，`POST /api/v1/session/two-factor`（`pending` + `code`）用验证器的码或一枚恢复码完成。同一个 TOTP 码 90 秒内只认一次；每枚恢复码只能成功一次（校验与作废在同一个数据库事务里，并发也只成功一次），用恢复码登录不会关掉两步验证；剩余不足两枚时登录结果与 `account show` 都有 `recovery_codes_low` 提示，`account recovery-codes regenerate` 重新生成 8 枚并作废旧的。`account totp disable` 关掉；已启用时再 `setup` 是 `conflict`，要换密钥先 disable。登录第二步验错一次，那张 5 分钟的 `pending` 票据就作废，要重新用密码登录。
- **当场验证**：`account set-password` / `account totp setup` / `account totp confirm` / `account totp disable` / `account recovery-codes regenerate` 是人类专属命令：每次执行都要在同一个请求里带上自己的密码（`verify-password`）与——账号开了两步验证时——第二因素（`verify-code`），验一次用一次，不签发任何提升票据。CLI 上密码只从终端读（`--verify-password` 不是命令行参数，给了就是用法错误），`--verify-code` 可以作参数也可以终端输入（恢复码建议终端输入，写在命令行上会留在 shell 历史与进程列表里）；stdin 不是终端时直接以 `human_required` 拒绝、不等待。本机管理员不是账号，要用 `--verify-user <管理员用户名>` 指明验谁；登录的用户只能验自己。REST 上这三个值放在 JSON 体里，它们永不进审计摘要。`account set-password --new-password`（终端读两遍）改完作废该账号其它全部会话、保留当前这一个。
- **忘了管理员密码**：在主控本机执行 `satchel admin reset-password <用户名> --confirm <用户名>`（本地命令，直接开数据目录里的库，主控在不在跑都行；只对管理员账号；新密码在终端里读两遍）。它作废该账号全部会话、不动两步验证，且因为不经主控而**不进审计**（stderr 会提示这一点）。

## REST 与 MCP

三个投影都从 `internal/command` 的命令表构造，一条命令登记进表就同时有 CLI 子命令、REST 路由与 MCP 可达；`docs/commands.md` 是由表生成的「命令 × scope 对照表」（`go generate ./internal/command/`，CI 守着一致）。

- **REST**：`/api/v1/…`，路径由命令路径推出（`read` 用 GET、flag 作查询参数；其余用 POST、flag 与 `confirm` 在 JSON 体里；`password` 类型的 flag 与当场验证的 `verify-*` 也在 JSON 体里；列表命令去掉末尾的 `list`，`limit` 默认 50、上限 500、`cursor` 翻页）。成功 200，body 与 CLI `--json` 是同一个对象；失败 body 是四字段错误，状态码按错误码折算（400 / 401 / 403 / 404 / 409 / 428 / 503 / 500）。未登记的键、类型不对、文件路径类参数（`-f` / `--filename` / `--file`）都是 `bad_request`；不兼容 mmwx 的 `/api/admin/*`。命令表之外只有四条路由：`GET /api/v1/healthz`、`POST /api/v1/session`（登录）、`POST /api/v1/session/two-factor`（第二步）、`DELETE /api/v1/session`（登出）。
- **MCP**：`/mcp`（Streamable HTTP，无状态），只有两个工具：`satchel_run`（`args` 命令数组 + 可选 `confirm`，输出恒为 JSON）与 `satchel_explain`（`target`）。命令数组交给与 CLI 相同的解析器、不经 shell；身份只来自这次连接，`args` 里的 `--token` / `--server`、本地命令（`version`、`db`、`serve`、`admin reset-password`）、人类专属命令、初始化向导的 `setup *`、文件路径参数一律拒绝。
- **审计**：每条经主控执行的命令（含被权限、confirm 或当场验证拒绝的）写一条 `audit_logs`，`satchel audit list` 看（只对管理员开放）；`password` 类型的 flag 在摘要里打码，当场验证的值不进摘要。无身份的请求被拒时不记，但不要身份的 `setup status` / `setup init` 执行了就记（`actor_kind` 为 `anonymous`）；`explain`、`healthz`、`/public/`、登录 / 登出入口不记。

现有经主控的命令：`whoami`（身份对象）、`audit list`（`--actor` / `--command` / `--since` / `--limit` / `--cursor`）、`explain [target]`。

## 安装

三种方式（技术方案第 09 章）。装完 `systemctl status satchel`（或 `rc-service satchel status`）能看到主控在跑，`curl http://127.0.0.1:12889/api/v1/healthz` 返回 `{"status":"ok",…}`。

```sh
# 一键脚本：裸机（Debian / Ubuntu / RHEL 系 / Alpine，systemd 或 OpenRC；POSIX sh，Alpine 也能跑）
curl -fsSL https://raw.githubusercontent.com/tajiaoyezi/satchel/main/install.sh | sudo sh
# 一键脚本：Docker（写 /opt/satchel/docker-compose.yml 与 .env 后 compose up）
curl -fsSL https://raw.githubusercontent.com/tajiaoyezi/satchel/main/install.sh | sudo sh -s -- --docker
# 选项（两条路都认 --version / --prerelease）：--version v0.1.0、--prerelease、--binary <本地文件>、--skip-verify（只对 --binary）、--no-start、--install-dir
```

脚本从 GitHub Release 下载二进制与 `.sig`（直连失败回退 gh-proxy），**用脚本自己内嵌的发布公钥（与 `pkg/release` 同一份）经 openssl 验签、验不过不落盘**——信任锚是这份脚本，不是刚下载的二进制——再装到 `/usr/local/bin/satchel`，数据目录 `/var/lib/satchel`，服务名 `satchel`。M0 只有预发布版：一键脚本要加 `--prerelease`。

Docker Compose：仓库根的 `docker-compose.yml` 与 `.env.example`（`cp .env.example .env` 后 `docker compose up -d`；`--profile postgres` 起本机的 PostgreSQL，主控走 host 网络所以 `SATCHEL_DATABASE_HOST=127.0.0.1`；`SATCHEL_LISTEN` 改监听地址）。镜像 `ghcr.io/tajiaoyezi/satchel`，入口脚本先跑 `db migrate` 再执行传入的子命令（默认 `serve`），`HEALTHCHECK` 打 `/api/v1/healthz`；容器内不原地替换二进制，升级换镜像 tag。

裸二进制：从 Release 下载对应平台的文件与 `.sig`，用已装的 `satchel __verify <file> <sig>`（或 openssl 加仓库里的公钥）核对后放到 PATH 里；`__verify` 是自升级用的验签入口，别拿刚下载的文件验它自己。六个平台里只有 Linux 两个承诺能跑主控，其它四个只保证客户端部分可用。

## 发布

打 tag 就是发版：`git tag v0.1.0 && git push origin v0.1.0`（tag 含 `-` 是 prerelease）。发布线（`.github/workflows/release.yml`）：`build` 六个平台自动跑 → `sign` 停在受保护环境 `release-signing` 等仓库拥有者批准，批准后用 `tools/sign` 给每个二进制签 Ed25519 分离签名（`<file>.sig`，64 字节）并用公钥验回、出 `checksums.txt` → `release` 建 GitHub Release（同 tag 已有 Release 即失败，不覆盖）→ `docker` 推多架构镜像（标签 `0.1.0`、`0.1`、`0`、`latest`；prerelease 是 `0.1.0-beta.1` 与 `beta`）。

私钥只在环境 secret `RELEASE_SIGNING_PRIVATE_KEY` 里，环境上还要有变量 `RELEASE_SIGNING_ARMED=1`（签名 job 用它确认环境是人手建好、设了审批人的，不是 GitHub 自动建的）；公钥清单在 `pkg/release`（编进二进制）与 `install.sh` 各一份、测试钉住一致，三个二进制共用一把发布密钥；轮换先发一版带新旧两把、下一版再去掉旧的。`satchel-agent` 与 `satchel-plugins` 的发布线检出本仓库的固定 tag 跑同一份签名程序。受保护环境要「必需审批人」，GitHub 免费套餐只在公开仓库上提供，所以三个 Go 仓库是公开的。本地演练：`go run ./tools/sign keygen` 生成一对测试密钥，`RELEASE_SIGNING_PRIVATE_KEY=<私钥> go run ./tools/sign sign <file>`，验回要么把测试公钥经 ldflags 注入（`-X github.com/satchel/satchel/pkg/release.publicKeysCSV=<公钥>`）再 `verify`，要么直接用 openssl——源码里的正式公钥和你的测试私钥不成对，`verify` 会失败是正常的。

## 数据库

主控默认用 SQLite（数据目录下的 `satchel.db`），可选 PostgreSQL（数据目录下的 `database.json` 写 `driver: postgres`，或用环境变量 `SATCHEL_DATABASE_*` 覆盖）。数据目录由 `--data-dir` 或环境变量 `SATCHEL_DATA_DIR` 指定，默认 `/var/lib/satchel`。

数据目录的布局是固定的，名字都是 `internal/base/db` 里的常量：`database.json`（数据库配置）、`config.yaml`（主控配置，可不存在）、`satchel.db`（SQLite 库文件）、`master.key`（主控通信密钥）、`satchel.sock`（主控运行时的 unix socket）、`subscribes/`（订阅文件）、`rule_templates/`（规则模板）、`public/`（`/public/` 对外提供的静态文件）。`db migrate` 与 `serve` 会把目录和三个子目录一起建出来（0700），postgres 模式也一样——库在别处，但主控密钥、订阅文件、规则模板、静态文件仍在这里；`db status` 是只读命令，不建目录。备份的内容表按这份布局取（socket 与 `public/` 不进备份）。

```sh
./satchel db migrate --data-dir ./data   # 执行迁移，然后比对库结构与注册表
./satchel db status --data-dir ./data    # 查看迁移状态与结构比对结果（只读，不建目录、不建库）
./satchel db unlock --data-dir ./data    # 清除上一次迁移被中断后残留的迁移锁
```

每条命令都支持 `--json`，输出是带 `apiVersion` 的 JSON 对象；失败时 stderr 是 `code`、`reason`、`state`、`next` 四字段（`--json` 时是 JSON 对象）。退出码按第 05 章的表：0 成功、1 一般失败、2 用法错误（未知子命令、未知 flag、多余参数、重复的 flag；只有 `usage` 是 2）、3 认证失败、4 权限不足、5 对象不存在、6 版本冲突、7 危险操作未确认、8 部分失败、9 人类专属操作未验证身份；请求内容不对（`bad_request`）、配置不合法（`config`）、连不上主控（`unavailable`）都是 1。

### 结构漂移提示

`db migrate` 跑完会把库里的实际表结构（列、类型、可空、默认值、CHECK、主键、唯一、索引、外键）与注册表逐项比对，发现差异时以错误码 `schema_mismatch`、退出码 1 结束，并列出每一处差异；库里已有表却没有迁移记账（`satchel_migrations` 表不存在）时不动库，同样报 `schema_mismatch` 并说明。`db status` 在全部迁移已应用、或迁移记账缺失时做同样的比对并把结果列在输出里（`--json` 里是 `schema` 一节：`checked`、`consistent`、`diff`、`extraTables`；没比对时 `consistent` 是 null）。出现这些提示说明库是按旧的 `0001_init` 建的，而注册表已经变了。处置按下面「开发期删库重建」；首个正式发布之后改为写新的迁移文件。库里有、注册表里没有的表（例如共用一个 PostgreSQL 库时别的应用的表）只在 `extraTables` 里列出，不算错误。

迁移过程被打断（进程被杀、断电）会在锁表里留下一把锁，之后 `db migrate` 报 `conflict` 并提示 `db unlock`；确认没有别的迁移在跑之后执行它即可。

### 本地起 PostgreSQL

存储层的测试在 SQLite 与 PostgreSQL 各跑一遍。本地没配 `SATCHEL_TEST_PG_DSN` 时 PostgreSQL 那一遍会跳过并提示；CI 设了 `SATCHEL_TEST_REQUIRE_PG=1`，缺库直接失败。

```sh
docker run -d --name satchel-pg -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16-alpine
export SATCHEL_TEST_PG_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
go test ./...
```

每个测试在自己的随机 schema 里跑，结束后删掉，测试之间不共享表。

### 重新生成

表结构的唯一来源是 `internal/base/schema` 的注册表，按功能簇分文件：`tables_agent.go`（Satchel 新增的 12 张 agent-native 表）、`tables_users.go`、`tables_packages.go`、`tables_servers.go`、`tables_nodes.go`（mmwx 的核心 kind 五簇）、`tables_subscriptions.go`、`tables_certificates.go`、`tables_traffic.go`、`tables_ops.go`、`tables_federation.go`、`tables_telegram.go`（mmwx 的照抄七簇）。现在共 87 张表、32 个 kind；配置类与动作类的 kind 表有自增整数 id、resource_version 与 deleted_at（主控设置类的单例只有 resource_version），每一列标了分档（spec、status、动作专属、人类专属、主控自身类）与是否打码。16 条自然键索引决定 `metadata.name`：单列自然键（name、username）直接用值，复合自然键（Inbound 的 server_id 加 tag、Certificate 的 domain 加 server_id、CustomRule 的 name 加 type 等）按列序用 `/` 连起来，撞上报 `name_taken`；没有自然键的 17 个 kind 没有 name，只按 id 寻址；name 由序列化时按 spec 算出，值里可能含 `/`，按名寻址要按列查而不是拆字符串。mmwx 默认 1 的 18 个布尔列在这里库默认 FALSE、标了「省略即为真」：apply 解码时没填就是 true（对更新也一样，apply 是整份替换，生成 spec 的一方要把布尔显式写出来），创建代码要照标记显式置 true；Satchel 自己新增的布尔列（通知渠道、自动化规则的 enabled）默认关，不在其列。指向 `users(username)` 的 17 条外键都是 ON UPDATE CASCADE，用户改名（第 05 章七组的专门操作，不是 spec 写）时引用它的行跟着改。用户的订阅令牌、会话、订阅设置、凭据表、批量追踪、可达性、流量账本、日志记录、邀请码、联邦记录这些附属表不是 kind，保留 mmwx 的主键形状，没有版本与软删除列。系统设置是一个单例 kind SystemSettings：`system_config` 单行（主键固定为 1，带 resource_version、不带 deleted_at）加 `system_settings` 键值表，版本号只有 system_config 那一个，改任何一列或任何一个 key 都要带它。键值表的 key 目录在 `tables_settings.go`：92 个 key 各带类型（bool / int / string / json）、分档（17 个人类专属、9 个主控自身类、58 个日常运维、1 个只读、7 个运行态）与是否打码（Turnstile 密钥、TG 机器人 token、外置探针令牌哈希三个），生成器把它们和列一起合成 SystemSettings 的 Spec / Status 结构体与字段清单，key 不进 DDL 与模型；mmwx 的 114 个 key 里另外 22 个不搬（License 与许可徽章、旧 api_token、Reality 共享池、一次性修复标记等），清单在测试里。键值表的 value 是文本：布尔读时接受 `1` / `0` / `true` / `false` / 空，写时统一 `true` / `false`；整数十进制；json 原文。改了注册表之后重新生成，并把生成物一起提交：

```sh
go generate ./internal/base/schema/
```

生成物有四份：两套迁移 SQL（`internal/base/db/migrations/{sqlite,postgres}/0001_init.tx.up.sql`）、bun 模型（`internal/base/model/zz_generated.go`）、kind 的 Spec / Status 结构体与字段清单（`pkg/api/v1/zz_generated_kinds.go`）。`go test` 会比对生成物与注册表，不一致即失败；CI 另外跑一遍 `go generate` 再看 `git diff`。

### 开发期删库重建

首个正式发布之前，`0001_init` 由注册表重新生成，不堆 0002、0003。这意味着注册表一变，开发机上已经迁移过的库就与新的 0001 对不上了，直接删掉重建：

```sh
rm -f ./data/satchel.db ./data/satchel.db-wal ./data/satchel.db-shm   # SQLite
psql "$SATCHEL_TEST_PG_DSN" -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'  # PostgreSQL
./satchel db migrate --data-dir ./data
```

开发期没有需要保留的数据。打第一个正式 tag 时冻结 `0001_init`，之后改表只能新增序号更大的迁移文件。

## 代码分层

六层，依赖只向下：契约（`pkg/api/v1`、`internal/command`）→ 投影（`internal/projection`）→ 横切（`internal/middleware`）→ 业务（`internal/service`）→ 仓储（`internal/core`）→ 基础设施（`internal/base`）。每个目录的 `doc.go` 写了该层的职责；引用规则由 `internal/layering_test.go` 钉住，违反即 `go test` 失败。

一条经主控的命令走的路：投影（cli / rest / mcp）把请求解成 `command.Invocation` → 执行链 `audit(authz(dispatch))`（横切层：留痕在最外层、权限在里面、最里面按绑定分发；身份由 `authn` 在 HTTP 层按连接判定后放进 ctx）→ service 的处理函数。命令表（`internal/command`）只放元数据，处理函数的绑定与各层的装配都在 `cmd/satchel`（`app.go`），端到端测试也在那里。CLI 进程里的执行链换成连 socket 的 REST 客户端；MCP 的 `satchel_run` 用同一棵 cobra 树解析、用主控进程内的链执行。

## 许可证

GPL-3.0，见 [LICENSE](LICENSE)。
