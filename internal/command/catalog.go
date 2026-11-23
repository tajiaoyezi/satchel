package command

import v1 "github.com/satchel/satchel/pkg/api/v1"

// GroupSummaries 是分组节点在帮助里的一句话说明；分组只做分组，本身不是命令。
var GroupSummaries = map[string]string{
	"db":                       "数据库：执行迁移、查看迁移状态、清除迁移锁",
	"audit":                    "审计记录：谁在何时经主控做了什么",
	"setup":                    "初始化向导：第一次打开主控时建第一个管理员",
	"account":                  "自己的账号：状态、密码、两步验证",
	"account totp":             "两步验证（TOTP）：启用、确认、禁用",
	"account recovery-codes":   "两步验证的恢复码",
	"admin":                    "主控本机的应急操作",
	"settings":                 "系统设置：一个单例对象，整单一个 resourceVersion（主控设置类）",
	"settings snapshots":       "系统设置的写前快照",
	"settings master-url":      "主控地址与订阅域名（七组人类专属：改的时候当场验证）",
	"settings gates":           "门：关闭公网访问、静默模式、登录限流与封禁的参数、Turnstile、反代登记（七组人类专属：改的时候当场验证）",
	"security":                 "安全：安全事件与 IP 封禁（封禁与解封要当场验证）",
	"token":                    "API 令牌：签发、列出、改权限、吊销（签发、改权限、吊销要当场验证）",
	"mcp":                      "MCP 接入：stdio 垫片、把 AI runtime 接上主控、看谁在连",
	"logs":                     "主控的系统日志（只对管理员开放）",
	"logs files":               "数据目录 logs/ 下的日志文件",
	"schedule":                 "内置定时任务与它们的运行记录（只对管理员开放）",
	"schedule runs":            "内置定时任务每次运行的记录",
	"job":                      "长任务：受理后在主控里接着跑的命令，按 job id 查结果（只对管理员开放）",
	"backup":                   "整库备份与恢复（只对管理员开放；下载与恢复要当场验证）",
	"database":                 "主控用的数据库：查看、试连 PostgreSQL、从 SQLite 在线迁移（只对管理员开放；迁移要当场验证）",
	"update":                   "主控自升级：检查更新、升到所选渠道的最新版（只对管理员开放；升级属主控自身类危险操作）",
	"settings update-cdn":      "更新 CDN 的开关（主控自身类危险操作）",
	"settings batch-threshold": "批量类危险门控阈值（需 permission 危险类与 --confirm）",
	"settings image":           "logo、登录壁纸这类图片字段用的图片：上传之后用 settings set 把地址写进字段",
	"plans":                    "配置变更预演计划：列出与查看历史计划（只对管理员开放）",
	"snapshot":                 "配置类对象快照：查看历史快照与按单条或批次生成回滚计划（只对管理员开放）",
	"server":                   "服务器（节点）：加服务器、取安装命令、管理凭据、看在线状态，经通道临时限速、踢线、看连接与流量，以及远程升级/卸载/切端口/探地址/只读配置检查（只对管理员开放）",
	"server token":             "服务器通信凭据：查看原文（需 secrets）、平滑轮换与立即吊销（需 permission）",
	"server limit":             "主控临时限速：设与撤（只动「主控临时」那一份来源）",
	"server update":            "节点 satchel-agent 版本检查、单台自更新与批量串行自更新",
	"server config":            "节点只读配置检查与无侵入校验（配置文件只读，改动须经 plan / apply）",
	"server service":           "节点运行服务（kernel / nginx / agent）状态查询与启停控制（同机 nginx 控制需当场人类验证）",
	"server nginx":             "节点 nginx 远程安装/卸载、server 块巡检与受保护的 stream 清理（同机操作需当场人类验证并保护 443）",
	"server steal":             "服务器偷取模式（tunnel / fallback / off）：查 443 绑定状态与偷自己本地 nginx 预览、声明式部署与切换",
	"server warp":              "节点纯 Go WARP WireGuard 凭据管理（状态、注册安装、授权/离线导入、移除）与出站/防送中快捷路由规划",
	"evidence":                 "一键取证证据包：分页查询摘要与查看单份完整证据包（系统只写 kind EvidencePackage，只对管理员开放）",
	"return-route":             "返程路由（kind ReturnRoute）：列表查询与节点返程测路长任务（增删改恢复走 plan / apply）",
	"tunnel":                   "任意门单跳转发、复用分流与 N 台链式隧道编排（零迁移，全部写操作一律经 plan / apply）",
	"tunnel multiplex":         "复用已有 direct 入站按域名/IP 分流转发（不另开端口，自动排在兜底规则之前）",
	"tunnel chain":             "N 台服务器首尾相接链式隧道查询、单 Plan 原子编排与整链清理",
	"website":                  "网站（kind Website）：查询、扫盘（未纳管零覆盖）、无状态向导与模板目录（所有写操作一律经 plan / apply）",
	"inbound":                  "代理入站：查询、协议矩阵、配置示例、密钥生成、无状态向导与多机批量规划（只对管理员开放）",
	"inbound batch":            "多机批量入站规划与批次记录查询（所有写操作一律经 plan / apply）",
	"outbound":                 "代理出站：查询、节点实时探活、中转/落地规划与多机批量规划（只对管理员开放）",
	"outbound landing":         "中转/落地出站与绑定路由规则一体化规划（经 plan / apply 原子提交）",
	"outbound batch":           "多机批量出站规划与批次记录查询（所有写操作一律经 plan / apply）",
	"routing":                  "代理路由：查询与遮蔽分析、内置 sing-box@v1.14.4 快捷预设与自定义预设管理（只对管理员开放）",
	"routing preset":           "内置与用户自定义路由规则快捷预设（零废弃 geoip/geosite）",
}

// Catalog 是本仓库登记的全部命令。按功能域分文件时把各自的切片拼进来；顺序无关，Table 会排序。
func Catalog() *Table {
	return MustNew(catalogCommands()...)
}

func catalogCommands() []*Command {
	var all []*Command
	all = append(all, localCommands()...)
	all = append(all, baseCommands()...)
	all = append(all, identityCommands()...)
	all = append(all, settingsCommands()...)
	all = append(all, tokenCommands()...)
	all = append(all, securityCommands()...)
	all = append(all, opsCommands()...)
	all = append(all, backupCommands()...)
	all = append(all, databaseCommands()...)
	all = append(all, updateCommands()...)
	all = append(all, configTxCommands()...)
	all = append(all, proxyCommands()...)
	all = append(all, websiteCommands()...)
	all = append(all, tunnelCommands()...)
	all = append(all, networkCommands()...)
	all = append(all, diagCommands()...)
	all = append(all, serverCommands()...)
	return all
}

// diagCommands 是 m2-21 的三层无 SSH 远程排障命令：固定只读诊断集、受控任意命令执行与一键取证/证据包查询。
func diagCommands() []*Command {
	name := Arg{Name: "name", Description: "服务器的名字或 ID"}
	return []*Command{
		{Path: []string{"server", "diag"}, Summary: "在节点运行固定只读诊断集（10 项：ports/services/logs/config/connectivity/firewall/clock/resources/certs/speedtest；无 secrets 时 config 自动打码）", Class: ClassRead,
			Args: []Arg{
				name,
				{Name: "item", Optional: true, Description: "可选：仅运行单个诊断项（ports、services、logs、config、connectivity、firewall、clock、resources、certs 或 speedtest）"},
			}},
		{Path: []string{"server", "exec"}, Summary: "在本地已开启 allow_exec 的远程节点执行受控命令（第五类 exec 危险操作，同机永久禁止；支持 --command 或 -- <命令>，不做交互式 shell）",
			Class: ClassAction, Shape: ShapeJob, Danger: v1.DangerExec, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"}, TrailingCommandFlag: "command",
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "command", Type: TypeString, Description: "在节点 /bin/sh -c 中执行的命令文本（CLI 与 MCP 也支持在 -- 之后直接传入）"},
				{Name: "timeout", Type: TypeString, Default: "30s", Description: "命令执行超时（1s..300s，默认 30s）"},
			}},
		{Path: []string{"server", "evidence"}, Summary: "一键采集指定服务器排障证据包并落入 evidence_packages（离线或部分采集失败时记 collect_error 仍落库）", Class: ClassAction,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "category", Type: TypeString, Default: "general", Description: "取证类别：general（默认）、offline、network、kernel、resources 或 cert"},
			}},
		{Path: []string{"evidence", "list"}, Summary: "按 id 倒序分页列出排障证据包摘要（不含 items 正文，只对管理员开放）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
				{Name: "category", Type: TypeString, Description: "按取证类别过滤：general、offline、network、kernel、resources 或 cert"},
			},
			Columns: []string{"id", "category", "server_id", "collected_at", "size_bytes", "collect_error", "expires_at"}},
		{Path: []string{"evidence", "get"}, Summary: "查看单份完整排障证据包（含结构化排障数据 items，只对管理员开放）", Class: ClassRead,
			Args: []Arg{{Name: "id", Description: "证据包 ID（evidence list 里的 id）"}}},
	}
}

