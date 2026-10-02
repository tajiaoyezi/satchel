# Satchel（百宝袋）

Agent-first 的多服务器代理管理系统。主控、CLI、MCP 与写给 AI 的 skills 在这一个仓库、一个二进制 `satchel` 里；节点守护是 [satchel-agent](https://github.com/satchel/satchel-agent)。

**状态：M1 主控基础阶段。主控能起来（`serve`）、REST / CLI / MCP 三个投影同构上线，初始化向导、网页登录与两步验证、系统设置、API 令牌与远程 CLI / AI runtime 接入、开局总览与 skills 可用，业务命令随后续 change 加入。**

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
| `SATCHEL_LOG_LEVEL` | 日志级别 `debug` / `info` / `warn` / `error`，对应 `log_level`；日志同时写 stderr 与 `<数据目录>/logs/satchel.log`，见「日志与定时任务」 | `info` |
| `SATCHEL_DATABASE_DRIVER` 等 | 数据库连接，见「数据库」一节 | SQLite |
| `SATCHEL_OUTPUT` | 设为 `json` 时 CLI 默认 JSON 输出（等价于 `--json`） | 文本 |
| `SATCHEL_SERVER` | CLI 要连的远程主控地址（等价于 `--server`），见「令牌与远程接入」 | 本机 socket |
| `SATCHEL_TOKEN` | CLI 带的 API 令牌（等价于 `--token`） | 不带 |
| `SATCHEL_FORCE_PUBLIC_ACCESS` | 「关闭公网访问」的自救开关：`1` / `true` / `yes` / `on` 时本进程跳过这一道门（只在 `serve` 启动时读，不改设置），见「门与登录防护」 | 关 |
| `SATCHEL_ALLOWED_ORIGINS` | 允许跨域调用的网页来源，逗号分隔的 `http(s)://` origin 或单独一个 `*`；只给带令牌的调用用，不带 cookie | 只允许同源 |

主控同时监听 TCP 与数据目录下的 unix socket `satchel.sock`（0600）。**身份只从连接判定**，按顺序取第一个：请求带了 `Authorization` 头就只看令牌——`Bearer <有效令牌>` 是令牌身份，别的一律是无效凭据、`unauthenticated`，不再往下看（见「令牌与远程接入」）；经 socket 进来、对端是 root 或运行主控的那个 OS 用户 → 本机管理员（全部权限，socket 上带的 cookie 不看）；TCP 上带有效会话 cookie → 登录的用户（管理员全部权限，普通用户只有 `read` + `operate`、没有危险类）；其余一律没有身份。没有身份能到的只有：`GET /api/v1/healthz`、`/public/<file>`（数据目录 `public/` 里的文件，目录不列、`..` 出不去）、初始化向导的 `setup status` / `setup init` / `setup restore`，以及下面的四个会话入口。经 TCP 的请求在判定身份之前先过三道门（见「门与登录防护」），经 socket 的不受影响。收到 SIGINT / SIGTERM 后停止接受新连接、等进行中的请求、正在跑的内置任务与长任务（同时等，各自最多 10 秒）、关库、删 socket、退出码 0。

### 初始化、登录与账号

- **初始化向导**：空库时先建第一个管理员。`satchel setup status` 报告是否已初始化与可走的路（空库时可以建管理员或恢复一份 Satchel 备份；导入 mmwx 随 M9）；`satchel setup init --username <名>`（可选 `--email`）在终端里读两遍密码，或在网页 / REST 上 `POST /api/v1/setup/init`，成功顺手下发会话 cookie。用户名 3 到 32 个字符、小写字母 / 数字 / `_` / `-`、以字母或数字开头；密码至少 8 个字符（bcrypt 存哈希）。库里已有用户后 `setup init` 是 `conflict`；两个并发的 init 只有一个成功。也可以不建管理员，直接 `satchel setup restore --file <备份.zip>`（或 `POST /api/v1/setup/restore`，请求体是备份文件本身）把一份备份恢复进空库，主控随后重启，见「备份与恢复」。**初始化之前谁都能建这个管理员、也能上传备份**（向导本来就不要身份），所以先在本机或内网完成初始化，再把主控暴露到公网。
- **登录与会话**：`POST /api/v1/session`（`username` / `password` / 可选 `remember_me`；Turnstile 启用时还要 `turnstile_token`）成功后下发 cookie `satchel_session`（HttpOnly、SameSite=Strict、Path=/，经 HTTPS 到达时带 Secure：TLS 直连，或经登记的反代且标了 `X-Forwarded-Proto: https`）：默认 24 小时，记住我 30 天。令牌是随机串，库里只存它的 SHA-256；`DELETE /api/v1/session` 登出。浏览器发来的写请求（身份来自会话 cookie 的，以及没有身份的登录入口与向导；`/api/v1/…` 与 `/mcp` 都算）要过同源检查：`Origin` 的 host 等于主控地址；没 `Origin` 时 `Sec-Fetch-Site` 不能是跨站；两个头都没有的非浏览器客户端放行。经 socket 与有效令牌来的请求不受影响。账号停用是 `forbidden`；用户名或密码不对都是同一条 `unauthenticated`；猜错太多次被登录限流锁住时是 `rate_limited`（见「门与登录防护」）。
- **两步验证与恢复码**：`satchel account totp setup` 给出密钥与 otpauth URL（扫进验证器），`account totp confirm --code <6 位>` 启用并一次性给出 8 枚恢复码（每枚 8 个十六进制字符，库里只存哈希）。开了两步验证后登录分两步：密码正确得到 5 分钟有效、只能用一次的 `pending` 票据，`POST /api/v1/session/two-factor`（`pending` + `code`）用验证器的码或一枚恢复码完成。同一个 TOTP 码 90 秒内只认一次；每枚恢复码只能成功一次（校验与作废在同一个数据库事务里，并发也只成功一次），用恢复码登录不会关掉两步验证；剩余不足两枚时登录结果与 `account show` 都有 `recovery_codes_low` 提示，`account recovery-codes regenerate` 重新生成 8 枚并作废旧的。`account totp disable` 关掉；已启用时再 `setup` 是 `conflict`，要换密钥先 disable。登录第二步验错一次，那张 5 分钟的 `pending` 票据就作废，要重新用密码登录。
- **当场验证**：`account set-password` / `account totp setup` / `account totp confirm` / `account totp disable` / `account recovery-codes regenerate` 是人类专属命令：每次执行都要在同一个请求里带上自己的密码（`verify-password`）与——账号开了两步验证时——第二因素（`verify-code`），验一次用一次，不签发任何提升票据。CLI 上密码只从终端读（`--verify-password` 不是命令行参数，给了就是用法错误），`--verify-code` 可以作参数也可以终端输入（恢复码建议终端输入，写在命令行上会留在 shell 历史与进程列表里）；stdin 不是终端时直接以 `human_required` 拒绝、不等待。本机管理员不是账号，要用 `--verify-user <管理员用户名>` 指明验谁；登录的用户只能验自己。REST 上这三个值放在 JSON 体里，它们永不进审计摘要。`account set-password --new-password`（终端读两遍）改完作废该账号其它全部会话、保留当前这一个。
- **忘了管理员密码**：在主控本机执行 `satchel admin reset-password <用户名> --confirm <用户名>`（本地命令，直接开数据目录里的库，主控在不在跑都行；只对管理员账号；新密码在终端里读两遍）。它作废该账号全部会话、不动两步验证，且因为不经主控而**不进审计**（stderr 会提示这一点）。

### 开局总览

`satchel overview`（REST `GET /api/v1/overview`，MCP 上是 `satchel_run` 的 `["overview"]`）一条命令返回全系统概况，解决 AI 每次连上来的冷启动：服务器、用户、告警、待办四个分区，每个分区是 `{items, total}`（`items` 至多 10 条，`total` 是调用者看得到的总数）。任何有身份的调用者都能看（本机管理员、管理员与普通用户的会话、任何令牌），普通用户只看到自己的数据。分区随对应功能出现：服务器随 M2、用户随 M3、告警与待办随 M4，在那之前是空列表（`items` 为 `[]`、`total` 为 0），不报错也不省掉。文本形式每个分区一行：`服务器：0`、`用户：0`、`告警：0`、`待办：0`。它只读，不发外部请求。

### 系统设置

系统设置是**一个单例对象**（kind `SystemSettings`，第 07 章「主控设置类」）：`system_config` 的列与 `system_settings` 键值表的 93 个 key 合在一起，整单一个 `resourceVersion`。`serve` 启动时（迁移之后、监听之前）确保那一行存在，空库起来就是版本 1。`settings *` 只对管理员开放（本机管理员与管理员账号），普通用户 `forbidden`。

- **读**：`satchel settings show`（`--json` 是资源信封）。`spec` 是日常运维档的 100 个字段（既有列如 `heartbeat_interval`，也有 key 如 `branding_site_title`），`status` 是其余四档（七组人类专属如 `master_url`、主控自身类如 `update_cdn_enabled`、只读的 `require_encryption` 恒为 true、运行态如 `master_https_recovery_pending`）。键值表里没有的 key 按默认值表补（照 mmwx 读侧的 fallback，`default_theme` 默认 `flat`）；打码字段（`telegram_bot_token`、`turnstile_secret_key`、`tgbot_token`、`probe_external_token_sha256`）只对带 `secrets` scope 的身份给原文（本机管理员、管理员账号的会话、打开了密钥读取的令牌），其余身份非空时输出 `***`；审计摘要里一律打码。字段清单与分档看 `satchel explain SystemSettings`。
- **写日常运维档**：`satchel settings set --set <字段>=<值> [--set …] --resource-version <N>`，REST 是 `POST /api/v1/settings/set`，体 `{"set":{"heartbeat_interval":45,"branding_site_title":"Satchel"},"resource-version":3}`。一次可以改任意多个字段，列与 key 混着给；服务端先整体校验——字段必须是 `spec` 里的（别的档一律 `field_not_applyable` 并点名分档，不认识的 `unknown_field`），值是字符串时按键值表的编码规则解析（布尔 `true` / `false` / `1` / `0` / 空，整数规范十进制，json 必须是合法 JSON 文本），JSON 原生类型直接收，再过照 mmwx 抄来的字段规则（如 `subscription_output_format` 只收 `yaml` / `json`、`default_theme` 四个主题名、`heartbeat_interval` 至少 5、`dashboard_refresh_interval_ms` 在 1000 到 60000 之间；mmwx 静默改写的地方这里一律拒绝并说明范围；Satchel 另加了两条 mmwx 没有的形状规则：`probe_external_token_sha256` 要是 64 位小写十六进制，`login_wallpaper` 与探针 logo 一样只收 `/`、`http(s)://`、`data:image/` 开头的引用）——任一字段不过整单拒绝、不部分写入。然后在**一个事务**里：比对 `resourceVersion`（不匹配 `version_conflict`，退出码 6；没有跳过比对的写法，冲突了重新读一遍再改，`--force` 不是参数）→ 存一份写前快照 → 写两张表 → 版本加 1。打码字段交回 `***` 表示保持不变，空串表示清掉。成功返回写后的整个对象，新版本在 `metadata.resourceVersion` 里。
- **快照与回滚**：每次成功的 `settings set` / `settings rollback` 在 `config_snapshots` 里追加一行写前的日常运维档（七组字段不进快照；主控自身类字段随 m1-08 的第一条写命令再进）。`satchel settings snapshots list` 按时间倒序列出（id、object_version、created_at、source、content_hash；不给内容，里面有原文密钥）；`satchel settings rollback <id> --resource-version <N>` 把那份快照的内容当成一次 `settings set` 写回：重过字段分档与规则、同样比对版本、同样先存写前快照——回滚本身也能被回滚。它不走 apply、不把版本号倒回去。
- **主控地址（七组）**：`satchel settings master-url set --url <主控地址> --subscription-url <订阅域名> --resource-version <N>` 是人类专属命令：要当场验证（见上一节），MCP 与令牌一律 `human_required`。两个至少给一个；值必须是干净的 HTTP(S) origin（只有 scheme 与 host，可带端口；末尾斜杠去掉），空串表示清掉。它同样比对并抬版本，但不存快照。改完不推送到节点、不做主控迁移（随 M2 的节点通道）。
- **门（七组）**：关闭公网访问、静默模式、隐藏登录入口、登录限流与封禁的参数、Turnstile 的两个 key、反代登记这 15 个字段只能用 `satchel settings gates set --set <字段>=<值> … --resource-version <N>` 写：人类专属（当场验证），别的字段一律 `field_not_applyable`，同样比对并抬版本、不存快照，写完下一个请求就按新值判定，不用重启。见「门与登录防护」。
- **哪些 key 什么时候生效**：本 change 交付的是存储与写路径；更新 CDN 开关随 m1-08，通知参数随 M4，TG 机器人随 M5，HTTPS 自愈与 `external_https` 随 M6，采集间隔与 agent 日志开关下发到节点随 M2。

### 令牌与远程接入

远程 CLI、AI runtime 与脚本进主控用的凭据只有一种：API 令牌（第 05 章功能②）。

- **令牌与权限范围**：令牌是 `sat_` 加 43 个字符的随机串，库里只存它的 SHA-256，明文只在签发的那一次输出里出现。权限范围由 `read`（恒有）、`operate`、六个危险类（`delete`、`restart`、`permission`、`batch`、`exec`、`master`）与单独的 `secrets`（密钥读取）组成；预设由它推出：没有 `operate` 是 `readonly`，有 `operate` 且六类全开是 `full`，其余是 `ops`。命令上 `--preset readonly|ops|full` 把 `operate` 与危险类设成该预设的样子，`--danger <类>`（可重复）把危险类设成恰好这几个并隐含 `operate`，`--secrets` 开关密钥读取；`--preset readonly` 带 `--danger` 是 `bad_request`。新建时默认只读、不过期（`--expires-in 720h` 设过期时间）。
- **签发者与上限**：令牌挂在签发者的账号名下，权限上限是签发者的角色：管理员什么都能签；普通用户只能签 `read` 与 `operate`，要带危险类或 `secrets` 直接 `forbidden` 并点名超出的项，不会悄悄截掉。令牌每次被使用时，生效的权限是它的权限范围与签发者**当下**角色的交集：签发者被降为普通用户后危险类与密钥读取随之失效，签发者停用或删除后令牌无效。
- **签发、改、吊销**：`satchel token create --name <名字> [--preset …] [--danger …] [--secrets] [--expires-in …] [--runtime <标签>]`、`token update <id>`（改名字、权限范围、过期时间，至少给一项）、`token revoke <id>` 是人类专属命令（当场验证见上文），令牌不能签令牌（REST 与 MCP 上都是 `human_required`）。改权限与吊销立刻生效，令牌字符串不变；已吊销的不能再改。`token list` 列出令牌、`state`（`active` / `revoked` / `expired`）与最后使用时间（按分钟记）；普通用户只看得到、改得了自己的令牌，别人的一律 `not_found`，管理员可用 `--owner` 过滤。
- **第一把令牌在哪签**：在主控本机经 socket 执行，例如 `satchel token create --name laptop --preset ops --verify-user admin`（本机管理员不是账号，要用 `--verify-user` 指明一个管理员，令牌挂在它名下），或在网页上签（随 m1-11）。远程只带令牌的 CLI 签不了新令牌。
- **远程 CLI**：连哪个主控、带哪把令牌，各自按「根 flag（`--server`、`--token`）→ 环境变量（`SATCHEL_SERVER`、`SATCHEL_TOKEN`）→ 登录文件」的顺序取第一个有的；都没有 server 就连本机 socket。`--server` 可以带路径前缀（主控挂在反代的子路径下）。`--token` 会留在进程列表与 shell 历史里，只适合临时试一下；脚本用环境变量；常用的机器用 `satchel login --server <地址>`：令牌从终端读，先用它调一次 `whoami` 确认是令牌身份再存进登录文件——用户配置目录下的 `satchel/login.json`（Linux 上 `~/.config/satchel/`，macOS 上 `~/Library/Application Support/satchel/`），文件 0600，权限对组或其他用户开放时拒绝使用。登录文件里的令牌只发给它自己记的那个 server：用 `--server` 或环境变量指向别的主控时不会带上它。`satchel logout` 只删登录文件，令牌在服务端仍然有效，吊销用 `token revoke`。本地命令（`version`、`db *`、`serve`、`admin reset-password`、`logout` 等）显式带 `--server` / `--token` 是用法错误，免得以为在改远端、实际开了本机的库；环境变量不算。人类专属命令（`token create` / `update` / `revoke`、`account set-password` 等）只能在主控本机经 socket、不带令牌执行：CLI 连的是远程主控或带着令牌时直接 `human_required`，不问密码，也就不会把密码发出去（远程 CLI 没有人的身份）。
- **本机配了令牌**：带了令牌就按令牌算，经 socket 也一样：主控本机的进程设了 `SATCHEL_TOKEN`，就只有这把令牌的权限，不再是本机管理员。带了无效的令牌（不存在、已吊销、已过期、签发者停用，或 `Authorization` 头不是 Bearer）一律 `unauthenticated`，连初始化向导也拒，reason 不区分是哪一种，不会退回本机管理员或会话身份，也不记审计；`/api/v1/healthz` 与 `/public/` 不受影响。
- **明文 HTTP 的风险**：用 `http://` 把令牌、或终端里输入的密码（例如远程执行 `setup init`）发给回环地址以外的主控时，CLI 会在 stderr 提示一行（密码在输入之前提示；输出是 JSON 时不提示，stderr 只留给错误）：同一网络上的人能截获它们。经公网访问请给主控配 HTTPS。
- **接 AI runtime**：`satchel mcp init --runtime claude-code|codex|hermes` 在主控本机经 socket 签一把令牌（要加 `--verify-user`；预设默认 `ops`，名字与 runtime 标签默认 `<runtime>@<主机名>`，`--preset` / `--name` 可改；远程 CLI 签不了，用 `--use-token` 改用一把已有的令牌），写进 runtime 的配置。写进配置的主控地址取 `--url`，不给就用 CLI 连的 server，都没有就按 serve 的监听地址推出本机地址（`0.0.0.0:12889` 推成 `http://127.0.0.1:12889`）；它是非本机的 `http://` 时会提示一行（输出是 JSON 时不提示）。签发之前先请求这个地址的 `/api/v1/healthz`，连不上或不是 Satchel 主控就停下；签出来的令牌先对它调一次 `whoami`，那里认不出这把令牌（不是签发它的主控）就不写配置、提示吊销它。
  - Claude Code：先 `claude mcp remove --scope user satchel` 再 `claude mcp add --scope user satchel -- <satchel 的绝对路径> mcp stdio`（已经登记了同样的就都不跑；add 失败时把原来的登记交还给你），三个变量 `SATCHEL_SERVER`、`SATCHEL_TOKEN`、`SATCHEL_OUTPUT=json` 写进 Claude Code 配置目录（`CLAUDE_CONFIG_DIR`，没设时 `~/.claude`，见下一节）下 `settings.json` 的 `env`（顶层其它键与顺序不变）；判断「已经登记过」读的是配置目录下的 `.config.json`（存在时），否则设了 `CLAUDE_CONFIG_DIR` 时是 `$CLAUDE_CONFIG_DIR/.claude.json`、没设时是 `~/.claude.json`。`satchel mcp stdio` 是给只支持本地进程方式的 runtime 用的垫片：连上主控的 `/mcp`，先确认令牌有效，再把两个工具原样转给 runtime；stdout 只走协议。
  - Codex：`$CODEX_HOME/config.toml`（默认 `~/.codex/config.toml`）的 `[mcp_servers.satchel]` 里的 `url` 与 `http_headers.Authorization`（Bearer 加令牌；块里你写的其它键，如 `enabled`、`disabled_tools`、超时，原样保留），以及 `[shell_environment_policy]` 的 `set` 里的三个变量。块是 stdio 写法或配了 `bearer_token_env_var` 时先停下。注意：Codex 把受信任（trusted）项目里的 `.codex/config.toml` 与用户级配置按键合并，项目层只写一个 `[mcp_servers.satchel]` 的 `url`，你的令牌就会随用户级的 `http_headers` 发往那个地址（已对 Codex 0.156.1 的源码与正式二进制核实）；只把你信任的仓库标为 trusted。你配了 include 过滤（`include_only` 或 `filters`）且不放行 `SATCHEL_*` 时先停下，打印要手工加的 include。
  - Hermes：`$HERMES_HOME/.env`（默认 `~/.hermes/.env`）里的三个变量，`config.yaml` 的 `mcp_servers.satchel` 里的 `url` 与 `headers.Authorization`（`Bearer ${SATCHEL_TOKEN}`，令牌明文只在 `.env` 里；条目里你写的其它键原样保留），以及 `terminal.env_passthrough`。

  只动 Satchel 自己的键（你在 satchel 条目里写的 `enabled: false` 之类不改，输出会提示接入后仍是停用的）；改已有文件前先备份成 `<文件>.satchel-bak-<时间戳>`（0600），先写临时文件再改名、保留原权限，但写进令牌的 `settings.json`、`config.toml`、`.env` 会去掉组与其他用户的权限（输出里注明）；新文件 0600、新目录 0700。原来配置里的另一把令牌不会被吊销，输出会提示你用 `mcp status` 找到后 `token revoke`；任何一个文件解析不了或写法不在支持范围内，在签发令牌之前就停下（`config`）并打印要手工加的片段。`--print` 只打印片段与命令、不写文件（这时输出里有令牌明文）。令牌签出来之后写文件失败是 `partial_failure`（退出码 8），输出里带令牌明文与片段。改完重启 runtime，验证命令分别是 `claude mcp get satchel`、`codex mcp get satchel --json`、`hermes mcp test satchel`。写好配置之后，同一条命令再把 skills 装进 runtime 的 skills 目录，见下一节。
- **谁在连**：`satchel mcp status` 列出绑了 runtime 标签的令牌，按最后使用时间倒序，从没用过的排最后；可见范围与 `token list` 相同。

### skills：写给 AI 的操作手册

七份 skills 编在 satchel 二进制里（源文件在 `internal/base/skills/files/`，每个 skill 一个目录、里面只有一个 `SKILL.md`），`satchel mcp init` 接入时一起装进 runtime 的用户级 skills 目录，runtime 按场景自动加载。skills 只是说明，不是权限：令牌的权限范围、危险类与人类专属始终由主控判定。

| skill | 讲什么 |
|---|---|
| `satchel-basics` | 开局（`overview`、`whoami`）；按需学命令（`--help`、`explain`，版本不同时用 MCP 的 `satchel_explain` 核对）；输出与四字段错误、退出码；长任务；危险操作与 `--confirm`；人类专属命令怎么请人在主控本机执行；MCP 上怎么调用、哪些做不了；远程用 CLI |
| `satchel-troubleshoot` | 日志、内置定时任务、长任务、审计与安全事件；主控起不来、管理员忘了密码、被门或 IP 封禁挡在外面时请人在主控本机处理 |
| `satchel-backup` | 生成、列出、上传备份；下载与恢复要请人做；新装的主控走初始化向导 |
| `satchel-upgrade` | 检查更新、升级主控、更新 CDN 的开关、升级失败时的自动回退 |
| `satchel-settings` | 日常运维档的设置、写前快照与回滚；主控地址与门这两组要请人改 |
| `satchel-access` | 令牌与谁在连；签发、改权限、吊销令牌与接入 runtime 要请人做；账号；IP 封禁 |
| `satchel-database` | 看当前的数据库、试连 PostgreSQL、从 SQLite 在线迁移 |

`docs/skills.md` 是由命令表与 skills 生成的「命令 × skills 对照表」（与 `docs/commands.md` 同一步 `go generate ./internal/command/`）。测试保证命令表里每条非隐藏命令都至少有一个 skill 讲到，skills 里写的每条 `satchel` 命令行都能在这个二进制的 CLI 上解析，CI 守着生成物与命令表和 skills 一致。

- **装在哪**：

  | runtime | skills 目录 |
  |---|---|
  | Claude Code | 配置目录下的 `skills/`：`$CLAUDE_CONFIG_DIR/skills`，没设时 `~/.claude/skills` |
  | Codex | `~/.agents/skills`（Codex 0.95 起的用户级目录，跟着 `HOME` 走、与 `CODEX_HOME` 无关；更早的 Codex 用不上 skills） |
  | Hermes | `$HERMES_HOME/skills`（默认 `~/.hermes/skills`）；用 profile 的，先把 `HERMES_HOME` 设成那个 profile 的目录再跑 |

  每个 skill 写成 `<skills 目录>/satchel-<名字>/SKILL.md`。只写这几个文件：内容相同的不写，不同的直接覆盖、不留备份；同名目录里别的文件、skills 目录里别的 skill 都不动；新文件 0600、新目录 0700。skills 目录本身可以是符号链接（照常跟随）；某个 `satchel-*` 目录或其中的 `SKILL.md` 是符号链接、位置被普通文件占着或读不了时，在签发令牌之前停下（`config`），修好那个路径后重跑。
- **生效**：重启 runtime 之后生效。Claude Code 与 Codex 运行中通常几秒内自动发现（Claude Code 的 skills 目录是这次新建的，要执行 `/reload-skills`）；Hermes 没有热加载，交互界面、gateway、桌面版这些长期运行的进程要重启（不重启时在会话里执行 `/reload-skills` 可以先用上，系统提示里的 skills 索引要到重启才更新）。三个 runtime 都不用打开任何开关。
- **顺序与失败**：先写 MCP 配置、执行 runtime 的命令，再写 skills。写配置失败时 skills 不写，`next` 提示配置补好之后执行 `satchel mcp init --runtime <x> --skills-only`；写 skills 失败时配置已经写好，以 `partial_failure`（退出码 8）结束，不再给令牌明文，`state` 里带着配置部分的结果（主控地址、令牌 id、改了的文件与备份、提示），`next` 同样指向 `--skills-only`。`--print` 不读写 skills，只在输出里给出该装的目录。
- **只装 skills**：`satchel mcp init --runtime <x> --skills-only` 只写 skills：不连主控、不签令牌、不改配置，主控没在跑也行。它只和 `--runtime` 一起用，同时给 `--use-token`、`--print`、`--url`、`--name`、`--preset`、`--verify-user`、`--verify-code` 或 `--server`、`--token` 是用法错误。升级 satchel 之后用它更新 skills。`satchel-*` 这几个目录归 Satchel 管，手改的内容会被下次 `mcp init` 覆盖；不想要 skills，删掉这几个目录即可，MCP 配置不受影响。
- **谁来跑**：`mcp init` 写的是执行它的那个用户的目录。在主控本机用 `sudo` 跑时写进的是 root 的目录；给普通用户的 runtime 装 skills，由那个用户自己执行 `--skills-only`（root 写出的 0700 目录，别的用户读不到）。
- **Claude Code 的配置目录**：在启动 Claude Code 的环境里设了 `CLAUDE_CONFIG_DIR` 时，`settings.json`、用户级 MCP 登记与 skills 都在那个目录，`mcp init` 跟着它走。它是空值或相对路径（例如字面的 `~/cfg`，Claude Code 不展开 `~`）时以 `config` 停下；只写在 `~/.claude/settings.json` 的 `env` 里时也以 `config` 停下：这样设时 Claude Code 只把 `settings.json` 与 skills 换到那个目录，MCP 登记仍在 `~/.claude.json`，照 `next` 把它挪到启动 Claude Code 的环境里（如 shell 的配置文件）再跑。这两项检查在签发令牌与写任何文件之前做，`--print` 与 `--skills-only` 也做。设在 VS Code 扩展或桌面版的启动环境里的，satchel 看不到：按输出里列出的目录手工调整。
- **别的客户端**：没有自动安装，照 `internal/base/skills/files/` 手工复制到它的 skills 目录。
- **从没有 skills 的版本升级**：主控与各处的 CLI 一起升级（skills 装的是本机 CLI 这个二进制里的版本）。已经接入的 runtime，升级之后执行一次 `satchel mcp init --runtime <x> --skills-only`。另外两种 Claude Code 用户：
  - 在启动环境里设了 `CLAUDE_CONFIG_DIR` 的：以前的 `mcp init` 把环境变量写进了 `~/.claude/settings.json`，Claude Code 没有读它。重跑一次完整的 `mcp init`（可以 `--use-token` 沿用原来的令牌）；之后 `~/.claude/settings.json` 的 `env` 里那三个 `SATCHEL_*`（含令牌明文）和旁边的 `.satchel-bak-*` 都没用了，可以删掉。没用 `--use-token` 时，`satchel token list` 里会有两把同名的 `claude-code@<主机名>`，吊销 id 不是这次输出里令牌 id 的那一把（id 较小的是原来那把）；也可以重跑时用 `--name` 给新令牌另起名字。
  - 只在 `~/.claude/settings.json` 的 `env` 里设了它的：以前的配置是生效的；重跑时会先以 `config` 停下，照 `next` 把它挪到启动环境里再跑。

### 门与登录防护

主控门口的防护照搬 mmwx，有几处不照抄（技术方案第 10 章）。改这些设置都是 `settings gates set`（七组，当场验证）；经数据目录下 unix socket 的请求不受这一节的任何门、限流与封禁影响：能连上那个 0600 socket 的只有 root 与运行主控的用户。

- **来源 IP 与反代登记**：来源 IP 只认 TCP 连接的对端地址，请求头一律不看；只有对端落在 `trusted_proxies` 登记的某一项里时，才按那一项的头取客户端地址（`X-Real-IP`、`CF-Connecting-IP` 取整个值；`X-Forwarded-For` 从右往左跳过登记过的地址，取第一个不在登记里的——最左边是客户端自己能随便写的），取不到合法地址就用对端地址。经登记的反代进来的请求**不算本机**；登记的反代标了 `X-Forwarded-Proto: https` 时这个请求算经 HTTPS 到达（会话 cookie 带 Secure）。常见写法：
  - Cloudflare Tunnel（`cloudflared` 与主控同机）：`--set 'trusted_proxies=[{"cidr":"127.0.0.1/32","header":"CF-Connecting-IP"}]'`。不登记的话，封禁与限流看到的都是回环地址。
  - 同机 nginx 反代：`[{"cidr":"127.0.0.1/32","header":"X-Forwarded-For"}]`，nginx 里 `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;` 与 `proxy_set_header X-Forwarded-Proto $scheme;`。
  - 反代在另一个 Docker 容器里：登记那个 Docker 网络的网段（如 `172.18.0.0/16`）。
  - 登记的范围越宽，能伪造来源 IP 的人越多；登记成 `0.0.0.0/0` 等于谁都能冒充任何地址。至多 64 项。
  - 登记的范围里不能有真实的客户端：落在登记里的地址会被当成反代跳过，这时它自己在头里写的地址会被当成来源 IP，可以冒充别人；写一个内网地址的话，`skip_local_ip` 开着时它的 IP 维度就不计了。例如 VPN 或内网用户会直接连主控时，不要登记整个 `10.0.0.0/8`，只登记反代自己的地址。
  - 登记回环地址等于相信本机上每一个能连 TCP 的进程：它们都可以在头里写任意地址（冒充别的 IP，让它被封，或留下误导的安全事件）。TCP 上分辨不出对端是 cloudflared 还是别的进程；本机上跑着你不信任的程序时，把反代放到单独的地址上（例如 Docker 网络），只登记那个地址。
  - 把回环地址登记成反代之后，从回环进来的请求都不算本机：打开「关闭公网访问」时，同机经回环打 `/api/v1/healthz` 的健康检查（例如 host 网络下 Docker 的 HEALTHCHECK）也会被拦下。
- **三道门**（第 10 章那张表；按「关闭公网访问 → 静默模式 → IP 封禁」的次序，在判定身份之前）：

  | 入口 | 关闭公网访问 | 静默模式（锁定期） | 隐藏登录入口 |
  |---|---|---|---|
  | 面板：网页、登录与验证码入口、`/api/v1/` 下的接口 | 只放本机或主控域名 | 一律 404，有会话也一样 | 只作用在探针伪装页上（随 M7） |
  | MCP：`/mcp` | 同面板接口 | 放行 | 无关 |
  | 机器入口：本版本是 `/api/v1/healthz` 与 `/public/`（节点通道、订阅、探针、TG webhook 等随后续里程碑加入） | 同上 | 放行 | 无关 |
  | 本机 unix socket 的 CLI | 不受影响 | 不受影响 | 不受影响 |

  - **关闭公网访问**（`master_local_only`）：只放行本机来的请求，以及主控地址是 `https://` 时 `Host` 等于它的域名的请求（主控地址不是 https 时只放本机）。被拦下的网页请求 307 跳到主控地址，其余是 `forbidden`。它不改监听地址（监听地址只看 `config.yaml` 与 `SATCHEL_LISTEN`）。`Host` 头可以伪造，这道门挡的是 IP 扫描与明文直连，更严的限制在防火墙。
  - **静默模式**（`silent_mode`、`silent_mode_timeout`，默认 15 分钟）：面板那一行在锁定期内回与不存在的路径一模一样的 404，不带任何表明静默模式的头；主控启动后的 `silent_mode_timeout` 分钟内不锁（重启主控可以临时进面板）。「有效用户拉到订阅就对所有来源开放若干分钟」随 M3 的订阅入口。在网页上打开它，自己也会立刻进不去。
  - **隐藏登录入口**（`probe_disguise_block_login`，mmwx 的「阻止登录」）：只作用在探针伪装页上，伪装页随 M7；本版本里它只存不生效，登录照常。
- **登录限流**（`login_rate_max_attempts` / `login_rate_window_minutes` / `login_rate_lock_minutes`，默认 1 小时内 5 次、锁 1 小时）：猜密码按来源 IP 与账号名两个维度分别计数，达到上限的那一次之后锁定，锁定期内登录与当场验证直接 `rate_limited`（HTTP 429，退出码 1，`state.until` 是解锁时间），不再比对密码。与 mmwx 不同：两步登录时密码对了不清零，第二步错了照计，整个登录成功才清零；当场验证的密码或验证码比对不上也计入（只缺第二因素不计）；账号已停用时密码对了也算一次失败。同时发来的一批尝试最多只有上限那么多次会去比对密码，其余直接 `rate_limited`。`skip_local_ip`（默认开）时本地与内网地址不计 IP 维度，账号维度照计。有人故意猜错你的用户名会把你的网页登录锁一阵子，这时在主控本机用 CLI 不受影响。
- **令牌猜测的封禁**（`brute_force_enabled` / `brute_force_max_failures` / `brute_force_window_minutes` / `brute_force_block_minutes`，默认 24 小时内 5 次、封 24 小时）：经 TCP 的请求带了无效的 `Authorization` 头，按来源 IP 计一次，达到上限自动封禁这个 IP，写进 `ip_bans`，重启后恢复。被封的 IP **只有带 `Authorization` 头的请求**被拒（`forbidden`），网页会话与登录照常（猜密码由登录限流管）——所以一个配着已吊销令牌、不停重试的 AI 客户端不会把你的网页也封掉。`skip_local_ip` 开着时本地与内网地址不计也不封。关掉 `brute_force_enabled` 只停自动封禁，已有的封禁照常生效（mmwx 关掉会让全部封禁失效）。手动：`satchel security ban <ip> [--permanent]` 与 `security unban <ip>` 是人类专属命令，`security bans list` 列出生效中的封禁（都只对管理员开放）。
- **安全事件**：登录与当场验证比对不上（`login_fail` / `login_locked`、`verify_fail` / `verify_locked`）、令牌校验失败（`probe`）、自动封禁（`ban`）、手动封禁（`ban_manual`）、解封（`unban`）各记一条，含来源 IP、路径或命令名、账号名与「第几次 / 上限」。`satchel security events list [--kind …] [--ip …]` 按时间倒序看（只对管理员开放）。保留 90 天，见「日志与定时任务」。
- **Turnstile 登录验证码**：`turnstile_site_key`（非空时至少 20 个字符）与 `turnstile_secret_key` 两个都填才启用。启用后网页登录要带 `turnstile_token`，主控先向 Cloudflare 核对：没带或没过是 `bad_request`（不算一次猜密码），连不上 Cloudflare 是 `unavailable`。登录页用不要身份的 `GET /api/v1/session/captcha` 取 `enabled` 与 `site_key`（不含 secret key）。第二步、当场验证与经 socket 的登录不要验证码。「测试配置」随 m1-11 的设置页。
- **跨域（CORS）**：默认只允许同源（mmwx 默认对所有来源放开）。`SATCHEL_ALLOWED_ORIGINS` 列出的来源拿到 `Access-Control-Allow-Origin` 等头、预检直接 204；永远不发 `Access-Control-Allow-Credentials`，所以跨域只给自己拿着令牌的网页用，会话 cookie 不跨域。
- **被挡在门外时**：在主控本机经 socket 用 CLI 改回来，例如 `satchel settings gates set --set master_local_only=false --set silent_mode=false --resource-version <N> --verify-user <管理员>`，或 `satchel security unban <ip> --verify-user <管理员>`；静默模式也可以重启主控后在开放期里进去；连本机 shell 都不方便时，以 `SATCHEL_FORCE_PUBLIC_ACCESS=1` 重启主控只跳过「关闭公网访问」这一道门（进来之后关掉设置、再去掉这个变量）。

### 日志与定时任务

- **日志文件**：`serve` 的日志同时写 stderr（`docker logs` 看的是它）与 `<数据目录>/logs/satchel.log`（0600），两处内容相同，是 `log/slog` 的文本格式，级别按 `log_level`。当前文件到 50 MB 时改名成 `satchel-<时间>.log` 另开一个新的，旧文件最多留 3 个，所以日志总量大约在 200 MB 以内。属性名含 `password`、`secret` 或以 `token` 结尾的值写成 `***`（兜底；令牌与密码本来就不进日志）。日志文件打不开（例如数据目录只读）时 `serve` 启动失败。
- **看日志**（只对管理员开放，令牌要 `read`）：
  ```sh
  satchel logs list                          # 从新到旧，只扫当前文件末尾 50000 行，走分页
  satchel logs list --level warn             # warn 及更高的（mmwx 只取这一级）
  satchel logs list --grep 审计写入失败       # 原文里含这段文本的行，区分大小写；这段文本在审计里打码
  satchel logs files list                    # 当前文件与轮转下来的旧文件
  satchel logs list --file satchel-2026-09-25T08-00-00.000.log   # 看某个旧文件
  ```
  每一项拆成 `time`、`level`、`msg` 与 `attrs`，拆不开的行给 `raw`。文件一直在追加，两次翻页之间有新日志时页与页会错开几行；要稳定的视图就看已经轮转下来的旧文件。不能手动删日志文件（删日志等于抹掉证据）。
- **内置定时任务**：主控启动一分钟后第一次运行，之后按间隔运行；同一个任务不会同时跑两份；以系统身份直接调业务层，不写审计。

  | 任务 | 间隔 | 做什么 |
  |---|---|---|
  | `session_cleanup` | 1 小时 | 删掉过期的会话（登录时不再顺带清理） |
  | `audit_cleanup` | 1 小时 | 删掉 180 天以前的审计记录 |
  | `security_event_cleanup` | 1 小时 | 删掉 90 天以前的安全事件 |
  | `task_run_cleanup` | 1 小时 | 删掉 7 天以前的任务运行记录 |
  | `ban_sweep` | 10 分钟 | 清掉内存里已失效的封禁与过期的令牌猜测计数 |
  | `login_limit_sweep` | 10 分钟 | 清掉内存里过期的登录限流计数 |
  | `db_checkpoint` | 5 分钟 | 只在 SQLite 下：把 WAL 写回主库（TRUNCATE，库忙时退回 PASSIVE） |
  | `backup_local` | 24 小时 | 在 `backups/` 生成一份整库备份；最新一份不到 20 小时、或已有备份或恢复在进行时跳过（见「备份与恢复」） |
  | `job_cleanup` | 1 小时 | 删掉 7 天以前、已经结束的长任务 |
  | `db_health` | 1 分钟 | SQLite 跑 `quick_check`，PostgreSQL 检查连通性；变坏时记一条 error 日志，恢复时记一条 info 日志 |

  三个保留期是常量，不能配置。需要长期留存审计的，定期用 `satchel audit list --json` 导出到别处。
- **看任务**（只对管理员开放）：`satchel schedule list` 列出任务、间隔与最近一次运行的结果；`satchel schedule runs list [--task <名字>] [--status running|ok|error]` 按 id 倒序（写入的先后）列出运行记录。每次运行开始时记一行 `running`，结束时改成 `ok` 或 `error`，带耗时与一句结果。间隔短于一小时的任务（两个内存清理、检查点、健康检查）开始时不记，成功的记录每小时最多一条；失败每次都记，失败之后的第一次成功也记。主控停止时，内置任务与 HTTP 同时停，被打断的那次记成 `error` 并写明是主控停止；结束时没写进库的记录（例如 SQLite 的写锁被请求占着）在关库前再写一次，仍没写上的下次启动时改成 `error`。三张表的清理按 id 往后扫，碰到保留期以内的行就停：时钟回拨、或有一行时间在未来时，它后面更早的行这一轮不会删。

### 备份与恢复

- **备份里有什么**：一个 ZIP，含 `manifest.json`（格式、时间、驱动、已应用的迁移、主控版本）、数据库（SQLite 是 `VACUUM INTO` 导出的一致拷贝，PostgreSQL 是 `pg_dump` 导出的当前 schema 的纯 SQL；两者都不带会话，恢复后所有人要重新登录）、`database.json`、`config.yaml`、`master.key`（主控通信密钥，恢复后节点不用重新配对）、`subscribes/` 与 `rule_templates/`。socket、`public/`、`logs/`、`backups/`、`recovery-codes/` 不进备份。
- **本机备份**：放在数据目录的 `backups/`（0700，每份 0600），最多留 7 份（手动的、上传的、恢复前与升级前自动生成的都算）。内置任务 `backup_local` 每天生成一份。
- **命令**（都只对管理员开放）：
  ```sh
  satchel backup create                    # 长任务：CLI 跟到结束；--no-wait 只拿 job，之后 satchel job get <job_id>
  satchel backup list
  satchel backup upload --file ./b.zip     # 把一份备份传进 backups/（先校验；最大 4 GiB）
  satchel backup download <名字> --output ./b.zip   # 人类专属：当场验证（备份里有主控密钥与全部数据）
  satchel backup restore <名字> --verify-user <管理员>  # 人类专属：当场验证；主控随后重启
  ```
  同一时刻只能有一次备份、上传或恢复，其余的是 `conflict`；在线迁移、自升级进行中，或数据目录里还留着升级标记 `upgrade-pending.json` 时也会被拒绝（写入暂停期间是 `unavailable`，其余时候是 `conflict`；内置的 `backup_local` 这时跳过）。上传与下载收发的是文件本身，MCP 上做不了；备份解压后超过 16 GiB 一律拒绝。校验不过（打不开、清单不认识、比本主控新、含不允许的路径）是 `bad_request`；**不支持跨驱动恢复**（SQLite 的备份恢复到 PostgreSQL 或反过来），是 `conflict`——在同驱动的主控上恢复后再用在线迁移换驱动。
- **恢复要重启**：`backup restore` 只校验、写待恢复标记 `restore-pending.json`、回应之后优雅退出，由 systemd（`Restart=always`）或 compose（`restart: unless-stopped`）拉起；**直接在前台跑 `satchel serve` 的要手动再启动**。下次启动时，在打开库之前先存一份 `before-restore-<时间>.zip`（这次恢复的后悔药），再换库：SQLite 换文件；PostgreSQL 用 `psql` 在一个事务里删掉整个 schema 再导入，出错整个回滚——**恢复会把整个 schema 换成备份里的样子**，不在备份里的对象也会被删。`master.key`、`subscribes/`、`rule_templates/` 一并换成备份里的；`database.json` 与 `config.yaml` 保持这台机器当前的。换下来的旧文件在 `backups/replaced-<时间>-<随机后缀>/`，不自动删。恢复失败时原样用旧库启动。换到一半主控被杀也没关系：停放目录先写进了待恢复标记，重启会接着做完；万一撤回也失败、库文件不在原处，主控会拒绝启动并指出原件在哪个目录，而不是在空库上跑起来。
- **恢复之后恢复码全部换新**：库回到过去，已经用掉的恢复码会重新变成可用，所以恢复后全部作废，开了两步验证的账号各生成一批新的，明文在数据目录的 `recovery-codes/recovery-codes-<时间>.txt`（0600，路径也写进日志）。拿到码登录后请删掉这个文件。两步验证的密钥也回到了备份那一刻，备份之后换过验证器的人用这里的恢复码登录，再重新绑定。结果在 `settings show` 的运行态 `last_restore` 里，恢复后的库里也有一条审计。
- **坏库自动恢复**（只限 SQLite）：启动时先跑 `quick_check`，库确定损坏就从 `backups/` 里最新一份能用的备份自动恢复，坏的库文件留在 `backups/corrupt-<时间>-<随机后缀>/`；没有可用的备份时拒绝启动——把一份同驱动的备份放进 `backups/` 再启动即可。自动恢复失败时坏库回到原处，同一次失败不会反复重试（数据目录里的 `restore-pending.json` 记着失败原因，处理好之后删掉它再启动）。PostgreSQL 连不上就不启动，不自动恢复。
- **PostgreSQL 的客户端工具**：备份要 `pg_dump`（主版本不低于服务器），恢复要 `psql`；找不到或版本太低时报 `unavailable` 并给出安装命令（例如 `apt install postgresql-client-18`，Debian 系先加 PostgreSQL 官方的 apt 源），主控不会自己装。Docker 镜像已预装 18。
- **长任务**：跑得久的命令（`backup create`、`database migrate`、`update apply`）受理后立刻返回 job，工作在主控里接着跑；`satchel job get <job_id>` 与 `satchel job list [--status ...]` 查看（只对管理员开放），MCP 的 `satchel_run` 最多等 60 秒，到时返回当时的 job。主控重启时没跑完的 job 标为 `failed`（`update apply` 的 job 例外：它由升级之后起来的进程按升级标记写成 `done` 或 `failed`）。

### 数据库设置与在线迁移

- **看与试连**（只对管理员开放）：`satchel database show` 显示当前的驱动与连接参数（密码只报是否配了）以及是否被 `SATCHEL_DATABASE_*` 环境变量覆盖；`satchel database test --host <主机> --name <库名> --user <用户> [--port 5432] [--sslmode prefer]`（密码从终端读）只读地试连一个 PostgreSQL，报告版本、当前 schema 是否为空，以及这台主控上的 `pg_dump` / `psql` 够不够迁过去之后做备份。
- **从 SQLite 在线迁移到 PostgreSQL**：`satchel database migrate --host … --name … --user … --verify-user <管理员>`，人类专属（当场验证），长任务（CLI 跟到结束，`--no-wait` 只拿 job）。
  - 前提：当前是 SQLite；没有用 `SATCHEL_DATABASE_*` 环境变量配库（否则改 `database.json` 不生效）；没有备份、上传、恢复在进行；目标库连得上，且当前 schema 里一张表都没有。
  - **迁移期间主控只能查 job**：别的命令、登录一律 `unavailable`，内置任务写不进库只记日志。用 `satchel job get <job_id>` 看进度（阶段、当前表、完成的表数、已拷的行数）。耗时与数据量成正比，建议在低峰时做。
  - 过程：在目标库上建表 → 在一个事务里按外键顺序逐表拷贝、推进自增序列、逐表核对行数 → 提交 → 改写 `database.json`（这就是提交点）→ 几秒后主控退出，由服务管理器拉起并连 PostgreSQL。会话、令牌、两步验证、设置的版本都原样过去。
  - 提交点之前任何一步失败：目标库回到空的，`database.json` 不变，主控照常用 SQLite。
  - 迁移后 `satchel.db` 留在数据目录里但**不再是最新的**；以 PostgreSQL 为准。要回到 SQLite 只能手工把 `database.json` 改回去，迁移之后写进 PostgreSQL 的数据不会在 SQLite 里。
  - 只支持 SQLite 到 PostgreSQL 一个方向。迁过去之后备份与恢复要 `pg_dump` 与 `psql`（见「备份与恢复」）。

### 升级主控与更新 CDN

- **检查更新**：`satchel update check [--channel stable|prerelease]`（只对管理员开放）报出当前版本、所选渠道的最新版本、发布说明、版本信息从哪来（更新 CDN 或 GitHub），以及这台主控能不能自升级（`can_apply` 与原因）。
- **升级**：`satchel update apply <版本号> --confirm <版本号>`，长任务（CLI 跟到结束）。它属于第 05 章危险操作的「主控自身类」：令牌要单独开这一类，每次都要带 `--confirm`。版本号必须等于 `update check` 看到的最新版本，不能降级、不能跳到中间版本。
  - 过程：按更新 CDN → GitHub → gh-proxy 的顺序下载二进制与签名（二进制与签名必须来自同一个源，一个源不对就整对换下一个）→ 用正在运行的主控编进去的公钥验签 → 试跑新二进制确认版本号 → 暂停写入（这期间只能查 job）→ 生成 `before-upgrade-<时间>.zip` → 写升级标记 `upgrade-pending.json` → 把旧二进制留成 `satchel.bak`，原子替换 → 优雅停止后原地 exec 新二进制（PID 不变，手工前台跑的也行）。
  - 新版本起来之后先经本机的 socket 请求一次自己的 `healthz`（不受「关闭公网访问」与反代登记影响），拿到 200，再确认 TCP 监听有回应（门的拒绝也算有回应），才算成功：job 写成 `done`，删掉升级标记。健康检查通过之前新版本只放行查 job、不收写入（这时还可能回退，回退会换回升级前的库），通常只有一两秒。跟着的 CLI 在主控重启的那几秒里接着等（最多 2 分钟；经反向代理访问时，代理这几秒回的 502 / 503 / 504、Cloudflare 回源失败的 520–524 也当作在重启）。
  - 同一个数据目录上只能跑一个主控：`serve` 整个运行期间锁着数据目录里的 `serve.lock`，第二个 `serve` 直接 `conflict`；原地重启时锁交给新进程，不会被插进来的实例抢走。
  - **升级是一个事务**：新版本启动失败、健康检查不过、或者连续 3 次没能启动（崩溃后被服务管理器拉起；install.sh 装的 systemd 服务是 `Restart=always`，OpenRC 服务由 `supervise-daemon` 监管；收到停止信号不算一次）时，自动把 `satchel.bak` 放回去、用升级前的备份换回旧库（与「备份与恢复」同一套流程，**恢复码会全部换新**，明文在 `recovery-codes/` 里），旧版本起来后把 job 写成 `failed` 并带上原因。旧二进制与升级前的库总是一起回来。
  - 下载、验签或试跑新二进制失败时什么都不动；暂停写入之后、exec 之前的任一步失败，撤回已经做过的（替换已经发生就放回旧二进制）、放开写入。放回旧二进制本身也失败时，写入保持暂停、升级标记留着，job 是 `failed`：重启主控，起来的是新版本就做健康检查（通过的话 job 改写成 `done`，不过就自动回退），是旧版本就只收尾。
  - **自动回退不了的情况**：新二进制在读到升级标记之前就崩溃（例如运行时初始化失败），新代码根本没跑起来，自动回退不会发生。这时用旧二进制手工做一遍与自动回退相同的事：
    1. 停掉主控，看数据目录里 `upgrade-pending.json` 的 `previous`（旧二进制，标准安装是 `/usr/local/bin/satchel.bak`）、`target`（`/usr/local/bin/satchel`）与 `backup`（升级前的备份名）。
    2. `cp <previous> <target>` 放回旧二进制。
    3. 在数据目录写 `restore-pending.json`（0600）：`{"backup": "<backup>", "source": "upgrade_rollback", "phase": "pending"}`。
    4. 把 `upgrade-pending.json` 的 `phase` 改成 `rolling_back`，`error` 写上原因（例如「新版本启动即崩溃，手工回退」）。
    5. 启动主控：旧版本先换回升级前的库（恢复码全部重发，明文在 `recovery-codes/`），再把 job 写成 `failed`、删掉两个标记。
    不要只换回旧二进制就启动：新版本可能已经迁移过库（`attempts` 为 0 也不能说明没有，停止信号不计次），旧版本会因库结构比它新而起不来，或者带着新版本写过的库继续跑。
  - **回退时换库失败**（例如备份坏了、磁盘满、找不到 `psql`、PostgreSQL 还没起来）：旧版本拒绝启动，而不是带着升级之后的库对外服务；两个标记都留着，处理好原因后把数据目录里 `restore-pending.json` 的 `phase` 改回 `pending` 再启动，主控会重试换库。升级前的备份确实坏了、修不回来时，有两条出路：把 `restore-pending.json` 的 `backup` 改成 `backups/` 里另一份能用的备份（`phase` 同样改回 `pending`），换回那一刻；或者重新装回新版本的二进制（从 Release 下载并验签），再删掉 `restore-pending.json` 与 `upgrade-pending.json`，带着升级之后的库继续跑新版本。
  - **升级成功之后又换回了旧版本**（升级标记已是 `committed` 或已删）：旧版本面对的是升级之后的库，库结构比它新时会拒绝启动。推荐换回新版本；确实要回到旧版本，就按上面「自动回退不了」的步骤 2–5 恢复升级前的备份（升级之后的写入全部丢掉）。标记已删时：备份名到 `backups/` 里按 `before-upgrade-` 前缀找，旧二进制是目标路径加 `.bak`，步骤 4 跳过。注意升级前的备份只在升级标记还在时受保护，之后按 7 份轮换（`backup_local` 每天一份，大约一周后就没了），`.bak` 也只有一份（再升级一次就被覆盖）：隔得久了就回不去，只能留在新版本。
  - 不能自升级的情况：Docker 部署（换镜像 tag：一键脚本在 `.env` 的 `SATCHEL_IMAGE` 里固定了安装时的版本，先把 tag 改成新版本号，`latest` 不用改，再 `docker compose pull && docker compose up -d`）、直接 `go build` 的开发版、非 Linux。
- **更新 CDN**：`satchel settings update-cdn set <true|false> --resource-version <N> --confirm <true|false>`（主控自身类）打开或关掉。关掉之后检查更新与下载直接走 GitHub。**CDN 的域名现在还没有**（`internal/base/selfupdate` 里的 `CDNBase` 为空），开关开着也不走 CDN，`update check` 的 `cdn.reason` 会说明。启用步骤：在代码里填上域名 → 仓库配好 R2 的四个 secret（`R2_ACCOUNT_ID`、`R2_ACCESS_KEY_ID`、`R2_SECRET_ACCESS_KEY`、`R2_BUCKET`）→ 设仓库变量 `UPDATE_CDN_ARMED=1` → 下一次发版时发布线把二进制、签名与 `version.json` 推上去。渠道的版本索引只会被更新的版本覆盖：要撤回一个有问题的版本，手工把 R2 上 `satchel/channels/<渠道>/version.json` 换成上一个版本的内容（重跑旧版本的发布线不会覆盖更新的索引）。同一渠道短时间内连发三个版本时，GitHub 可能取消中间那个的换索引，重跑它即可。

## REST 与 MCP

三个投影都从 `internal/command` 的命令表构造，一条命令登记进表就同时有 CLI 子命令、REST 路由与 MCP 可达；`docs/commands.md` 是由表生成的「命令 × scope 对照表」（`go generate ./internal/command/`，CI 守着一致）。

- **REST**：`/api/v1/…`，远程调用带 `Authorization: Bearer <令牌>`；路径由命令路径推出（`read` 用 GET、flag 作查询参数；其余用 POST、flag 与 `confirm` 在 JSON 体里；`password` 类型的 flag 与当场验证的 `verify-*` 也在 JSON 体里；`object` 类型的 flag（如 `settings set` 的 `set`）在 JSON 体里是一个对象、CLI 上写成可重复的 `--set 字段=值`；列表命令去掉末尾的 `list`，`limit` 默认 50、上限 500、`cursor` 翻页）。成功 200，body 与 CLI `--json` 是同一个对象；失败 body 是四字段错误，状态码按错误码折算（400 / 401 / 403 / 404 / 409 / 428 / 429 / 503 / 500）。未登记的键、类型不对、文件路径类参数（`-f` / `--filename` / `--file`）都是 `bad_request`；不兼容 mmwx 的 `/api/admin/*`。命令表之外只有五条路由：`GET /api/v1/healthz`、`POST /api/v1/session`（登录）、`POST /api/v1/session/two-factor`（第二步）、`DELETE /api/v1/session`（登出）、`GET /api/v1/session/captcha`（登录页取验证码配置）。
- **MCP**：`/mcp`（Streamable HTTP，无状态），只有两个工具：`satchel_run`（`args` 命令数组 + 可选 `confirm`，输出恒为 JSON）与 `satchel_explain`（`target`）。命令数组交给与 CLI 相同的解析器、不经 shell；身份只来自这次连接（本机 socket，或经 TCP 带 `Authorization: Bearer <令牌>`），`args` 里的 `--token` / `--server` / `--data-dir`、本地命令（`version`、`db`、`serve`、`admin reset-password`）、只在 CLI 里有的 `login` / `logout` / `mcp *`、人类专属命令（含 `token create` / `update` / `revoke`）、初始化向导的 `setup *`、文件路径参数一律拒绝。只支持本地进程方式的 runtime 用 `satchel mcp stdio` 垫片（见「令牌与远程接入」）。
- **审计**：每条经主控执行的命令（含被权限、confirm 或当场验证拒绝的）写一条 `audit_logs`，`satchel audit list` 看（只对管理员开放）；`password` 类型的 flag 在摘要里打码，`object` 类型的 flag 按所属 kind 的打码字段逐键打码，当场验证的值不进摘要。令牌身份的记录带 `token_id`，`actor` 是签发者。无身份与带无效凭据的请求被拒时不记，但不要身份的 `setup status` / `setup init` 执行了就记（`actor_kind` 为 `anonymous`）；`explain`、`healthz`、`/public/`、会话入口不记；被三道门拦下的请求到不了命令执行链，也不记（猜密码与猜令牌另记在安全事件里）。

现有经主控的命令：`whoami`（身份对象）、`audit list`（`--actor` / `--command` / `--since` / `--limit` / `--cursor`）、`explain [target]`、`setup *`、`account *`、`settings show` / `set` / `snapshots list` / `rollback` / `master-url set` / `gates set`、`token create` / `list` / `update` / `revoke`、`mcp status`、`security events list` / `bans list` / `ban` / `unban`。

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

打 tag 就是发版：`git tag v0.1.0 && git push origin v0.1.0`（tag 含 `-` 是 prerelease）。发布线（`.github/workflows/release.yml`）：`build` 六个平台自动跑 → `sign` 停在受保护环境 `release-signing` 等仓库拥有者批准，批准后用 `tools/sign` 给每个二进制签 Ed25519 分离签名（`<file>.sig`，64 字节）并用公钥验回、出 `checksums.txt` → `release` 建 GitHub Release（同 tag 已有 Release 即失败，不覆盖）→ `docker` 推多架构镜像（标签 `0.1.0`、`0.1`、`0`、`latest`；prerelease 是 `0.1.0-beta.1` 与 `beta`）→ `publish-cdn` 把这次的二进制与签名推到更新 CDN（R2 的 `satchel/releases/<版本>/`）→ `publish-cdn-index` 再换渠道的版本索引 `satchel/channels/<stable|prerelease>/version.json`（同一渠道一个一个来，只在这次的版本更新时覆盖；渠道还没有索引时直接写入，读现有索引失败（不是「不存在」）就失败而不是盲目覆盖）；仓库变量 `UPDATE_CDN_ARMED=1` 才跑，没设就跳过。

私钥只在环境 secret `RELEASE_SIGNING_PRIVATE_KEY` 里，环境上还要有变量 `RELEASE_SIGNING_ARMED=1`（签名 job 用它确认环境是人手建好、设了审批人的，不是 GitHub 自动建的）；公钥清单在 `pkg/release`（编进二进制）与 `install.sh` 各一份、测试钉住一致，三个二进制共用一把发布密钥；轮换先发一版带新旧两把、下一版再去掉旧的。`satchel-agent` 与 `satchel-plugins` 的发布线检出本仓库的固定 tag 跑同一份签名程序。受保护环境要「必需审批人」，GitHub 免费套餐只在公开仓库上提供，所以三个 Go 仓库是公开的。本地演练：`go run ./tools/sign keygen` 生成一对测试密钥，`RELEASE_SIGNING_PRIVATE_KEY=<私钥> go run ./tools/sign sign <file>`，验回要么把测试公钥经 ldflags 注入（`-X github.com/satchel/satchel/pkg/release.publicKeysCSV=<公钥>`）再 `verify`，要么直接用 openssl——源码里的正式公钥和你的测试私钥不成对，`verify` 会失败是正常的。

## 数据库

主控默认用 SQLite（数据目录下的 `satchel.db`），可选 PostgreSQL（数据目录下的 `database.json` 写 `driver: postgres`，或用环境变量 `SATCHEL_DATABASE_*` 覆盖）。数据目录由 `--data-dir` 或环境变量 `SATCHEL_DATA_DIR` 指定，默认 `/var/lib/satchel`。

数据目录的布局是固定的，名字都是 `internal/base/db` 里的常量：`database.json`（数据库配置）、`config.yaml`（主控配置，可不存在）、`satchel.db`（SQLite 库文件）、`master.key`（主控通信密钥）、`satchel.sock`（主控运行时的 unix socket）、`subscribes/`（订阅文件）、`rule_templates/`（规则模板）、`public/`（`/public/` 对外提供的静态文件）、`logs/`（`serve` 的日志文件）、`backups/`（本机备份）、`recovery-codes/`（恢复之后新恢复码的明文）。`db migrate` 与 `serve` 会把目录和六个子目录一起建出来（0700），postgres 模式也一样——库在别处，但主控密钥、订阅文件、规则模板、静态文件、日志与本机备份仍在这里；`db status` 是只读命令，不建目录。备份的内容表见「备份与恢复」（socket、`public/`、`logs/`、`backups/`、`recovery-codes/` 不进备份）。

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
