package command

import v1 "github.com/satchel/satchel/pkg/api/v1"

// GroupSummaries 是分组节点在帮助里的一句话说明；分组只做分组，本身不是命令。
var GroupSummaries = map[string]string{
	"db":                     "数据库：执行迁移、查看迁移状态、清除迁移锁",
	"audit":                  "审计记录：谁在何时经主控做了什么",
	"setup":                  "初始化向导：第一次打开主控时建第一个管理员",
	"account":                "自己的账号：状态、密码、两步验证",
	"account totp":           "两步验证（TOTP）：启用、确认、禁用",
	"account recovery-codes": "两步验证的恢复码",
	"admin":                  "主控本机的应急操作",
	"settings":               "系统设置：一个单例对象，整单一个 resourceVersion（主控设置类）",
	"settings snapshots":     "系统设置的写前快照",
	"settings master-url":    "主控地址与订阅域名（七组人类专属：改的时候当场验证）",
	"settings gates":         "门：关闭公网访问、静默模式、登录限流与封禁的参数、Turnstile、反代登记（七组人类专属：改的时候当场验证）",
	"security":               "安全：安全事件与 IP 封禁（封禁与解封要当场验证）",
	"token":                  "API 令牌：签发、列出、改权限、吊销（签发、改权限、吊销要当场验证）",
	"mcp":                    "MCP 接入：stdio 垫片、把 AI runtime 接上主控、看谁在连",
	"logs":                   "主控的系统日志（只对管理员开放）",
	"logs files":             "数据目录 logs/ 下的日志文件",
	"schedule":               "内置定时任务与它们的运行记录（只对管理员开放）",
	"schedule runs":          "内置定时任务每次运行的记录",
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
	return all
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
		{Path: []string{"settings", "gates", "set"}, Summary: "改门这一组的设置：关闭公网访问、静默模式、登录限流与封禁的参数、Turnstile、反代登记（人类专属：当场验证；不存快照）", Class: ClassMasterSettings, HumanOnly: true,
			Flags: []Flag{
				{Name: "set", Type: TypeObject, Kind: v1.Kind("SystemSettings"), Description: "要改的字段与值，只收门这一组：master_local_only、silent_mode、silent_mode_timeout、probe_disguise_block_login、brute_force_enabled、brute_force_max_failures、brute_force_window_minutes、brute_force_block_minutes、login_rate_max_attempts、login_rate_window_minutes、login_rate_lock_minutes、skip_local_ip、turnstile_site_key、turnstile_secret_key、trusted_proxies"},
				version,
			}},
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
		{Path: []string{"mcp", "init"}, Summary: "把一个 AI runtime 接上主控：签一把令牌（或用已有的），写进它的 MCP 配置与环境变量", Class: ClassLocal,
			Flags: []Flag{
				{Name: "runtime", Type: TypeString, Description: "要接入的 runtime：claude-code、codex 或 hermes"},
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