// networkCommands 是 m2-19 的节点网络探测、纯 Go WARP 闭环与返程路由查询/测路命令（node-network-warp-and-return-routes）。
func networkCommands() []*Command {
	name := Arg{Name: "name", Description: "服务器的名字或 ID"}
	return []*Command{
		{Path: []string{"server", "scan"}, Summary: "从节点执行受限 TCP 端口扫描（单次最多 256 个端口、有界并发与超时）", Class: ClassAction,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "host", Type: TypeString, Default: "127.0.0.1", Description: "待扫描主机或 IP（默认 127.0.0.1）"},
				{Name: "ports", Type: TypeStrings, Description: "端口或区间列表（如 443、8000-8010；总数上限 256，留空扫常用端口）"},
				{Name: "timeout", Type: TypeDuration, Default: "1s", Description: "单端口连接超时（100ms..5s，默认 1s）"},
				{Name: "concurrency", Type: TypeInt, Default: "32", Description: "并发数（1..64，默认 32）"},
			}},
		{Path: []string{"website", "validate"}, Summary: "从节点对远程站点执行 TCP/TLS/SNI/HTTP2/证书与 Reality 适用性校验", Class: ClassAction,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "domain", Type: TypeString, Description: "待校验目标域名或主机（必填）"},
				{Name: "port", Type: TypeInt, Default: "443", Description: "目标端口（1..65535，默认 443）"},
				{Name: "sni", Type: TypeString, Description: "可选：TLS SNI 域名（默认同 --domain）"},
				{Name: "timeout", Type: TypeDuration, Default: "10s", Description: "单次探测超时（1s..30s，默认 10s）"},
			}},
		{Path: []string{"server", "domain-latency"}, Summary: "从节点批量探测域名 TCP/TLS 握手延迟（最多 200 域名、最多 16 并发）", Class: ClassAction,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "domains", Type: TypeStrings, Description: "待探测域名列表（必填，去重后 1..200 个）"},
				{Name: "port", Type: TypeInt, Default: "443", Description: "探测端口（1..65535，默认 443）"},
				{Name: "mode", Type: TypeString, Default: "tls", Description: "探测模式：tls（默认）或 tcp"},
				{Name: "timeout", Type: TypeDuration, Default: "5s", Description: "单域名超时（500ms..15s，默认 5s）"},
				{Name: "concurrency", Type: TypeInt, Default: "16", Description: "并发数（1..16，默认 16）"},
			}},
		{Path: []string{"server", "warp", "status"}, Summary: "查询节点纯 Go WARP WireGuard 状态与库内 WARP 出站/防送中路由摘要（绝不暴露私钥）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "warp", "install"}, Summary: "在节点以纯 Go 注册 Cloudflare WARP WireGuard 设备并以 0600 落盘（裸机与容器通用）", Class: ClassAction, Shape: ShapeJob,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "license-key", Type: TypeString, Secret: true, Description: "可选：顺带绑定的 WARP+ license 密钥（审计打码）"},
				{Name: "mtu", Type: TypeInt, Default: "1280", Description: "WireGuard MTU（1280..1500，默认 1280）"},
				{Name: "endpoint", Type: TypeString, Description: "可选：自定义 WARP endpoint（host:port，默认 engage.cloudflareclient.com:2408）"},
				{Name: "api-base", Type: TypeString, Description: "可选：覆盖 Cloudflare WARP API 基址（调试/测试用）"},
				{Name: "reinstall", Type: TypeBool, Description: "已安装时强制重新注册新设备"},
			}},
		{Path: []string{"server", "warp", "authorize"}, Summary: "为节点绑定 WARP+ license 或离线导入已有 WireGuard 凭据（两种模式二选一，敏感参数打码）", Class: ClassAction,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "license-key", Type: TypeString, Secret: true, Description: "WARP+ license 密钥（与离线导入参数互斥，审计打码）"},
				{Name: "api-base", Type: TypeString, Description: "可选：覆盖 Cloudflare WARP API 基址"},
				{Name: "private-key", Type: TypeString, Secret: true, Description: "离线导入模式：标准 Base64 32 字节 WireGuard 私钥（审计打码）"},
				{Name: "address-v4", Type: TypeString, Description: "离线导入模式：WireGuard IPv4 地址或 CIDR（如 172.16.0.2/32）"},
				{Name: "address-v6", Type: TypeString, Description: "离线导入模式（可选）：WireGuard IPv6 地址或 CIDR"},
				{Name: "peer-public-key", Type: TypeString, Description: "离线导入模式（可选）：对端公钥（默认 Cloudflare 官方公钥）"},
				{Name: "endpoint", Type: TypeString, Description: "离线导入模式（可选）：对端 host:port"},
				{Name: "reserved", Type: TypeString, Description: "离线导入模式（可选）：3 个逗号分隔的 0..255 保留字节"},
				{Name: "mtu", Type: TypeInt, Default: "1280", Description: "离线导入模式（可选）：MTU（1280..1500，默认 1280）"},
			}},
		{Path: []string{"server", "warp", "remove"}, Summary: "原子删除节点 WARP 凭据文件、更新 warp_installed 状态并提示库内残留 WARP 出站", Class: ClassAction, Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args: []Arg{name}},
		{Path: []string{"server", "warp", "outbound-plan"}, Summary: "从节点拉取 WARP WireGuard 凭据规划 sing-box endpoints[] 出站（可选 --anti-china 附带防送中路由）并生成待确认 Plan", Class: ClassConfig,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "tag", Type: TypeString, Default: "warp", Description: "WARP 出站 tag（默认 warp）"},
				{Name: "anti-china", Type: TypeBool, Description: "同时附带一条复用内置 warp-google-meta 预设的防止送中路由规则"},
				{Name: "inbound-tag", Type: TypeString, Description: "可选（配合 --anti-china）：限定仅匹配指定入站 tag"},
				{Name: "sort-order", Type: TypeInt, Default: "40", Description: "防送中路由规则排序值（默认 40）"},
			}},
		{Path: []string{"return-route", "list"}, Summary: "分页列出 ReturnRoute 返程路由配置与最近一次实测状态（按服务器与运营商升序）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
				{Name: "carrier", Type: TypeString, Description: "按运营商代码过滤（如 CT、CU、CM、EDU）"},
				{Name: "enabled", Type: TypeBool, Description: "按启用状态过滤"},
				{Name: "include-deleted", Type: TypeBool, Description: "包含已软删除的返程路由"},
			},
			Columns: []string{"id", "server_id", "carrier", "enabled", "route_type", "entry_ip", "entry_asn", "tested_at"}},
		{Path: []string{"return-route", "get"}, Summary: "查看单条 ReturnRoute 返程路由配置与最近实测状态", Class: ClassRead,
			Args: []Arg{{Name: "id", Description: "返程路由 ID（return-route list 里的 id）"}}},
		{Path: []string{"return-route", "test"}, Summary: "启动关联服务器长任务实测返程路由（默认测整机已启用线路，支持 --carrier 单测）并回写状态不抬版本", Class: ClassAction, Shape: ShapeJob,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "carrier", Type: TypeString, Description: "可选：仅测试指定运营商线路（省略时测试该服务器全部已启用线路）"},
			}},
	}
}

// tunnelCommands 是 m2-17 的服务器偷取模式、任意门单跳/复用隧道与 N 台链式隧道命令（node-steal-and-tunnels）。
func tunnelCommands() []*Command {
	name := Arg{Name: "name", Description: "服务器的名字或 ID"}
	targetFlags := []Flag{
		{Name: "target-address", Type: TypeString, Description: "固定转发目标主机或 IP（与 --target-port 成对，和 --target-server/--target-inbound 二选一）"},
		{Name: "target-port", Type: TypeInt, Description: "固定转发目标端口（1..65535）"},
		{Name: "target-server", Type: TypeString, Description: "目标服务器名或 ID（与 --target-inbound 成对，自动解析地址与端口）"},
		{Name: "target-inbound", Type: TypeString, Description: "目标服务器上的活跃入站 tag（禁止同机同端口自环）"},
	}
	tunnelPlanFlags := append([]Flag{
		{Name: "server-name", Type: TypeString, Description: "入口服务器名或 ID（必填）"},
		{Name: "tag", Type: TypeString, Description: "可选：direct 入站 tag（默认 tunnel-<listen_port>，禁止 tunnel-in 或链式命名）"},
		{Name: "listen-port", Type: TypeInt, Description: "入口监听端口（1..65535；省略或 0 时自动在服务器端口范围内分配，同机节点禁止 443）"},
		{Name: "listen", Type: TypeString, Description: "监听地址（留空由服务器 IPv6 设置决定）"},
		{Name: "network", Type: TypeString, Description: "监听协议：tcp,udp（默认）、tcp 或 udp（引用已有入站时默认按目标协议推导）"},
		{Name: "sort-order", Type: TypeInt, Default: "50", Description: "入站与路由规则排序值（默认 50）"},
	}, targetFlags...)
	multiplexPlanFlags := append([]Flag{
		{Name: "server-name", Type: TypeString, Description: "入口服务器名或 ID（必填）"},
		{Name: "inbound-tag", Type: TypeString, Description: "要复用的已有活跃 direct 入站 tag（必填，禁止 tunnel-in）"},
		{Name: "domain", Type: TypeStrings, Description: "精确匹配的 SNI/HTTP 域名（至少提供一种匹配条件）"},
		{Name: "domain-suffix", Type: TypeStrings, Description: "后缀匹配的域名列表"},
		{Name: "ip-cidr", Type: TypeStrings, Description: "目标 IP/CIDR 列表"},
		{Name: "source-ip-cidr", Type: TypeStrings, Description: "来源 IP/CIDR 列表"},
		{Name: "sort-order", Type: TypeInt, Description: "可选：规则排序值（省略或 0 时自动排在现有兜底规则之前）"},
	}, targetFlags...)
	chainPlanFlags := append([]Flag{
		{Name: "label", Type: TypeString, Description: "链式隧道唯一标签（2..32 位小写字母/数字/短横线，首尾字母或数字；必填）"},
		{Name: "servers", Type: TypeStrings, Description: "按入口到出口顺序排列的至少 2 台互不重复服务器名或 ID 列表（每个值为完整服务器名或 ID；必填）"},
		{Name: "entry-port", Type: TypeInt, Description: "可选：入口机监听端口（1..65535；省略或 0 时自动分配，同机节点禁止 443）"},
		{Name: "network", Type: TypeString, Description: "监听协议：tcp,udp（默认）、tcp 或 udp"},
	}, targetFlags...)

	return []*Command{
		{Path: []string{"server", "steal", "status"}, Summary: "查询服务器偷取模式状态、443 绑定、tunnel-in 规则、推荐空闲端口与偷自己本地 nginx 预览（纯读库）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "steal", "plan"}, Summary: "规划服务器偷取模式部署、模式切换（tunnel / fallback）或关闭（off），生成单一待确认 Plan（同机节点禁止开启 443）", Class: ClassConfig,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "mode", Type: TypeString, Description: "偷取模式：tunnel（443 direct SNI 分流）、fallback（Reality 握手偷自己）或 off（关闭）（必填）"},
				{Name: "domain", Type: TypeString, Description: "伪装站与 SNI 域名（可选，留空沿用服务器当前 domain）"},
				{Name: "site-type", Type: TypeString, Description: "伪装站类型：decoy（默认）、static 或 proxy"},
				{Name: "site-value", Type: TypeString, Description: "伪装站值：内置模板名（landing/blog/docs/empty）、静态目录或上游 URL"},
				{Name: "internal-port", Type: TypeInt, Description: "可选（仅 tunnel 模式）：内部代理入站回环转发端口（1..65535，留空自动复用或分配）"},
				{Name: "inbound-tag", Type: TypeString, Description: "可选：指定要重映射的非 WS Reality 入站 tag（留空自动选择主 Reality 入站）"},
			}},
		{Path: []string{"tunnel", "list"}, Summary: "从主控数据库聚合分页列出散装任意门隧道（单跳入站与复用分流规则，排除 tunnel-in 与链式隧道）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "按入口服务器名或 ID 过滤"},
				{Name: "kind", Type: TypeString, Description: "按类型过滤：inbound（单跳入站）或 multiplex（复用分流规则）"},
			},
			Columns: []string{"kind", "server_name", "tag", "listen_port", "network", "target_address", "target_port", "enabled"}},
		{Path: []string{"tunnel", "plan"}, Summary: "规划单跳任意门端口转发（direct 入站 + 路由覆盖规则，支持固定目标或引用已有服务器入站）并生成待确认 Plan", Class: ClassConfig,
			Flags: tunnelPlanFlags},
		{Path: []string{"tunnel", "multiplex", "plan"}, Summary: "复用已有 direct 入站新增按域名/IP 分流转发的路由规则（不另开端口，自动排在兜底规则之前）并生成待确认 Plan", Class: ClassConfig,
			Flags: multiplexPlanFlags},
		{Path: []string{"tunnel", "delete-plan"}, Summary: "规划删除单跳任意门隧道（--server-name + --tag 连带删绑定规则）或单条复用分流规则（--rule-id）", Class: ClassConfig,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "删除单跳隧道时的服务器名或 ID（与 --tag 成对，和 --rule-id 二选一）"},
				{Name: "tag", Type: TypeString, Description: "要删除的单跳 direct 入站 tag（连带清理绑定规则；禁止 tunnel-in 与链式跳）"},
				{Name: "rule-id", Type: TypeInt, Description: "删除单条复用分流规则时的 RoutingRule ID（保留共享 direct 入站）"},
			}},
		{Path: []string{"tunnel", "chain", "list"}, Summary: "按 label 字典序分页聚合列出所有 N 台链式隧道（tunnel-<label>-h<i>）及完整性状态", Class: ClassRead, List: true,
			Columns: []string{"label", "hop_count", "entry_server_name", "entry_port", "final_target", "network", "complete"}},
		{Path: []string{"tunnel", "chain", "plan"}, Summary: "编排 N 台服务器（N>=2）首尾相接链式隧道：按跳分配端口并将全部 2N 个变更打包进单一原子 Plan", Class: ClassConfig,
			Flags: chainPlanFlags},
		{Path: []string{"tunnel", "chain", "delete-plan"}, Summary: "按 --label 一次性规划软删除整条链式隧道在全部服务器上的入站与绑定路由规则", Class: ClassConfig,
			Flags: []Flag{
				{Name: "label", Type: TypeString, Description: "要整链软删除的链式隧道标签（必填）"},
			}},
	}
}

