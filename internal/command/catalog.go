package command

// GroupSummaries 是分组节点在帮助里的一句话说明；分组只做分组，本身不是命令。
var GroupSummaries = map[string]string{
	"db":                     "数据库：执行迁移、查看迁移状态、清除迁移锁",
	"audit":                  "审计记录：谁在何时经主控做了什么",
	"setup":                  "初始化向导：第一次打开主控时建第一个管理员",
	"account":                "自己的账号：状态、密码、两步验证",
	"account totp":           "两步验证（TOTP）：启用、确认、禁用",
	"account recovery-codes": "两步验证的恢复码",
	"admin":                  "主控本机的应急操作",
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
	return all
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
