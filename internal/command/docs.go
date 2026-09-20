package command

import (
	"bytes"
	"fmt"
	"strings"
)

// DocsPath 是「命令 × scope 对照表」在仓库里的位置（相对模块根）。
const DocsPath = "docs/commands.md"

// GenerateDocs 生成「命令 × scope 对照表」（第 05 章随 M1 交付的那张表）：非本地命令一段、本地命令一段。
// 输出只取决于表，排序稳定；TestCommandsDocUpToDate 与 CI 的 git diff 守着它与表一致。
func GenerateDocs(t *Table) []byte {
	var b bytes.Buffer
	b.WriteString("<!-- 由 go generate ./internal/command/ 从命令表生成，不要手改。 -->\n")
	b.WriteString("# 命令 × scope 对照表\n\n")
	b.WriteString("每条命令所需的权限范围（scope）、危险类与 confirm 口径、是否人类专属、是否不要身份，以及 REST 映射，都来自 `internal/command` 的命令表；")
	b.WriteString("CLI、REST、MCP 三个投影从同一张表构造。危险类命令须带字符串 `confirm`（object 填对象名、count 填本次数量）；")
	b.WriteString("人类专属命令对任何令牌拒绝、MCP 上不可用，请求里要带当场验证的 `verify-password` / `verify-code` / `verify-user`；")
	b.WriteString("不要身份的命令只有初始化向导的两条，MCP 上同样不可用。\n\n")
	b.WriteString("## 经主控的命令\n\n")
	b.WriteString("| 命令 | 类别 | scope | 危险类 | confirm | 人类专属（当场验证） | 不要身份 | 列表 | REST |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range t.Remote() {
		info := Describe(c)
		confirm := ""
		if c.Confirm != nil {
			confirm = string(c.Confirm.Kind)
			if c.Confirm.Kind == ConfirmObject {
				confirm += " (" + c.Confirm.Arg + ")"
			}
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s | %s | `%s %s` |\n",
			c.Name(), c.Class, info.Scope, dash(string(c.Danger)), dash(confirm), yesNo(c.HumanOnly), yesNo(c.Anonymous), yesNo(c.List), info.REST.Method, info.REST.Path)
	}
	b.WriteString("\n## 本地命令（不经主控，只在 CLI 里）\n\n")
	b.WriteString("| 命令 | 说明 |\n|---|---|\n")
	for _, c := range t.Local() {
		name := "`" + c.Name() + "`"
		if c.Hidden {
			name += "（隐藏）"
		}
		fmt.Fprintf(&b, "| %s | %s |\n", name, strings.ReplaceAll(c.Summary, "|", "\\|"))
	}
	return b.Bytes()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