// websiteCommands 是 m2-15 的网站（kind Website）查询、目录扫描、无状态向导与模板目录命令（node-nginx-websites）。
func websiteCommands() []*Command {
	return []*Command{
		{Path: []string{"website", "list"}, Summary: "分页列出 Website 站点配置与扫盘纳管状态（按服务器与域名升序）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
				{Name: "type", Type: TypeString, Description: "按站点类型过滤：static、proxy 或 decoy"},
				{Name: "include-deleted", Type: TypeBool, Description: "包含已软删除的站点"},
			},
			Columns: []string{"id", "server_id", "domain", "type", "use_443", "managed", "conf_path"}},
		{Path: []string{"website", "get"}, Summary: "查看单条 Website 站点配置与运行态扫盘信息", Class: ClassRead,
			Args: []Arg{{Name: "id", Description: "站点 ID（website list 里的 id）"}}},
		{Path: []string{"website", "scan"}, Summary: "扫描节点 nginx HTTP 站点目录：已纳管站点只更新 status 不抬版本，未纳管手工文件原样列出绝不覆盖", Class: ClassAction,
			Args: []Arg{{Name: "name", Description: "服务器名或 ID"}}},
		{Path: []string{"website", "build"}, Summary: "无状态站点向导：校验域名与防注入规则、支持静态/普通反代/WSS反代/伪装站并输出预览配置与 plan_op", Class: ClassRead,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "目标服务器名或 ID（必填）"},
				{Name: "domain", Type: TypeString, Description: "站点域名，如 app.example.com（必填）"},
				{Name: "type", Type: TypeString, Description: "站点类型：static、proxy 或 decoy（必填）"},
				{Name: "target", Type: TypeString, Description: "目标目录、反代上游 URL 或伪装模板名（landing/blog/docs/empty）"},
				{Name: "wss-path", Type: TypeString, Description: "可选（仅 proxy）：WebSocket 路径（如 /ws），自动生成 WSS 反代目标与 Upgrade 配置"},
				{Name: "certificate-id", Type: TypeInt, Description: "可选：关联证书 ID"},
				{Name: "use-443", Type: TypeBool, Description: "是否直接监听 443 端口（主控同机节点禁止启用）"},
			}},
		{Path: []string{"website", "templates"}, Summary: "列出内置站点类型、WSS 反代与伪装站模板目录及安全保护规则", Class: ClassRead},
	}
}

