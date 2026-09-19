package command

// GroupSummaries 是分组节点在帮助里的一句话说明；分组只做分组，本身不是命令。
var GroupSummaries = map[string]string{
	"db":    "数据库：执行迁移、查看迁移状态、清除迁移锁",
	"audit": "审计记录：谁在何时经主控做了什么",
}

// Catalog 是本仓库登记的全部命令。按功能域分文件时把各自的切片拼进来；顺序无关，Table 会排序。
func Catalog() *Table {
	return MustNew(catalogCommands()...)
}

func catalogCommands() []*Command {
	var all []*Command
	all = append(all, localCommands()...)
	all = append(all, baseCommands()...)
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
