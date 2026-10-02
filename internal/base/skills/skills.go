// Package skills 是基础设施层的 skills 手册（master-skills）：写给 AI 的操作手册随二进制分发，`satchel mcp init` 把它们装进
// runtime 的 skills 目录。源文件在 files/ 下，每个 skill 一个目录、目录里只有一个 SKILL.md，编进二进制。
// 这里提供四样东西：加载并做格式检查；从 skill 里抽出 satchel 命令行并按命令表检查；覆盖检查；生成「命令 × skills 对照表」。
// 加载与检查都接受一个 fs.FS，测试用临时的一份 skills。
package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

//go:embed files
var embedded embed.FS

// Files 是编进二进制的 skills，根目录下就是各个 skill 目录。
func Files() fs.FS {
	sub, err := fs.Sub(embedded, "files")
	if err != nil {
		panic(err) // files 是编译期就在的目录
	}
	return sub
}

// FileName 是每个 skill 目录里唯一的文件。
const FileName = "SKILL.md"

// 格式上限（master-skills「skills 的存放与格式」）：名字不超过 32 个字符（Hermes 在 Telegram、Discord 上的斜杠命令名上限），
// description 按 Unicode 字符数不超过 1024（Agent Skills 规范），全文不超过 500 行。
const (
	maxNameLen  = 32
	maxDescLen  = 1024
	maxFileLine = 500
)

// nameRe 是 skill 名：satchel- 开头，由小写字母、数字与单个连字符分隔的词组成。
var nameRe = regexp.MustCompile(`^satchel(-[a-z0-9]+)+$`)

// Skill 是一份 skill：目录名、description 与 SKILL.md 全文。
type Skill struct {
	Name        string
	Description string
	Content     string
}

// Problem 是检查出的一处问题：哪个 skill、第几行（0 表示整份）、那一处原文与原因。
type Problem struct {
	Skill  string
	Line   int
	Text   string
	Reason string
}

func (p Problem) String() string {
	where := p.Skill
	if p.Line > 0 {
		where = fmt.Sprintf("%s 第 %d 行", p.Skill, p.Line)
	}
	if p.Text != "" {
		return fmt.Sprintf("%s：%s（%s）", where, p.Reason, p.Text)
	}
	return fmt.Sprintf("%s：%s", where, p.Reason)
}

// Load 读 fsys 下的全部 skills（按名字排序）并做格式检查；格式不合的 skill 不进返回的清单，问题点名它与原因。
func Load(fsys fs.FS) ([]Skill, []Problem, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, nil, err
	}
	var skills []Skill
	var problems []Problem
	for _, e := range entries {
		if !e.IsDir() {
			problems = append(problems, Problem{Skill: e.Name(), Reason: "skills 的根目录下只放 skill 目录"})
			continue
		}
		s, ps := loadOne(fsys, e.Name())
		problems = append(problems, ps...)
		if len(ps) == 0 {
			skills = append(skills, s)
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, problems, nil
}

func loadOne(fsys fs.FS, name string) (Skill, []Problem) {
	var problems []Problem
	bad := func(line int, reason string) {
		problems = append(problems, Problem{Skill: name, Line: line, Reason: reason})
	}
	if !nameRe.MatchString(name) {
		bad(0, "名字要以 satchel- 开头，由小写字母、数字与单个连字符分隔的词组成")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		bad(0, fmt.Sprintf("名字超过 %d 个字符", maxNameLen))
	}
	entries, err := fs.ReadDir(fsys, name)
	if err != nil {
		bad(0, "读不了这个目录："+err.Error())
		return Skill{}, problems
	}
	for _, e := range entries {
		if e.Name() != FileName {
			bad(0, "目录里只能有 "+FileName+"，多了 "+e.Name())
		}
	}
	raw, err := fs.ReadFile(fsys, name+"/"+FileName)
	if err != nil {
		bad(0, "读不到 "+FileName)
		return Skill{}, problems
	}
	desc, ps := checkFrontmatter(name, raw)
	problems = append(problems, ps...)
	lines := bytes.Count(raw, []byte("\n"))
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		lines++ // 最后一行没有换行符
	}
	if lines > maxFileLine {
		bad(0, fmt.Sprintf("全文超过 %d 行", maxFileLine))
	}
	return Skill{Name: name, Description: desc, Content: string(raw)}, problems
}

// checkFrontmatter 检查 SKILL.md 的开头：第一行就是 ---；frontmatter 只有 name 与 description 两行，name 等于目录名，
// description 一行、双引号括起、非空、不超过 1024 个字符、不含 < 与 >；任何值里都没有 ---。返回 description。
func checkFrontmatter(name string, raw []byte) (string, []Problem) {
	var problems []Problem
	bad := func(line int, reason string) {
		problems = append(problems, Problem{Skill: name, Line: line, Reason: reason})
	}
	lines := strings.Split(string(raw), "\n")
	if lines[0] != "---" {
		bad(1, "第一行要是 ---（前面不能有 BOM 或空行）")
		return "", problems
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		bad(1, "frontmatter 没有用 --- 结束")
		return "", problems
	}
	seen := map[string]bool{}
	for i := 1; i < end; i++ {
		key, value, ok := strings.Cut(lines[i], ": ")
		if !ok || (key != "name" && key != "description") {
			bad(i+1, "frontmatter 只能有 name 与 description 两个字段，每个写成一行「字段: 值」")
			continue
		}
		if seen[key] {
			bad(i+1, key+" 写了两次")
		}
		seen[key] = true
		if strings.Contains(value, "---") {
			bad(i+1, "frontmatter 的值里不能有 ---（Claude Code 遇到第一个 --- 就结束 frontmatter）")
		}
		if key == "description" && (len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"') {
			bad(i+1, "description 要写成一行、用双引号括起来")
		}
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fm); err != nil {
		bad(2, "frontmatter 不是合法的 YAML："+err.Error())
		return "", problems
	}
	switch {
	case !seen["name"]:
		bad(2, "缺 name")
	case fm.Name != name:
		bad(2, fmt.Sprintf("name 要等于目录名 %s，得到 %q", name, fm.Name))
	}
	desc := fm.Description
	switch {
	case !seen["description"] || strings.TrimSpace(desc) == "":
		bad(2, "缺 description")
	case utf8.RuneCountInString(desc) > maxDescLen:
		bad(2, fmt.Sprintf("description 超过 %d 个字符", maxDescLen))
	case strings.ContainsAny(desc, "<>"):
		bad(2, "description 里不能有 < 或 >")
	}
	return desc, problems
}