// proxyCommands 是 m2-13 的代理资源（Inbound / Outbound / RoutingRule）查询、向导、密钥生成、探测、预设与多机批量规划命令（node-proxy-resources）。
func proxyCommands() []*Command {
	idArg := func(desc string) []Arg { return []Arg{{Name: "id", Description: desc}} }
	inboundBuildFlags := []Flag{
		{Name: "tag", Type: TypeString, Description: "入站 tag（留空按 <protocol>-<port> 自动生成）"},
		{Name: "protocol", Type: TypeString, Description: "入站协议（见 inbound protocols）"},
		{Name: "port", Type: TypeInt, Description: "监听端口（1..65535；省略或 0 时按目标服务器偷取模式与端口范围自动分配，同机节点禁止 443）"},
		{Name: "listen", Type: TypeString, Description: "监听地址（留空由服务器 IPv6 设置决定）"},
		{Name: "transport", Type: TypeString, Description: "传输层：tcp、ws、grpc、http、httpupgrade 或 quic"},
		{Name: "security", Type: TypeString, Description: "安全层：none、tls 或 reality"},
		{Name: "sni", Type: TypeString, Description: "TLS / Reality server_name（SNI）"},
		{Name: "handshake", Type: TypeString, Description: "Reality / ShadowTLS 握手目标（host 或 host:port，默认 443）"},
		{Name: "path", Type: TypeString, Description: "WebSocket / HTTP / HTTPUpgrade 路径"},
		{Name: "service-name", Type: TypeString, Description: "gRPC service_name"},
		{Name: "cipher", Type: TypeString, Description: "Shadowsocks 加密算法（默认 2022-blake3-aes-128-gcm）"},
		{Name: "flow", Type: TypeString, Description: "VLESS 流控（tcp + tls/reality 默认 xtls-rprx-vision）"},
		{Name: "cert-path", Type: TypeString, Description: "TLS 证书路径（certificate_path）"},
		{Name: "key-path", Type: TypeString, Description: "TLS 私钥路径（key_path）"},
		{Name: "user-name", Type: TypeString, Description: "默认凭据用户名（默认 default）"},
		{Name: "password", Type: TypeString, Secret: true, Description: "自定义密码或 PSK（留空自动生成随机强密码）"},
		{Name: "uuid", Type: TypeString, Secret: true, Description: "自定义 UUID（留空自动生成 UUIDv4）"},
		{Name: "sort-order", Type: TypeInt, Description: "入站渲染排序值（默认 0）"},
	}
	buildFlagsWithServer := append([]Flag{
		{Name: "server-name", Type: TypeString, Description: "目标服务器名或 ID（必填）"},
	}, inboundBuildFlags...)
	batchInboundFlags := append([]Flag{
		{Name: "servers", Type: TypeStrings, Description: "目标服务器名或 ID 列表（每个值为完整服务器名或 ID，与 --all 二选一）"},
		{Name: "all", Type: TypeBool, Description: "对当前全部服务器批量规划（与 --servers 二选一）"},
		{Name: "spec", Type: TypeString, Secret: true, Description: "可选：直接提供 Inbound spec 模板 JSON（不传则使用向导参数逐机生成独立密钥）"},
	}, inboundBuildFlags...)

	return []*Command{
		// 入站（8 条）
		{Path: []string{"inbound", "list"}, Summary: "分页列出入站配置（按服务器与排序升序；无 secrets 权限打码 tls/settings）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
				{Name: "protocol", Type: TypeString, Description: "按协议名过滤"},
				{Name: "include-deleted", Type: TypeBool, Description: "包含已软删除的入站"},
			},
			Columns: []string{"id", "server_id", "tag", "protocol", "port", "enabled"}},
		{Path: []string{"inbound", "get"}, Summary: "查看单条入站配置（含实时派生的 Reality 公钥；无 secrets 权限打码 tls/settings）", Class: ClassRead,
			Args: idArg("入站 ID（inbound list 里的 id）")},
		{Path: []string{"inbound", "protocols"}, Summary: "查看 sing-box@v1.14.4 原生入站协议矩阵、传输/安全组合规则与并行协议线说明", Class: ClassRead},
		{Path: []string{"inbound", "examples"}, Summary: "查看纯 Go 内嵌的 sing-box 入站配置示例库（可按协议/传输/安全筛选）", Class: ClassRead,
			Flags: []Flag{
				{Name: "protocol", Type: TypeString, Description: "按协议筛选"},
				{Name: "transport", Type: TypeString, Description: "按传输层筛选"},
				{Name: "security", Type: TypeString, Description: "按安全层筛选"},
			}},
		{Path: []string{"inbound", "keygen"}, Summary: "使用加密随机源生成 UUID、Reality X25519 密钥对、short_id、强密码、SS2022 PSK 与 WireGuard 密钥对", Class: ClassRead,
			Flags: []Flag{
				{Name: "type", Type: TypeString, Default: "all", Description: "生成类型：all（默认）、uuid、x25519、short-id、password、ss2022 或 wireguard"},
				{Name: "cipher", Type: TypeString, Default: "2022-blake3-aes-128-gcm", Description: "SS2022 密钥算法"},
			}},
		{Path: []string{"inbound", "build"}, Summary: "无状态入站向导：校验协议组合、自动生成密钥并输出可直接提交 plan 的 InboundSpec", Class: ClassRead,
			Flags: buildFlagsWithServer},
		{Path: []string{"inbound", "batch", "list"}, Summary: "按 id 倒序分页列出多机批量入站批次记录（batch_inbounds）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "batch-id", Type: TypeString, Description: "按批次编号过滤"},
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
			},
			Columns: []string{"id", "batch_id", "server_id", "tag", "protocol", "port", "created_at"}},
		{Path: []string{"inbound", "batch", "plan"}, Summary: "为多台服务器批量规划入站并生成一份待确认 Plan（提交经 apply 原子落库与记录批次）", Class: ClassConfig,
			Flags: batchInboundFlags},

		// 出站（6 条）
		{Path: []string{"outbound", "list"}, Summary: "分页列出出站配置与最近一次探测状态（无 secrets 权限打码 settings）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
				{Name: "balancer-group", Type: TypeString, Description: "按负载均衡组名过滤"},
				{Name: "include-deleted", Type: TypeBool, Description: "包含已软删除的出站"},
			},
			Columns: []string{"id", "server_id", "tag", "protocol", "balancer_group", "is_warp"}},
		{Path: []string{"outbound", "get"}, Summary: "查看单条出站配置与探测状态（无 secrets 权限打码 settings）", Class: ClassRead,
			Args: idArg("出站 ID（outbound list 里的 id）")},
		{Path: []string{"outbound", "probe"}, Summary: "经在线节点运行中 sing-box 内核的指定出站实测 HTTP 可达性与延迟，并回写探测状态（不抬升版本）", Class: ClassAction,
			Args: idArg("出站 ID（outbound list 里的 id）"),
			Flags: []Flag{
				{Name: "url", Type: TypeString, Description: "探测目标 http(s) URL（默认复用出站 probe.url 或 gstatic generate_204）"},
				{Name: "timeout", Type: TypeDuration, Default: "10s", Description: "单次探测超时（1s..30s，默认 10s）"},
			}},
		{Path: []string{"outbound", "landing", "plan"}, Summary: "为源服务器规划落地出站与对应绑定路由规则并生成待确认 Plan（支持引用另一台服务器入站自动派生与 --detour）", Class: ClassConfig,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "源服务器名或 ID（必填）"},
				{Name: "outbound-tag", Type: TypeString, Description: "新建落地出站的 tag（必填）"},
				{Name: "inbound-tag", Type: TypeString, Description: "仅对该入站落地（省略时生成整机 catch_all 兜底路由）"},
				{Name: "target-server", Type: TypeString, Description: "落地目标服务器名或 ID（与 --target-inbound 配合自动派生客户端出站）"},
				{Name: "target-inbound", Type: TypeString, Description: "落地目标服务器上的入站 tag"},
				{Name: "protocol", Type: TypeString, Description: "手工指定落地出站协议（与 --target-server 二选一）"},
				{Name: "settings", Type: TypeString, Secret: true, Description: "手工指定落地出站 settings JSON 对象"},
				{Name: "detour", Type: TypeString, Description: "可选：前置链式跳转出站 tag"},
				{Name: "balancer-group", Type: TypeString, Description: "可选：加入的负载均衡组名"},
				{Name: "sort-order", Type: TypeInt, Default: "100", Description: "路由规则排序值（默认 100）"},
			}},
		{Path: []string{"outbound", "batch", "list"}, Summary: "按 id 倒序分页列出多机批量出站批次记录（batch_outbounds）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "batch-id", Type: TypeString, Description: "按批次编号过滤"},
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
			},
			Columns: []string{"id", "batch_id", "server_id", "tag", "protocol", "created_at"}},
		{Path: []string{"outbound", "batch", "plan"}, Summary: "为多台服务器批量规划出站并生成一份待确认 Plan（提交经 apply 原子落库与记录批次）", Class: ClassConfig,
			Flags: []Flag{
				{Name: "servers", Type: TypeStrings, Description: "目标服务器名或 ID 列表（每个值为完整服务器名或 ID，与 --all 二选一）"},
				{Name: "all", Type: TypeBool, Description: "对当前全部服务器批量规划（与 --servers 二选一）"},
				{Name: "tag", Type: TypeString, Description: "出站 tag（必填）"},
				{Name: "protocol", Type: TypeString, Description: "出站协议（必填）"},
				{Name: "settings", Type: TypeString, Secret: true, Description: "出站 settings JSON 对象"},
				{Name: "balancer-group", Type: TypeString, Description: "可选：负载均衡组名"},
				{Name: "probe", Type: TypeString, Description: "可选：出站组探测配置 JSON（如 strategy/url/interval/tolerance）"},
			}},

		// 路由与预设（6 条）
		{Path: []string{"routing", "list"}, Summary: "按排序分页列出路由规则，并标出入站全兜底与被前序规则遮蔽的告警提示", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "server-name", Type: TypeString, Description: "按服务器名或 ID 过滤"},
				{Name: "include-deleted", Type: TypeBool, Description: "包含已软删除的路由规则"},
			},
			Columns: []string{"id", "server_id", "sort_order", "outbound_tag", "enabled", "catch_all"}},
		{Path: []string{"routing", "get"}, Summary: "查看单条路由规则及遮蔽分析", Class: ClassRead,
			Args: idArg("路由规则 ID（routing list 里的 id）")},
		{Path: []string{"routing", "preset", "builtin"}, Summary: "列出内置 sing-box@v1.14.4 快捷路由预设及配套 rule_set 定义（零废弃 geoip/geosite）", Class: ClassRead},
		{Path: []string{"routing", "preset", "list"}, Summary: "分页列出已保存的用户自定义路由快捷预设（routing_rule_presets）", Class: ClassRead, List: true,
			Flags:   []Flag{{Name: "owner", Type: TypeString, Description: "按归属账号过滤"}},
			Columns: []string{"id", "username", "name", "updated_at"}},
		{Path: []string{"routing", "preset", "save"}, Summary: "保存一条命名路由规则快捷预设（校验合法 JSON 并拒收废弃 geoip/geosite/user 键）", Class: ClassAction,
			Flags: []Flag{
				{Name: "name", Type: TypeString, Description: "预设名称（1..128 字符，必填）"},
				{Name: "rule", Type: TypeString, Description: "sing-box 路由 match JSON 对象（必填）"},
				{Name: "owner", Type: TypeString, Description: "可选：归属账号（本机管理员留空自动回落主管理员）"},
			}},
		{Path: []string{"routing", "preset", "delete"}, Summary: "删除一条已保存的自定义路由快捷预设（删除类危险操作，需 --confirm <id>）", Class: ClassAction,
			Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmObject, Arg: "id"},
			Args: idArg("预设 ID（routing preset list 里的 id）")},
	}
}

// configTxCommands 是 m2-12 的配置类唯一写路径与快照回滚命令（node-config-tx）。
func configTxCommands() []*Command {
	return []*Command{
		{Path: []string{"plan"}, Summary: "预演配置类资源变更（Server/Inbound/Outbound/RoutingRule/Website），返回逐对象与节点配置 diff、所需危险类与确认提示", Class: ClassConfig,
			Flags: []Flag{
				{Name: "spec", Type: TypeString, Description: "内联 JSON 字符串（单条变更对象或变更对象数组；与 --file 二选一）"},
				{Name: "file", Type: TypeFile, Description: "CLI 本地 JSON 文件路径或 -（读 stdin 并内联发给主控；REST/MCP 拒绝）"},
			}},
		{Path: []string{"apply"}, Summary: "确认并事务提交一份预演计划（写前快照、递增版本并串行推送节点原子生效；危险类与同机 nginx 人类验证按实时状态动态判定）",
			Class: ClassConfig, DynamicDanger: true, DynamicHuman: true,
			Flags: []Flag{
				{Name: "plan", Type: TypeInt, Description: "要提交的计划编号（plan 或 snapshot rollback 返回的 plan_id）"},
				{Name: "force", Type: TypeBool, Description: "跳过乐观锁 resourceVersion 校验（仍拒绝 diff 漂移；触发 permission 危险类与 --confirm）"},
			}},
		{Path: []string{"plans", "list"}, Summary: "按 id 倒序分页列出配置变更预演计划（只对管理员开放）", Class: ClassRead, List: true,
			Flags:   []Flag{{Name: "status", Type: TypeString, Description: "按状态过滤：pending、applied、expired 或 rejected"}},
			Columns: []string{"id", "status", "affected_count", "apply_id", "expires_at", "created_at"}},
		{Path: []string{"plans", "get"}, Summary: "查看单份配置变更预演计划详情与 diff（无 secrets 权限时敏感键自动打码）", Class: ClassRead,
			Args: []Arg{{Name: "id", Description: "计划编号（plans list 里的 id）"}}},
		{Path: []string{"snapshot", "list"}, Summary: "按 id 倒序分页列出配置类对象写前快照元数据（不含正文，只对管理员开放）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "kind", Type: TypeString, Description: "按资源类型过滤，如 Server、Inbound、Outbound、RoutingRule、Website"},
				{Name: "object-id", Type: TypeInt, Description: "按对象 ID 过滤"},
				{Name: "apply-id", Type: TypeString, Description: "按 apply 批次编号过滤"},
			},
			Columns: []string{"id", "object_kind", "object_id", "object_version", "source", "apply_id", "created_at"}},
		{Path: []string{"snapshot", "get"}, Summary: "查看单条配置快照内容（无 secrets 权限时敏感字段自动打码）", Class: ClassRead,
			Args: []Arg{{Name: "id", Description: "快照编号（snapshot list 里的 id）"}}},
		{Path: []string{"snapshot", "rollback"}, Summary: "按单条快照或 apply_id 批次生成一份新的待确认回滚计划（不倒退版本、不复活已软删除对象）", Class: ClassConfig,
			Flags: []Flag{
				{Name: "id", Type: TypeInt, Description: "单条快照 ID（与 --apply-id 二选一）"},
				{Name: "apply-id", Type: TypeString, Description: "整批 apply_id（与 --id 二选一）"},
			}},
	}
}

