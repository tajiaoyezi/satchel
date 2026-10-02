package skills

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/satchel/satchel/internal/command"
)

// DocsPath 是「命令 × skills 对照表」在仓库里的位置（相对模块根）。
const DocsPath = "docs/skills.md"

// Generate 加载 fsys 里的 skills，跑格式、命令行与覆盖检查；全部通过时生成「命令 × skills 对照表」
// （master-skills「命令 × skills 对照表由命令表与 skills 生成」），否则返回列出全部问题的错误，不生成一份不完整的表。
func Generate(t *command.Table, fsys fs.FS) ([]byte, error) {
	skills, problems, err := Load(fsys)
	if err != nil {
		return nil, err
	}
	refs, cmdProblems := CheckCommands(t, skills)
	problems = append(problems, cmdProblems...)
	var lines []string
	for _, p := range problems {
		lines = append(lines, p.String())
	}
	for _, name := range Coverage(t, refs) {
		lines = append(lines, "命令 "+name+" 没有被任何 skill 讲到")
	}
	if len(lines) > 0 {
		return nil, errors.New("skills 检查不通过：\n" + strings.Join(lines, "\n"))
	}
	return renderDoc(t, skills, refs), nil
}

// renderDoc 生成对照表：先列每个 skill 的名字与 description，再按命令路径每条非隐藏命令一行，列出引用它的 skill。
// 输出只取决于命令表与 skills，排序稳定。
func renderDoc(t *command.Table, skills []Skill, refs []Ref) []byte {
	cell := func(s string) string { return strings.ReplaceAll(s, "|", "\\|") }
	by := map[string]map[string]bool{}
	for _, r := range refs {
		if by[r.Command] == nil {
			by[r.Command] = map[string]bool{}
		}
		by[r.Command][r.Skill] = true
	}
	var b bytes.Buffer
	b.WriteString("<!-- 由 go generate ./internal/command/ 从命令表与 internal/base/skills/files/ 生成，不要手改。 -->\n")
	b.WriteString("# 命令 × skills 对照表\n\n")
	b.WriteString("skills 是写给 AI 的操作手册：源文件在 `internal/base/skills/files/`，编进 satchel 二进制，`satchel mcp init` 把它们装进 runtime 的 skills 目录。")
	b.WriteString("命令表里每一条非隐藏命令都要被至少一个 skill 讲到；skills 里写的每一条 satchel 命令行都要在这个二进制的 CLI 上解析得了。")
	b.WriteString("这两条由 `go test ./internal/base/skills/` 与 CI 守着（master-skills）。\n\n")
	b.WriteString("## skills\n\n| skill | 什么时候用 |\n|---|---|\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "| `%s` | %s |\n", s.Name, cell(s.Description))
	}
	b.WriteString("\n## 命令\n\n| 命令 | 讲到它的 skill |\n|---|---|\n")
	for _, c := range t.All() {
		if c.Hidden {
			continue
		}
		var names []string
		for name := range by[c.Name()] {
			names = append(names, "`"+name+"`")
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "| `%s` | %s |\n", c.Name(), strings.Join(names, "、"))
	}
	return b.Bytes()
}
