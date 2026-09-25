# Satchel（百宝袋）

Agent-first 的多服务器代理管理系统。主控、CLI 与 MCP 在这一个仓库、一个二进制 `satchel` 里；节点守护是 [satchel-agent](https://github.com/satchel/satchel-agent)。

**状态：M1 主控基础阶段。主控能起来（`serve`）、REST / CLI / MCP 三个投影同构上线，初始化向导、网页登录与两步验证、系统设置、API 令牌与远程 CLI / AI runtime 接入可用，业务命令随后续 change 加入。**

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
| `SATCHEL_SERVER` | CLI 要连的远程主控地址（等价于 `--server`），见「令牌与远程接入」 | 本机 socket |
| `SATCHEL_TOKEN` | CLI 带的 API 令牌（等价于 `--token`） | 不带 |

主控同时监听 TCP 与数据目录下的 unix socket `satchel.sock`（0600）。**身份只从连接判定**，按顺序取第一个：请求带了 `Authorization` 头就只看令牌——`Bearer <有效令牌>` 是令牌身份，别的一律是无效凭据、`unauthenticated`，不再往下看（见「令牌与远程接入」）；经 socket 进来、对端是 root 或运行主控的那个 OS 用户 → 本机管理员（全部权限，socket 上带的 cookie 不看）；TCP 上带有效会话 cookie → 登录的用户（管理员全部权限，普通用户只有 `read` + `operate`、没有危险类）；其余一律没有身份。没有身份能到的只有：`GET /api/v1/healthz`、`/public/<file>`（数据目录 `public/` 里的文件，目录不列、`..` 出不去）、初始化向导的 `setup status` / `setup init`，以及下面的三个会话入口。收到 SIGINT / SIGTERM 后停止接受新连接、等进行中的请求最多 10 秒、关库、删 socket、退出码 0。

### 初始化、登录与账号

- **初始化向导**：空库时先建第一个管理员。`satchel setup status` 报告是否已初始化与可走的路（本版本只有「建管理员」；恢复备份随 m1-07、导入 mmwx 随 M9）；`satchel setup init --username <名>`（可选 `--email`）在终端里读两遍密码，或在网页 / REST 上 `POST /api/v1/setup/init`，成功顺手下发会话 cookie。用户名 3 到 32 个字符、小写字母 / 数字 / `_` / `-`、以字母或数字开头；密码至少 8 个字符（bcrypt 存哈希）。库里已有用户后 `setup init` 是 `conflict`；两个并发的 init 只有一个成功。**初始化之前谁都能建这个管理员**（向导本来就不要身份），所以先在本机或内网完成 `setup init`，再把主控暴露到公网；来源 IP、Turnstile 与封禁随 m1-05 交付。
- **登录与会话**：`POST /api/v1/session`（`username` / `password` / 可选 `remember_me`）成功后下发 cookie `satchel_session`（HttpOnly、SameSite=Strict、Path=/，经 TLS 到达时带 Secure）：默认 24 小时，记住我 30 天。令牌是随机串，库里只存它的 SHA-256；`DELETE /api/v1/session` 登出。浏览器发来的写请求（身份来自会话 cookie 的，以及没有身份的登录入口与向导；`/api/v1/…` 与 `/mcp` 都算）要过同源检查：`Origin` 的 host 等于主控地址；没 `Origin` 时 `Sec-Fetch-Site` 不能是跨站；两个头都没有的非浏览器客户端放行。经 socket 与有效令牌来的请求不受影响。账号停用是 `forbidden`；用户名或密码不对都是同一条 `unauthenticated`。
- **两步验证与恢复码**：`satchel account totp setup` 给出密钥与 otpauth URL（扫进验证器），`account totp confirm --code <6 位>` 启用并一次性给出 8 枚恢复码（每枚 8 个十六进制字符，库里只存哈希）。开了两步验证后登录分两步：密码正确得到 5 分钟有效、只能用一次的 `pending` 票据，`POST /api/v1/session/two-factor`（`pending` + `code`）用验证器的码或一枚恢复码完成。同一个 TOTP 码 90 秒内只认一次；每枚恢复码只能成功一次（校验与作废在同一个数据库事务里，并发也只成功一次），用恢复码登录不会关掉两步验证；剩余不足两枚时登录结果与 `account show` 都有 `recovery_codes_low` 提示，`account recovery-codes regenerate` 重新生成 8 枚并作废旧的。`account totp disable` 关掉；已启用时再 `setup` 是 `conflict`，要换密钥先 disable。登录第二步验错一次，那张 5 分钟的 `pending` 票据就作废，要重新用密码登录。
- **当场验证**：`account set-password` / `account totp setup` / `account totp confirm` / `account totp disable` / `account recovery-codes regenerate` 是人类专属命令：每次执行都要在同一个请求里带上自己的密码（`verify-password`）与——账号开了两步验证时——第二因素（`verify-code`），验一次用一次，不签发任何提升票据。CLI 上密码只从终端读（`--verify-password` 不是命令行参数，给了就是用法错误），`--verify-code` 可以作参数也可以终端输入（恢复码建议终端输入，写在命令行上会留在 shell 历史与进程列表里）；stdin 不是终端时直接以 `human_required` 拒绝、不等待。本机管理员不是账号，要用 `--verify-user <管理员用户名>` 指明验谁；登录的用户只能验自己。REST 上这三个值放在 JSON 体里，它们永不进审计摘要。`account set-password --new-password`（终端读两遍）改完作废该账号其它全部会话、保留当前这一个。
- **忘了管理员密码**：在主控本机执行 `satchel admin reset-password <用户名> --confirm <用户名>`（本地命令，直接开数据目录里的库，主控在不在跑都行；只对管理员账号；新密码在终端里读两遍）。它作废该账号全部会话、不动两步验证，且因为不经主控而**不进审计**（stderr 会提示这一点）。

### 系统设置

系统设置是**一个单例对象**（kind `SystemSettings`，第 07 章「主控设置类」）：`system_config` 的列与 `system_settings` 键值表的 92 个 key 合在一起，整单一个 `resourceVersion`。`serve` 启动时（迁移之后、监听之前）确保那一行存在，空库起来就是版本 1。`settings *` 只对管理员开放（本机管理员与管理员账号），普通用户 `forbidden`。

- **读**：`satchel settings show`（`--json` 是资源信封）。`spec` 是日常运维档的 100 个字段（既有列如 `heartbeat_interval`，也有 key 如 `branding_site_title`），`status` 是其余四档（七组人类专属如 `master_url`、主控自身类如 `update_cdn_enabled`、只读的 `require_encryption` 恒为 true、运行态如 `master_https_recovery_pending`）。键值表里没有的 key 按默认值表补（照 mmwx 读侧的 fallback，`default_theme` 默认 `flat`）；打码字段（`telegram_bot_token`、`turnstile_secret_key`、`tgbot_token`、`probe_external_token_sha256`）只对带 `secrets` scope 的身份给原文（本机管理员、管理员账号的会话、打开了密钥读取的令牌），其余身份非空时输出 `***`；审计摘要里一律打码。字段清单与分档看 `satchel explain SystemSettings`。
- **写日常运维档**：`satchel settings set --set <字段>=<值> [--set …] --resource-version <N>`，REST 是 `POST /api/v1/settings/set`，体 `{"set":{"heartbeat_interval":45,"branding_site_title":"Satchel"},"resource-version":3}`。一次可以改任意多个字段，列与 key 混着给；服务端先整体校验——字段必须是 `spec` 里的（别的档一律 `field_not_applyable` 并点名分档，不认识的 `unknown_field`），值是字符串时按键值表的编码规则解析（布尔 `true` / `false` / `1` / `0` / 空，整数规范十进制，json 必须是合法 JSON 文本），JSON 原生类型直接收，再过照 mmwx 抄来的字段规则（如 `subscription_output_format` 只收 `yaml` / `json`、`default_theme` 四个主题名、`heartbeat_interval` 至少 5、`dashboard_refresh_interval_ms` 在 1000 到 60000 之间；mmwx 静默改写的地方这里一律拒绝并说明范围；Satchel 另加了两条 mmwx 没有的形状规则：`probe_external_token_sha256` 要是 64 位小写十六进制，`login_wallpaper` 与探针 logo 一样只收 `/`、`http(s)://`、`data:image/` 开头的引用）——任一字段不过整单拒绝、不部分写入。然后在**一个事务**里：比对 `resourceVersion`（不匹配 `version_conflict`，退出码 6；没有跳过比对的写法，冲突了重新读一遍再改，`--force` 不是参数）→ 存一份写前快照 → 写两张表 → 版本加 1。打码字段交回 `***` 表示保持不变，空串表示清掉。成功返回写后的整个对象，新版本在 `metadata.resourceVersion` 里。
- **快照与回滚**：每次成功的 `settings set` / `settings rollback` 在 `config_snapshots` 里追加一行写前的日常运维档（七组字段不进快照；主控自身类字段随 m1-08 的第一条写命令再进）。`satchel settings snapshots list` 按时间倒序列出（id、object_version、created_at、source、content_hash；不给内容，里面有原文密钥）；`satchel settings rollback <id> --resource-version <N>` 把那份快照的内容当成一次 `settings set` 写回：重过字段分档与规则、同样比对版本、同样先存写前快照——回滚本身也能被回滚。它不走 apply、不把版本号倒回去。
- **主控地址（七组）**：`satchel settings master-url set --url <主控地址> --subscription-url <订阅域名> --resource-version <N>` 是人类专属命令：要当场验证（见上一节），MCP 与令牌一律 `human_required`。两个至少给一个；值必须是干净的 HTTP(S) origin（只有 scheme 与 host，可带端口；末尾斜杠去掉），空串表示清掉。它同样比对并抬版本，但不存快照。改完不推送到节点、不做主控迁移（随 M2 的节点通道）。
- **哪些 key 什么时候生效**：本 change 交付的是存储与写路径；三道门与静默模式的写命令随 m1-05，更新 CDN 开关随 m1-08，通知参数随 M4，TG 机器人随 M5，HTTPS 自愈与 `external_https` 随 M6，采集间隔与 agent 日志开关下发到节点随 M2。

### 令牌与远程接入

远程 CLI、AI runtime 与脚本进主控用的凭据只有一种：API 令牌（第 05 章功能②）。

- **令牌与权限范围**：令牌是 `sat_` 加 43 个字符的随机串，库里只存它的 SHA-256，明文只在签发的那一次输出里出现。权限范围由 `read`（恒有）、`operate`、六个危险类（`delete`、`restart`、`permission`、`batch`、`exec`、`master`）与单独的 `secrets`（密钥读取）组成；预设由它推出：没有 `operate` 是 `readonly`，有 `operate` 且六类全开是 `full`，其余是 `ops`。命令上 `--preset readonly|ops|full` 把 `operate` 与危险类设成该预设的样子，`--danger <类>`（可重复）把危险类设成恰好这几个并隐含 `operate`，`--secrets` 开关密钥读取；`--preset readonly` 带 `--danger` 是 `bad_request`。新建时默认只读、不过期（`--expires-in 720h` 设过期时间）。
- **签发者与上限**：令牌挂在签发者的账号名下，权限上限是签发者的角色：管理员什么都能签；普通用户只能签 `read` 与 `operate`，要带危险类或 `secrets` 直接 `forbidden` 并点名超出的项，不会悄悄截掉。令牌每次被使用时，生效的权限是它的权限范围与签发者**当下**角色的交集：签发者被降为普通用户后危险类与密钥读取随之失效，签发者停用或删除后令牌无效。
- **签发、改、吊销**：`satchel token create --name <名字> [--preset …] [--danger …] [--secrets] [--expires-in …] [--runtime <标签>]`、`token update <id>`（改名字、权限范围、过期时间，至少给一项）、`token revoke <id>` 是人类专属命令（当场验证见上文），令牌不能签令牌（REST 与 MCP 上都是 `human_required`）。改权限与吊销立刻生效，令牌字符串不变；已吊销的不能再改。`token list` 列出令牌、`state`（`active` / `revoked` / `expired`）与最后使用时间（按分钟记）；普通用户只看得到、改得了自己的令牌，别人的一律 `not_found`，管理员可用 `--owner` 过滤。
- **第一把令牌在哪签**：在主控本机经 socket 执行，例如 `satchel token create --name laptop --preset ops --verify-user admin`（本机管理员不是账号，要用 `--verify-user` 指明一个管理员，令牌挂在它名下），或在网页上签（随 m1-10）。远程只带令牌的 CLI 签不了新令牌。
- **远程 CLI**：连哪个主控、带哪把令牌，各自按「根 flag（`--server`、`--token`）→ 环境变量（`SATCHEL_SERVER`、`SATCHEL_TOKEN`）→ 登录文件」的顺序取第一个有的；都没有 server 就连本机 socket。`--server` 可以带路径前缀（主控挂在反代的子路径下）。`--token` 会留在进程列表与 shell 历史里，只适合临时试一下；脚本用环境变量；常用的机器用 `satchel login --server <地址>`：令牌从终端读，先用它调一次 `whoami` 确认是令牌身份再存进登录文件——用户配置目录下的 `satchel/login.json`（Linux 上 `~/.config/satchel/`，macOS 上 `~/Library/Application Support/satchel/`），文件 0600，权限对组或其他用户开放时拒绝使用。登录文件里的令牌只发给它自己记的那个 server：用 `--server` 或环境变量指向别的主控时不会带上它。`satchel logout` 只删登录文件，令牌在服务端仍然有效，吊销用 `token revoke`。本地命令（`version`、`db *`、`serve`、`admin reset-password`、`logout` 等）显式带 `--server` / `--token` 是用法错误，免得以为在改远端、实际开了本机的库；环境变量不算。人类专属命令（`token create` / `update` / `revoke`、`account set-password` 等）只能在主控本机经 socket、不带令牌执行：CLI 连的是远程主控或带着令牌时直接 `human_required`，不问密码，也就不会把密码发出去（远程 CLI 没有人的身份）。
- **本机配了令牌**：带了令牌就按令牌算，经 socket 也一样：主控本机的进程设了 `SATCHEL_TOKEN`，就只有这把令牌的权限，不再是本机管理员。带了无效的令牌（不存在、已吊销、已过期、签发者停用，或 `Authorization` 头不是 Bearer）一律 `unauthenticated`，连初始化向导也拒，reason 不区分是哪一种，不会退回本机管理员或会话身份，也不记审计；`/api/v1/healthz` 与 `/public/` 不受影响。
- **明文 HTTP 的风险**：用 `http://` 把令牌、或终端里输入的密码（例如远程执行 `setup init`）发给回环地址以外的主控时，CLI 会在 stderr 提示一行（密码在输入之前提示；输出是 JSON 时不提示，stderr 只留给错误）：同一网络上的人能截获它们。经公网访问请给主控配 HTTPS。
- **接 AI runtime**：`satchel mcp init --runtime claude-code|codex|hermes` 在主控本机经 socket 签一把令牌（要加 `--verify-user`；预设默认 `ops`，名字与 runtime 标签默认 `<runtime>@<主机名>`，`--preset` / `--name` 可改；远程 CLI 签不了，用 `--use-token` 改用一把已有的令牌），写进 runtime 的配置。写进配置的主控地址取 `--url`，不给就用 CLI 连的 server，都没有就按 serve 的监听地址推出本机地址（`0.0.0.0:12889` 推成 `http://127.0.0.1:12889`）；它是非本机的 `http://` 时会提示一行（输出是 JSON 时不提示）。签发之前先请求这个地址的 `/api/v1/healthz`，连不上或不是 Satchel 主控就停下；签出来的令牌先对它调一次 `whoami`，那里认不出这把令牌（不是签发它的主控）就不写配置、提示吊销它。
  - Claude Code：先 `claude mcp remove --scope user satchel` 再 `claude mcp add --scope user satchel -- <satchel 的绝对路径> mcp stdio`（已经登记了同样的就都不跑；add 失败时把原来的登记交还给你），三个变量 `SATCHEL_SERVER`、`SATCHEL_TOKEN`、`SATCHEL_OUTPUT=json` 写进 `~/.claude/settings.json` 的 `env`（顶层其它键与顺序不变）。`satchel mcp stdio` 是给只支持本地进程方式的 runtime 用的垫片：连上主控的 `/mcp`，先确认令牌有效，再把两个工具原样转给 runtime；stdout 只走协议。
  - Codex：`$CODEX_HOME/config.toml`（默认 `~/.codex/config.toml`）的 `[mcp_servers.satchel]` 里的 `url` 与 `http_headers.Authorization`（Bearer 加令牌；块里你写的其它键，如 `enabled`、`disabled_tools`、超时，原样保留），以及 `[shell_environment_policy]` 的 `set` 里的三个变量。块是 stdio 写法或配了 `bearer_token_env_var` 时先停下。注意：Codex 把受信任（trusted）项目里的 `.codex/config.toml` 与用户级配置按键合并，项目层只写一个 `[mcp_servers.satchel]` 的 `url`，你的令牌就会随用户级的 `http_headers` 发往那个地址（已对 Codex 0.156.1 的源码与正式二进制核实）；只把你信任的仓库标为 trusted。你配了 include 过滤（`include_only` 或 `filters`）且不放行 `SATCHEL_*` 时先停下，打印要手工加的 include。
  - Hermes：`$HERMES_HOME/.env`（默认 `~/.hermes/.env`）里的三个变量，`config.yaml` 的 `mcp_servers.satchel` 里的 `url` 与 `headers.Authorization`（`Bearer ${SATCHEL_TOKEN}`，令牌明文只在 `.env` 里；条目里你写的其它键原样保留），以及 `terminal.env_passthrough`。

  只动 Satchel 自己的键（你在 satchel 条目里写的 `enabled: false` 之类不改，输出会提示接入后仍是停用的）；改已有文件前先备份成 `<文件>.satchel-bak-<时间戳>`（0600），先写临时文件再改名、保留原权限，但写进令牌的 `settings.json`、`config.toml`、`.env` 会去掉组与其他用户的权限（输出里注明）；新文件 0600、新目录 0700。原来配置里的另一把令牌不会被吊销，输出会提示你用 `mcp status` 找到后 `token revoke`；任何一个文件解析不了或写法不在支持范围内，在签发令牌之前就停下（`config`）并打印要手工加的片段。`--print` 只打印片段与命令、不写文件（这时输出里有令牌明文）。令牌签出来之后写文件失败是 `partial_failure`（退出码 8），输出里带令牌明文与片段。改完重启 runtime，验证命令分别是 `claude mcp get satchel`、`codex mcp get satchel --json`、`hermes mcp test satchel`；skills 随 m1-09 交付。
- **谁在连**：`satchel mcp status` 列出绑了 runtime 标签的令牌，按最后使用时间倒序，从没用过的排最后；可见范围与 `token list` 相同。

## REST 与 MCP

三个投影都从 `internal/command` 的命令表构造，一条命令登记进表就同时有 CLI 子命令、REST 路由与 MCP 可达；`docs/commands.md` 是由表生成的「命令 × scope 对照表」（`go generate ./internal/command/`，CI 守着一致）。

- **REST**：`/api/v1/…`，远程调用带 `Authorization: Bearer <令牌>`；路径由命令路径推出（`read` 用 GET、flag 作查询参数；其余用 POST、flag 与 `confirm` 在 JSON 体里；`password` 类型的 flag 与当场验证的 `verify-*` 也在 JSON 体里；`object` 类型的 flag（如 `settings set` 的 `set`）在 JSON 体里是一个对象、CLI 上写成可重复的 `--set 字段=值`；列表命令去掉末尾的 `list`，`limit` 默认 50、上限 500、`cursor` 翻页）。成功 200，body 与 CLI `--json` 是同一个对象；失败 body 是四字段错误，状态码按错误码折算（400 / 401 / 403 / 404 / 409 / 428 / 503 / 500）。未登记的键、类型不对、文件路径类参数（`-f` / `--filename` / `--file`）都是 `bad_request`；不兼容 mmwx 的 `/api/admin/*`。命令表之外只有四条路由：`GET /api/v1/healthz`、`POST /api/v1/session`（登录）、`POST /api/v1/session/two-factor`（第二步）、`DELETE /api/v1/session`（登出）。
- **MCP**：`/mcp`（Streamable HTTP，无状态），只有两个工具：`satchel_run`（`args` 命令数组 + 可选 `confirm`，输出恒为 JSON）与 `satchel_explain`（`target`）。命令数组交给与 CLI 相同的解析器、不经 shell；身份只来自这次连接（本机 socket，或经 TCP 带 `Authorization: Bearer <令牌>`），`args` 里的 `--token` / `--server` / `--data-dir`、本地命令（`version`、`db`、`serve`、`admin reset-password`）、只在 CLI 里有的 `login` / `logout` / `mcp *`、人类专属命令（含 `token create` / `update` / `revoke`）、初始化向导的 `setup *`、文件路径参数一律拒绝。只支持本地进程方式的 runtime 用 `satchel mcp stdio` 垫片（见「令牌与远程接入」）。
- **审计**：每条经主控执行的命令（含被权限、confirm 或当场验证拒绝的）写一条 `audit_logs`，`satchel audit list` 看（只对管理员开放）；`password` 类型的 flag 在摘要里打码，`object` 类型的 flag 按所属 kind 的打码字段逐键打码，当场验证的值不进摘要。令牌身份的记录带 `token_id`，`actor` 是签发者。无身份与带无效凭据的请求被拒时不记，但不要身份的 `setup status` / `setup init` 执行了就记（`actor_kind` 为 `anonymous`）；`explain`、`healthz`、`/public/`、登录 / 登出入口不记。

现有经主控的命令：`whoami`（身份对象）、`audit list`（`--actor` / `--command` / `--since` / `--limit` / `--cursor`）、`explain [target]`、`setup *`、`account *`、`settings show` / `set` / `snapshots list` / `rollback` / `master-url set`、`token create` / `list` / `update` / `revoke`、`mcp status`。

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