// serverCommands 是 m2-02 的服务器命令（node-pairing、node-channel）。server add 是配置类，plan/apply 实现之前直接建对象，
// Server 的修改与删除等 plan/apply 到了再加；安装命令里的 token 原文只给带密钥读取的身份。
func serverCommands() []*Command {
	name := Arg{Name: "name", Description: "服务器的名字（server list 里的 name）"}
	inbound := Flag{Name: "inbound", Type: TypeString, Description: "入站的 tag（节点 kernel.json 里的 inbounds[].tag）"}
	credential := Flag{Name: "credential", Type: TypeString, Description: "凭据名（入站里用户的名字）"}
	which := Flag{Name: "which", Type: TypeString, Default: "all", Description: "凭据范围：all（默认）、token、agent 或 pull"}
	return []*Command{
		{Path: []string{"server", "add"}, Summary: "加一台服务器并生成三把通信凭据（支持 auto、websocket、http、pull 四种连接模式；建了以后其它规格改删等 plan/apply）", Class: ClassConfig,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "mode", Type: TypeString, Default: "auto", Description: "连接模式：auto（默认）、websocket、http 或 pull"},
				{Name: "domain", Type: TypeString, Description: "管理入口域名或 IPv4 主机名"},
				{Name: "domain-v6", Type: TypeString, Description: "管理入口 IPv6 域名"},
				{Name: "listen-port", Type: TypeInt, Description: "节点管理监听端口（0 表示按默认 23889 使用）"},
				{Name: "pull-address", Type: TypeString, Description: "轮询只读口 IPv4 或主机名（pull 模式必填至少一个可达地址）"},
				{Name: "pull-address-v6", Type: TypeString, Description: "轮询只读口 IPv6 地址"},
				{Name: "pull-port", Type: TypeInt, Description: "节点只读轮询端口（0 表示按默认 23890 使用）"},
			}},
		{Path: []string{"server", "install-command"}, Summary: "给出这台服务器的一键安装命令（主控地址必须是 https；凭据原文要密钥读取）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "list"}, Summary: "列出服务器与在线状态（pending 还没连上过，connected 在线，offline 离线）", Class: ClassRead, List: true,
			Columns: []string{"id", "name", "connection_mode", "status", "last_heartbeat", "ip_address"}},
		{Path: []string{"server", "get"}, Summary: "查看一台服务器的完整对象（三把凭据一律打码）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "token", "show"}, Summary: "查看服务器的三把凭据原文与轮换/吊销待确认状态（需要 secrets 权限范围）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "token", "rotate"}, Summary: "平滑轮换服务器凭据并推送节点原子落盘（开启短暂双凭据窗口；需 permission 危险权限与 --confirm）",
			Class: ClassAction, Danger: v1.DangerPermission, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args: []Arg{name}, Flags: []Flag{which}},
		{Path: []string{"server", "token", "revoke"}, Summary: "立即吊销服务器凭据并踢断活跃连接、清理会话缓存（不推新凭据；需 permission 危险权限与 --confirm）",
			Class: ClassAction, Danger: v1.DangerPermission, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args: []Arg{name}, Flags: []Flag{which}},
		{Path: []string{"server", "limit", "set"}, Summary: "给一台在线服务器上「入站 × 凭据」设主控临时限速（0 表示这一份显式不限速；结果里有三份来源与生效值）", Class: ClassAction,
			Args: []Arg{name}, Flags: []Flag{inbound, credential,
				{Name: "mbps", Type: TypeInt, Description: "限速值，单位 Mbps，不小于 0；0 表示这一份显式不限速"},
				{Name: "expires-in", Type: TypeDuration, Description: "多久之后自动撤掉，如 30m；不给或为 0 表示一直有效"}}},
		{Path: []string{"server", "limit", "clear"}, Summary: "撤掉一台在线服务器上「入站 × 凭据」的主控临时限速（让这一份退出合成，与设成 0 不同）", Class: ClassAction,
			Args: []Arg{name}, Flags: []Flag{inbound, credential}},
		{Path: []string{"server", "kick"}, Summary: "踢掉一台在线服务器上某个凭据的连接（不给 --inbound 时踢它在所有入站上的连接；踢线不是封禁）", Class: ClassAction,
			Args: []Arg{name}, Flags: []Flag{credential, {Name: "inbound", Type: TypeString, Description: "入站的 tag；不给时作用到所有入站"}}},
		{Path: []string{"server", "connections"}, Summary: "列出一台在线服务器上「入站 × 凭据」的活动连接", Class: ClassRead, List: true,
			Args: []Arg{name}, Flags: []Flag{inbound, credential},
			Columns: []string{"id", "network", "peer", "outbound", "start"}},
		{Path: []string{"server", "traffic"}, Summary: "看主控收上来的流量累计（按入站、出站、凭据分上下行；在主控内存里，主控重启后从零开始）与内核重启次数", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "restart-kernel"}, Summary: "让一台在线服务器重新读 kernel.json 并在同一个进程里重启内核（已有连接会断开，计数与限速来源保留）",
			Class: ClassAction, Danger: v1.DangerRestart, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"}, Args: []Arg{name}},
		{Path: []string{"server", "update", "check"}, Summary: "检查节点 satchel-agent 最新可用版本、主控兼容性与容器约束（省略服务器名查全部）", Class: ClassRead,
			Args:  []Arg{{Name: "name", Description: "服务器的名字（省略时汇总全部服务器）", Optional: true}},
			Flags: []Flag{{Name: "channel", Type: TypeString, Default: "stable", Description: "更新渠道：stable 或 prerelease"}}},
		{Path: []string{"server", "update", "apply"}, Summary: "向单台在线非容器节点发起纯 Go 自更新并重连核对新版本（长任务；需 restart 危险类与 --confirm）",
			Class: ClassAction, Shape: ShapeJob, Danger: v1.DangerRestart, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args:  []Arg{name, {Name: "version", Description: "目标版本号，必须等于所选渠道当前最新版本，如 0.2.0"}},
			Flags: []Flag{{Name: "channel", Type: TypeString, Default: "stable", Description: "更新渠道：stable 或 prerelease"}}},
		{Path: []string{"server", "update", "batch"}, Summary: "向多台或全部在线节点逐台串行发起自更新（长任务；需 batch 与 restart 危险类，--confirm 填目标台数）",
			Class: ClassAction, Shape: ShapeJob, Danger: v1.DangerBatch, Confirm: &Confirm{Kind: ConfirmCount},
			Args: []Arg{{Name: "version", Description: "目标版本号，必须等于所选渠道当前最新版本，如 0.2.0"}},
			Flags: []Flag{
				{Name: "servers", Type: TypeStrings, Description: "目标服务器名列表（每个值为完整服务器名，与 --all 二选一）"},
				{Name: "all", Type: TypeBool, Description: "升级全部已登记的服务器（与 --servers 二选一）"},
				{Name: "channel", Type: TypeString, Default: "stable", Description: "更新渠道：stable 或 prerelease"},
			}},
		{Path: []string{"server", "uninstall"}, Summary: "远程卸载物理/虚机节点 satchel-agent 并将其置为 offline（拒绝 Docker 与 exec 中节点；需 master 危险类与 --confirm）",
			Class: ClassAction, Danger: v1.DangerMaster, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args: []Arg{name}},
		{Path: []string{"server", "switch-listen-port"}, Summary: "热切换节点管理端口与轮询端口（进程内平滑重绑，不断开内核代理连接；需 permission 危险类与 --confirm）",
			Class: ClassAction, Danger: v1.DangerPermission, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "listen-port", Type: TypeInt, Description: "新的节点管理监听端口（1..65535）"},
				{Name: "pull-port", Type: TypeInt, Description: "新的节点只读轮询监听端口（1..65535）"},
			}},
		{Path: []string{"server", "probe-master-url"}, Summary: "让指定节点用本地主控公钥握手探测主控地址或备用恢复地址连通性（只读防 SSRF）", Class: ClassRead,
			Args:  []Arg{name},
			Flags: []Flag{{Name: "target", Type: TypeString, Default: "master", Description: "要探测的内置目标：master（主控地址）或 recovery（恢复地址）"}}},
		{Path: []string{"server", "system-info"}, Summary: "采集一台在线节点的系统与硬件只读信息（主机名、系统、CPU、内存、磁盘、负载与 TFO）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "nics"}, Summary: "列出在线节点启用中的非回环网卡及可绑定 IPv4/IPv6 地址与作用域（global/private/ula）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "speed"}, Summary: "查看服务器实时上下行网速（读主控内存/库快照，不触发节点 RPC；省略服务器名查全部）", Class: ClassRead,
			Args: []Arg{{Name: "name", Description: "服务器的名字（省略时返回全部服务器的实时网速）", Optional: true}}},
		{Path: []string{"server", "traffic-stats", "set"}, Summary: "配置服务器的流量统计口径（方向、数据源与是否计入汇总，乐观并发需带 --resource-version）", Class: ClassConfig,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "resource-version", Type: TypeInt, Description: "当前的 metadata.resourceVersion（server get 里的值）；不匹配整单拒绝"},
				{Name: "mode", Type: TypeString, Description: "流量统计方向：both、upload、download 或 max"},
				{Name: "source", Type: TypeString, Description: "流量统计数据源：core（内核统计）或 system（物理网卡统计）"},
				{Name: "include", Type: TypeString, Description: "是否纳入流量统计汇总：true 或 false"},
			}},
		{Path: []string{"server", "logs", "list"}, Summary: "远程只读分页查看节点日志（支持 agent、core、nginx；不提供删除或清空）", Class: ClassRead, List: true,
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "service", Type: TypeString, Default: "agent", Description: "日志来源：agent（默认）、core 或 nginx"},
				{Name: "level", Type: TypeString, Description: "只看这个级别及更高的：debug、info、warn、error"},
				{Name: "grep", Type: TypeString, Secret: true, Description: "只看原文含这段文本的行（区分大小写；审计摘要打码）"},
				{Name: "file", Type: TypeString, Description: "看 server logs files 列出的某个白名单文件（仅支持 agent 或 core）"},
			},
			Columns: []string{"time", "level", "msg"}},
		{Path: []string{"server", "logs", "files"}, Summary: "远程只读分页列出节点日志目录里的 agent 与内核日志文件", Class: ClassRead, List: true,
			Args:    []Arg{name},
			Columns: []string{"name", "size", "modified", "active"}},
		{Path: []string{"server", "config", "status"}, Summary: "远程查询节点当前 kernel.json 哈希、内核运行态及与主控期望配置的同步状态", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "config", "files"}, Summary: "远程只读分页列出节点白名单配置目录文件（仅 kernel.json 与 nginx .conf，排除 config.yaml 与私钥）", Class: ClassRead, List: true,
			Args:    []Arg{name},
			Flags:   []Flag{{Name: "scope", Type: TypeString, Default: "all", Description: "目录范围：all（默认）、kernel 或 nginx"}},
			Columns: []string{"scope", "path", "size", "modified"}},
		{Path: []string{"server", "config", "read"}, Summary: "远程只读查看节点白名单配置文件内容（上限 512 KiB；无 secrets 权限自动打码敏感字段）", Class: ClassRead,
			Args:  []Arg{name},
			Flags: []Flag{{Name: "path", Type: TypeString, Description: "要读取的白名单文件完整路径（见 server config files 输出）"}}},
		{Path: []string{"server", "config", "test"}, Summary: "用主控当前渲染的规范 kernel.json 在节点上做无侵入 sing-box 校验（不中断运行中连接）", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "service", "status"}, Summary: "远程查询节点 kernel、nginx 与 agent 三项服务运行状态与初始化方式", Class: ClassRead,
			Args: []Arg{name}},
		{Path: []string{"server", "service", "control"}, Summary: "远程启停或重载节点 kernel 或 nginx 服务（重启类危险操作；同机 nginx 操作触发七组当场人类验证）",
			Class: ClassAction, DynamicHuman: true, Danger: v1.DangerRestart, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args: []Arg{name},
			Flags: []Flag{
				{Name: "service", Type: TypeString, Description: "目标服务：kernel 或 nginx（禁止控制 agent）"},
				{Name: "action", Type: TypeString, Description: "执行动作：start、stop、restart（nginx 额外支持 reload）"},
			}},
		{Path: []string{"server", "nginx", "install"}, Summary: "远程在节点安装并初始化 nginx（长任务；Docker 节点直接回复已预装；同机节点触发七组当场人类验证）",
			Class: ClassAction, Shape: ShapeJob, DynamicHuman: true,
			Args: []Arg{name}},
		{Path: []string{"server", "nginx", "uninstall"}, Summary: "远程卸载节点 nginx（长任务，删除类危险操作；Docker 节点拒绝卸载；同机节点触发七组当场人类验证）",
			Class: ClassAction, Shape: ShapeJob, DynamicHuman: true, Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args: []Arg{name}},
		{Path: []string{"server", "nginx", "servers"}, Summary: "远程只读分页巡检节点白名单配置目录内的 nginx HTTP 与 stream server 块（标注是否 Satchel 纳管）",
			Class: ClassRead, List: true,
			Args:    []Arg{name},
			Columns: []string{"kind", "conf_path", "managed", "server_names", "listen_ports", "has_ssl"}},
		{Path: []string{"server", "nginx", "clear-stream"}, Summary: "清理节点 /etc/nginx/stream_servers/ 下指定端口的 Satchel 纳管配置并重载（删除类；保护主控同机 443 与未纳管文件，同机触发七组当场验证）",
			Class: ClassAction, DynamicHuman: true, Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"},
			Args:  []Arg{name},
			Flags: []Flag{{Name: "port", Type: TypeInt, Description: "要清理的目标 stream 监听端口（1..65535，同机 443 受保护）"}}},
	}
}

// updateCommands 是 m1-09 的主控自升级与更新 CDN 命令（master-self-update）。升级与改更新 CDN 开关是第 05 章危险操作的
// 「主控自身类」：令牌要单独开这一类，每次都要带 confirm（升级填目标版本号，开关填要设的值）。
func updateCommands() []*Command {
	channel := Flag{Name: "channel", Type: TypeString, Default: "stable", Description: "更新渠道：stable 或 prerelease"}
	return []*Command{
		{Path: []string{"update", "check"}, Summary: "检查所选渠道的最新版本（先问更新 CDN，再问 GitHub），以及这台主控能不能自升级", Class: ClassRead,
			Flags: []Flag{channel}},
		{Path: []string{"update", "apply"}, Summary: "把主控升到所选渠道的最新版（长任务；验签、升级前备份、替换后原地重启，失败时旧二进制与备份一起回）",
			Class: ClassAction, Shape: ShapeJob, Danger: v1.DangerMaster, Confirm: &Confirm{Kind: ConfirmObject, Arg: "version"},
			Args:  []Arg{{Name: "version", Description: "目标版本号，必须等于 update check 看到的最新版本，如 0.1.1"}},
			Flags: []Flag{channel}},
		{Path: []string{"settings", "update-cdn", "set"}, Summary: "打开或关掉更新 CDN（关掉后检查更新与下载直接走 GitHub）", Class: ClassMasterSettings,
			Danger: v1.DangerMaster, Confirm: &Confirm{Kind: ConfirmObject, Arg: "enabled"},
			Args:  []Arg{{Name: "enabled", Description: "true 或 false"}},
			Flags: []Flag{{Name: "resource-version", Type: TypeInt, Description: "当前的 metadata.resourceVersion（settings show 里的值）；不匹配整单拒绝"}}},
	}
}

// databaseCommands 是 m1-08 的数据库设置与在线迁移命令（master-db-migration）。迁移是第 05 章七组的「数据库切换与在线迁移」：
// 人类专属，要当场验证，令牌与 MCP 一律做不了。
func databaseCommands() []*Command {
	target := []Flag{
		{Name: "host", Type: TypeString, Description: "目标 PostgreSQL 的主机"},
		{Name: "port", Type: TypeInt, Default: "5432", Description: "目标 PostgreSQL 的端口"},
		{Name: "name", Type: TypeString, Description: "目标库名"},
		{Name: "user", Type: TypeString, Description: "目标库的用户"},
		{Name: "password", Type: TypePassword, Description: "目标库的密码（CLI 上从终端读）"},
		{Name: "sslmode", Type: TypeString, Default: "prefer", Description: "disable、allow、prefer、require、verify-ca、verify-full 之一"},
	}
	return []*Command{
		{Path: []string{"database", "show"}, Summary: "当前用的数据库：驱动、SQLite 的库文件或 PostgreSQL 的连接参数（不含密码）", Class: ClassRead},
		{Path: []string{"database", "test"}, Summary: "只读地试连一个 PostgreSQL：版本、是否为空、这台主控的备份工具够不够用", Class: ClassAction, Flags: target},
		{Path: []string{"database", "migrate"}, Summary: "把主控从 SQLite 在线迁移到一个空的 PostgreSQL（长任务；人类专属：当场验证；迁移期间只能查 job，成功后主控重启）",
			Class: ClassAction, Shape: ShapeJob, HumanOnly: true, Flags: target},
	}
}

// backupCommands 是 m1-07 的长任务与整库备份命令（master-jobs、master-backup）。本机生成与上传备份只要求管理员；
// 「把备份下载到主控之外」与「恢复备份」是第 05 章七组的整库操作：人类专属，要当场验证，令牌与 MCP 一律做不了。
func backupCommands() []*Command {
	name := Arg{Name: "name", Description: "backup list 里的备份名，如 satchel-backup-20260925T081500Z.zip"}
	return []*Command{
		{Path: []string{"job", "get"}, Summary: "查一个长任务：状态、开始与结束时间、退出码、结果", Class: ClassRead,
			Args: []Arg{{Name: "job", Description: "受理时返回的 job_id，如 job-0123456789abcdef"}}},
		{Path: []string{"job", "list"}, Summary: "按 id 倒序列出长任务", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "status", Type: TypeString, Description: "只看这个状态：queued、running、done、failed、unknown"},
				{Name: "server-name", Type: TypeString, Description: "只看关联这台服务器的长任务（按服务器名精确匹配）"},
			},
			Columns: []string{"job_id", "kind", "status", "created_at", "finished_at", "exit_code"}},
		{Path: []string{"backup", "create"}, Summary: "在主控本机的 backups/ 生成一份整库备份（长任务；最多留 7 份）", Class: ClassAction, Shape: ShapeJob},
		{Path: []string{"backup", "list"}, Summary: "列出主控本机 backups/ 里的备份，从新到旧", Class: ClassRead, List: true,
			Columns: []string{"name", "size", "created_at", "driver"}},
		{Path: []string{"backup", "upload"}, Summary: "把本地的一份备份传到主控的 backups/（先校验；最大 4 GiB）", Class: ClassAction, Shape: ShapeUpload,
			Flags: []Flag{{Name: UploadFlag, Type: TypeFile, Description: "本地备份文件的路径"}}},
		{Path: []string{"backup", "download"}, Summary: "把一份备份下载到本地（人类专属：当场验证；备份里有主控通信密钥与全部数据）", Class: ClassAction,
			Shape: ShapeDownload, HumanOnly: true, Args: []Arg{name},
			Flags: []Flag{{Name: DownloadFlag, Type: TypeFile, Description: "写到本地的这个路径（已存在就拒绝，权限 0600）"}}},
		{Path: []string{"backup", "restore"}, Summary: "用一份备份恢复整个主控（人类专属：当场验证；主控随后重启，在启动时换库）", Class: ClassAction,
			HumanOnly: true, Args: []Arg{name}},
	}
}

// opsCommands 是 m1-06 的系统日志与内置定时任务命令（master-logs、master-scheduler），都只对管理员开放（处理函数里判）。
func opsCommands() []*Command {
	return []*Command{
		{Path: []string{"logs", "list"}, Summary: "从新到旧列出主控日志文件里的行（只扫文件末尾 50000 行）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "level", Type: TypeString, Description: "只看这个级别及更高的：debug、info、warn、error"},
				{Name: "grep", Type: TypeString, Secret: true, Description: "只看原文里含这段文本的行（区分大小写；可能是在找令牌或密码，审计里打码）"},
				{Name: "file", Type: TypeString, Description: "看 logs files list 里列出的某个文件，默认是当前文件"},
			},
			Columns: []string{"time", "level", "msg"}},
		{Path: []string{"logs", "files", "list"}, Summary: "列出日志文件：当前文件与轮转下来的旧文件", Class: ClassRead, List: true,
			Columns: []string{"name", "size", "modified", "active"}},
		{Path: []string{"schedule", "list"}, Summary: "列出内置定时任务：间隔与最近一次运行的结果", Class: ClassRead, List: true,
			Columns: []string{"name", "interval", "last_started_at", "last_status", "summary"}},
		{Path: []string{"schedule", "runs", "list"}, Summary: "按 id 倒序（写入的先后）列出内置定时任务的运行记录", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "task", Type: TypeString, Description: "只看这个任务（精确匹配，如 audit_cleanup）"},
				{Name: "status", Type: TypeString, Description: "只看这个状态：running、ok、error"},
			},
			Columns: []string{"id", "task", "started_at", "duration_ms", "status", "detail"}},
	}
}

// securityCommands 是 m1-05 的安全事件与 IP 封禁命令（master-login-protection）。封禁的创建与解除属第 05 章七组的「门」：
// 人类专属，要当场验证，令牌与 MCP 一律拒绝。两条列表命令只对管理员开放（处理函数里判）。
func securityCommands() []*Command {
	ip := Arg{Name: "ip", Description: "单个 IP 地址，如 198.51.100.7 或 2001:db8::1"}
	return []*Command{
		{Path: []string{"security", "events", "list"}, Summary: "按时间倒序列出安全事件：登录与当场验证的失败、令牌校验失败、封禁与解封", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "kind", Type: TypeString, Description: "只看这一种：login_fail、login_locked、verify_fail、verify_locked、probe、ban、ban_manual、unban"},
				{Name: "ip", Type: TypeString, Description: "只看这个来源 IP 的"},
			},
			Columns: []string{"id", "at", "kind", "ip", "username", "path", "detail", "actor"}},
		{Path: []string{"security", "bans", "list"}, Summary: "列出生效中的 IP 封禁（被封的 IP 带令牌的请求一律拒绝）", Class: ClassRead, List: true,
			Columns: []string{"ip", "reason", "banned_at", "expires_at", "permanent", "fail_count", "actor"}},
		{Path: []string{"security", "ban"}, Summary: "封禁一个 IP：它带令牌的请求一律拒绝（人类专属：当场验证；默认按 brute_force_block_minutes 到期）", Class: ClassAction, HumanOnly: true,
			Args:  []Arg{ip},
			Flags: []Flag{{Name: "permanent", Type: TypeBool, Description: "永久封禁，直到解封"}}},
		{Path: []string{"security", "unban"}, Summary: "解除一个 IP 的封禁（人类专属：当场验证）", Class: ClassAction, HumanOnly: true,
			Args: []Arg{ip}},
	}
}

// tokenCommands 是 m1-04 的令牌与 MCP 接入命令（master-api-tokens、master-mcp）。签发、改权限、吊销属第 05 章七组：
// 人类专属，要当场验证，令牌与 MCP 一律拒绝。mcp status 是读命令，但在 mcp 分组下，MCP 解析器按首段拒绝。
func tokenCommands() []*Command {
	scopeFlags := []Flag{
		{Name: "preset", Type: TypeString, Description: "预设：readonly（只读）、ops（日常运维：可操作、危险类全关）、full（全权：可操作、六类危险全开）"},
		{Name: "danger", Type: TypeStrings, Description: "恰好打开这几个危险类（隐含可操作）：delete、restart、permission、batch、exec、master；要全关用 --preset ops"},
		{Name: "secrets", Type: TypeBool, Description: "密钥读取：输出里的打码字段给原文"},
		{Name: "expires-in", Type: TypeDuration, Description: "多久之后过期，如 720h；0 表示不过期"},
	}
	create := append([]Flag{
		{Name: "name", Type: TypeString, Description: "令牌的名字（1 到 64 个字符，可以重复）"},
	}, scopeFlags...)
	create = append(create, Flag{Name: "runtime", Type: TypeString, Description: "绑定的接入实例标签，如 claude-code@laptop（mcp init 会填）"})
	update := append([]Flag{
		{Name: "name", Type: TypeString, Description: "新名字（1 到 64 个字符）"},
	}, scopeFlags...)
	return []*Command{
		{Path: []string{"token", "create"}, Summary: "签发一把 API 令牌（默认只读、不过期；明文只在这次输出里出现一次）", Class: ClassAction, HumanOnly: true,
			Flags: create},
		{Path: []string{"token", "list"}, Summary: "列出 API 令牌（普通用户只看得到自己的）", Class: ClassRead, List: true,
			Flags:   []Flag{{Name: "owner", Type: TypeString, Description: "只看这个签发者的（管理员可用）"}},
			Columns: []string{"id", "name", "owner", "preset", "state", "runtime", "last_used_at", "expires_at"}},
		{Path: []string{"token", "update"}, Summary: "改一把令牌的名字、权限范围或过期时间（令牌字符串不变，改完立刻生效）", Class: ClassAction, HumanOnly: true,
			Args:  []Arg{{Name: "id", Description: "令牌 id（token list 里的 id）"}},
			Flags: update},
		{Path: []string{"token", "revoke"}, Summary: "吊销一把令牌（立刻失效，不能恢复）", Class: ClassAction, HumanOnly: true,
			Args: []Arg{{Name: "id", Description: "令牌 id（token list 里的 id）"}}},
		{Path: []string{"mcp", "status"}, Summary: "看谁在连：列出绑了 runtime 的令牌与它们的最后使用时间", Class: ClassRead, List: true,
			Columns: []string{"id", "name", "owner", "runtime", "preset", "last_used_at", "state"}},
	}
}

// settingsCommands 是 m1-03 的系统设置命令（master-settings）：读合并后的整个对象、写日常运维档、快照与回滚、
// 七组的主控地址。写命令都要带 --resource-version（整单一个版本，任何一档的写都比对并抬版本）。
func settingsCommands() []*Command {
	// 没有 --force：设置单例冲突了就重新读一遍再改（第 07 章）；force 归危险操作的权限类，随 M2 的 apply 一起做门。
	version := Flag{Name: "resource-version", Type: TypeInt, Description: "当前的 metadata.resourceVersion（settings show 里的值）；不匹配整单拒绝"}
	return []*Command{
		{Path: []string{"settings", "show"}, Summary: "显示系统设置：spec 是日常运维档，status 是人类专属、主控自身类、只读与运行态", Class: ClassRead},
		{Path: []string{"settings", "set"}, Summary: "改日常运维档的设置字段（列与 key 混着给，整体校验，同一个事务写入并存写前快照）", Class: ClassMasterSettings,
			Flags: []Flag{
				{Name: "set", Type: TypeObject, Kind: v1.Kind("SystemSettings"), Description: "要改的字段与值（字段名见 satchel explain SystemSettings 的 spec 字段）"},
				version,
			}},
		{Path: []string{"settings", "snapshots", "list"}, Summary: "按时间倒序列出系统设置的写前快照（不含内容）", Class: ClassRead, List: true,
			Columns: []string{"id", "object_version", "created_at", "source", "content_hash"}},
		{Path: []string{"settings", "rollback"}, Summary: "把一份快照的内容当成一次 settings set 写回（重过字段分档与规则，先存写前快照）", Class: ClassMasterSettings,
			Args:  []Arg{{Name: "snapshot", Description: "快照 id（settings snapshots list 里的 id）"}},
			Flags: []Flag{version}},
		{Path: []string{"settings", "master-url", "set"}, Summary: "改主控地址与订阅域名（人类专属：当场验证；干净的 HTTP(S) origin，空串清掉）", Class: ClassMasterSettings, HumanOnly: true,
			Flags: []Flag{
				{Name: "url", Type: TypeString, Description: "主控地址，如 https://panel.example.com"},
				{Name: "subscription-url", Type: TypeString, Description: "订阅域名，如 https://sub.example.com"},
				version,
			}},
		{Path: []string{"settings", "master-url", "migrate"}, Summary: "把主控地址迁到新域名或新 origin（人类专属：当场验证；--preview 并发让节点握手预检，换域名同步清空 Turnstile）",
			Class: ClassMasterSettings, HumanOnly: true,
			Flags: []Flag{
				{Name: "url", Type: TypeString, Description: "目标主控地址（必须是干净的 https:// origin，如 https://new.example.com）"},
				{Name: "subscription-url", Type: TypeString, Description: "可选：同步更新订阅域名"},
				{Name: "preview", Type: TypeBool, Description: "只读预检：并发让可达节点用主控公钥探测目标地址连通性，不改节点与数据库"},
				{Name: "change-domain", Type: TypeBool, Description: "显式标记本次切换域名并同步清空 Turnstile 验证密钥"},
				{Name: "skip-unreachable", Type: TypeBool, Description: "跳过当前离线节点（在线节点不可达仍阻断；被跳过的节点重连后会自动收敛）"},
				version,
			}},
		{Path: []string{"settings", "gates", "set"}, Summary: "改门这一组的设置：关闭公网访问、静默模式、登录限流与封禁的参数、Turnstile、反代登记（人类专属：当场验证；不存快照）", Class: ClassMasterSettings, HumanOnly: true,
			Flags: []Flag{
				{Name: "set", Type: TypeObject, Kind: v1.Kind("SystemSettings"), Description: "要改的字段与值，只收门这一组：master_local_only、silent_mode、silent_mode_timeout、probe_disguise_block_login、brute_force_enabled、brute_force_max_failures、brute_force_window_minutes、brute_force_block_minutes、login_rate_max_attempts、login_rate_window_minutes、login_rate_lock_minutes、skip_local_ip、turnstile_site_key、turnstile_secret_key、trusted_proxies"},
				version,
			}},
		// Turnstile 的测试配置（master-login-protection）：只核对、不改设置。flag 不叫 token（那是 CLI 的根 flag），
		// 用 siteverify 接口里这个值的名字 response。
		{Path: []string{"settings", "turnstile", "test"}, Summary: "用已保存的 secret 核对网页上 Turnstile 组件给出的 token（只核对，不改设置；只对管理员开放）", Class: ClassAction,
			Flags: []Flag{{Name: "response", Type: TypeString, Secret: true, Description: "Turnstile 组件拿到的一次性 token"}}},
		// 上传图片（master-settings「上传图片」）：只放文件、不改设置，写进字段照常用 settings set（有写前快照、能回滚）。
		{Path: []string{"settings", "image", "upload"}, Summary: "把一张图片放进 public/uploads/，返回它的地址（PNG、JPEG、GIF、WebP 或 ICO，最大 5 MiB；不改设置；只对管理员开放）",
			Class: ClassAction, Shape: ShapeUpload,
			Flags: []Flag{{Name: UploadFlag, Type: TypeFile, Description: "本地图片的路径"}}},
		{Path: []string{"settings", "batch-threshold", "set"}, Summary: "修改批量类危险门控阈值（batch_single_threshold / batch_window_threshold / batch_window_minutes；需 permission 危险类与 --confirm）",
			Class: ClassMasterSettings, Danger: v1.DangerPermission, Confirm: &Confirm{Kind: ConfirmObject, Arg: "key"},
			Args: []Arg{
				{Name: "key", Description: "设置项名：batch_single_threshold、batch_window_threshold 或 batch_window_minutes"},
				{Name: "value", Description: "新的正整数阈值"},
			},
			Flags: []Flag{version}},
	}
}

// localCommands 只在 CLI 进程里跑，不经主控（master-cli）。
// --data-dir 是 CLI 的全局 flag，不在这里登记（见 reservedFlags）。
func localCommands() []*Command {
	return []*Command{
		{Path: []string{"version"}, Summary: "打印版本号、commit 与构建时间", Class: ClassLocal},
		{Path: []string{"db", "migrate"}, Summary: "执行全部未应用的迁移，然后比对库结构与注册表", Class: ClassLocal},
		{Path: []string{"db", "status"}, Summary: "列出已应用与待应用的迁移，并比对库结构（不写盘）", Class: ClassLocal},
		{Path: []string{"db", "unlock"}, Summary: "清除上一次迁移被中断后残留的迁移锁", Class: ClassLocal},
		{Path: []string{"__verify"}, Summary: "用发布公钥验签（内部命令）", Class: ClassLocal, Hidden: true,
			Args: []Arg{{Name: "file", Description: "要验的文件"}, {Name: "sig", Description: "分离签名文件"}}},
		{Path: []string{"serve"}, Summary: "启动主控（只在 Linux 上）", Class: ClassLocal,
			Flags: []Flag{{Name: "config", Type: TypeString, Description: "配置文件路径（默认数据目录下的 config.yaml，或环境变量 SATCHEL_CONFIG）"}}},
		// 远程 CLI 的登录文件（master-cli「login 与 logout」）：login 用根 flag --server / --token，logout 只删文件。
		{Path: []string{"login"}, Summary: "验过一把令牌后，把主控地址与它存进登录文件，之后的命令默认连那个主控", Class: ClassLocal},
		{Path: []string{"logout"}, Summary: "删掉登录文件（令牌在服务端仍然有效，吊销用 token revoke）", Class: ClassLocal},
		// MCP 接入（master-mcp）：垫片与接入管理都在 CLI 进程里跑。mcp init 要替 token create 带当场验证，
		// 所以自己登记 --verify-user / --verify-code（本地命令不会被自动加这组保留 flag；密码仍只从终端读）。
		{Path: []string{"mcp", "stdio"}, Summary: "stdio 方式的 MCP 垫片：连上主控的 /mcp，把它的两个工具转给本地 runtime", Class: ClassLocal},
		{Path: []string{"mcp", "init"}, Summary: "把一个 AI runtime 接上主控：签一把令牌（或用已有的），写进它的 MCP 配置与环境变量，并装上 skills", Class: ClassLocal,
			Flags: []Flag{
				{Name: "runtime", Type: TypeString, Description: "要接入的 runtime：claude-code、codex 或 hermes"},
				{Name: "skills-only", Type: TypeBool, Description: "只把 skills 写进 runtime 的 skills 目录：不连主控、不签令牌、不改配置"},
				{Name: "url", Type: TypeString, Description: "写进 runtime 配置的主控地址（默认用 CLI 连的地址；在主控本机则按监听地址推出本机地址）"},
				{Name: "preset", Type: TypeString, Default: "ops", Description: "新令牌的预设：readonly、ops、full"},
				{Name: "name", Type: TypeString, Description: "新令牌的名字与 runtime 标签（默认 <runtime>@<主机名>）"},
				{Name: "use-token", Type: TypeBool, Description: "不签新令牌：从终端读一把已有的令牌"},
				{Name: "print", Type: TypeBool, Description: "只打印要加的配置与命令，不写任何文件"},
				{Name: VerifyUserFlag, Type: TypeString, Description: "签发时当场验证的管理员账号（本机管理员必填）"},
				{Name: VerifyCodeFlag, Type: TypeString, Description: "签发时当场验证的第二因素：验证器当前的码或一枚恢复码（不给会在终端里问）"},
			}},
		// 应急改密：直接开数据目录里的库，主控在不在跑都行；只对管理员账号；新密码从终端读两遍（master-accounts）。
		// 本地命令不许带危险类，--confirm 是它自己登记的普通 flag，处理函数自己比对。
		{Path: []string{"admin", "reset-password"}, Summary: "在主控本机重置一个管理员账号的密码（不经主控、不进审计）", Class: ClassLocal,
			Args:  []Arg{{Name: "username", Description: "要重置的管理员账号"}},
			Flags: []Flag{{Name: "confirm", Type: TypeString, Description: "填用户名确认"}}},
	}
}

// identityCommands 是 m1-02 的身份命令：初始化向导（不要身份）、自己的账号（人类专属的那几条要当场验证）。
func identityCommands() []*Command {
	return []*Command{
		{Path: []string{"setup", "status"}, Summary: "初始化向导的状态：库里有没有用户、哪几条起步路径可用", Class: ClassRead, Anonymous: true},
		{Path: []string{"setup", "init"}, Summary: "空库上建第一个管理员（已有用户一律拒绝）", Class: ClassAction, Anonymous: true,
			Flags: []Flag{
				{Name: "username", Type: TypeString, Description: "管理员用户名：3 到 32 个字符，小写字母、数字、_ 与 -，以字母或数字开头"},
				{Name: "password", Type: TypePassword, Description: "密码（至少 8 个字符；CLI 上从终端读两遍）"},
				{Name: "email", Type: TypeString, Description: "邮箱（可选）"},
			}},
		{Path: []string{"setup", "restore"}, Summary: "空库上用一份备份恢复整个主控（已有用户一律拒绝；主控随后重启）", Class: ClassAction,
			Anonymous: true, Shape: ShapeUpload, Flags: []Flag{{Name: UploadFlag, Type: TypeFile, Description: "本地备份文件的路径"}}},
		{Path: []string{"account", "show"}, Summary: "显示自己账号的状态：角色、两步验证、剩余恢复码、活动会话数", Class: ClassRead},
		{Path: []string{"account", "set-password"}, Summary: "改自己的密码（人类专属：当场验证；其它会话作废）", Class: ClassAction, HumanOnly: true,
			Flags: []Flag{{Name: "new-password", Type: TypePassword, Description: "新密码（至少 8 个字符；CLI 上从终端读两遍）"}}},
		{Path: []string{"account", "totp", "setup"}, Summary: "为自己的账号生成一把新的两步验证密钥（未启用，要用 confirm 启用）", Class: ClassAction, HumanOnly: true},
		{Path: []string{"account", "totp", "confirm"}, Summary: "用验证器算出的码启用两步验证，一次性返回 8 枚恢复码", Class: ClassAction, HumanOnly: true,
			Flags: []Flag{{Name: "code", Type: TypeString, Description: "验证器当前显示的 6 位码"}}},
		{Path: []string{"account", "totp", "disable"}, Summary: "关闭自己的两步验证，清掉密钥与恢复码", Class: ClassAction, HumanOnly: true},
		{Path: []string{"account", "recovery-codes", "regenerate"}, Summary: "作废全部旧恢复码，生成新的 8 枚", Class: ClassAction, HumanOnly: true},
	}
}

// baseCommands 是 m1-01 经主控的三条命令，把身份、权限、审计、REST、MCP 整条链跑通。
func baseCommands() []*Command {
	return []*Command{
		{Path: []string{"whoami"}, Summary: "显示当前调用者的身份、权限范围与危险类", Class: ClassRead},
		// 开局总览（master-overview）：四个分区随对应功能出现，M1 都是空的。
		{Path: []string{"overview"}, Summary: "开局总览：服务器、用户、告警、待办四个分区（各自随对应功能出现）", Class: ClassRead},
		{Path: []string{"audit", "list"}, Summary: "按时间倒序列出审计记录（只对管理员开放）", Class: ClassRead, List: true,
			Flags: []Flag{
				{Name: "actor", Type: TypeString, Description: "只看这个 actor（精确匹配）"},
				{Name: "command", Type: TypeString, Description: "只看这个命令（前缀匹配，如 audit）"},
				{Name: "since", Type: TypeString, Description: "只看不早于这个时间的记录（RFC 3339，如 2026-09-19T00:00:00Z）"},
			},
			Columns: []string{"id", "at", "actor", "actor_kind", "command", "result"}},
		{Path: []string{"explain"}, Summary: "解释一条命令或一个 kind：参数、类别、权限、字段与分档；不带参数列出全部", Class: ClassRead, Offline: true,
			Args: []Arg{{Name: "target", Description: "命令路径（如 \"audit list\"）或 kind 名（如 Task）", Optional: true}}},
	}
}
