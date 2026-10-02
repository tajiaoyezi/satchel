package skills

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/satchel/satchel/internal/command"
)

// skills 里 satchel 命令行的检查（master-skills「skills 引用的命令必须存在」）。只认几种简单写法，超出的直接判失败、
// 提示改写，不去猜它的意思（design 第 4 条）。

// Ref 是 skills 里一处 satchel 命令行解析出的命令（覆盖检查与对照表用）。
type Ref struct {
	Skill   string
	Line    int
	Command string
}

// rewriteHint 是写法超出范围时附在原因后面的提示。
const rewriteHint = "改成简单写法：管道、可选部分、注释与泛指的说法写在正文文字里，代码里写具体的一条命令"

// specialChars 是不在引号里就判失败的字符：管道、命令分隔、后台、重定向、注释、命令替换、转义、方括号、括号、glob、历史展开。
const specialChars = "|;&>#`\\[](){}*?!"

// assignRe 是命令前面的环境变量赋值 名字=值。
var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// segment 是一段要检查的代码：行内代码的内容，或代码块里的一行。
type segment struct {
	line int
	text string
}

// CheckCommands 抽出每份 skill 里的 satchel 命令行，按命令表检查；返回解析出的命令引用与问题。
func CheckCommands(t *command.Table, skills []Skill) ([]Ref, []Problem) {
	ck := newChecker(t)
	var refs []Ref
	var problems []Problem
	for _, s := range skills {
		segs, ps := segments(s)
		problems = append(problems, ps...)
		for _, seg := range segs {
			if bad := otherSpace(seg.text); bad != "" {
				problems = append(problems, Problem{Skill: s.Name, Line: seg.line, Text: seg.text,
					Reason: "代码里只认半角空格与制表符作空白，这里有 " + bad})
				continue
			}
			text, words, syntax, ok := commandLine(seg.text)
			if !ok {
				continue
			}
			if len(syntax) > 0 {
				problems = append(problems, Problem{Skill: s.Name, Line: seg.line, Text: text,
					Reason: strings.Join(syntax, "；") + "。" + rewriteHint})
				continue
			}
			name, reasons := ck.parse(words)
			for _, r := range reasons {
				problems = append(problems, Problem{Skill: s.Name, Line: seg.line, Text: text, Reason: r})
			}
			if name != "" {
				refs = append(refs, Ref{Skill: s.Name, Line: seg.line, Command: name})
			}
		}
	}
	return refs, problems
}

// segments 按规则 1 从 SKILL.md 的正文里取出代码：``` 围栏代码块里的每一行（一行去掉行首空白后以 ``` 开头就开始或结束代码块，
// 列表里缩进的也算），与代码块之外每一段行内代码（不跨行）。~~~ 围栏、一行里配不成对的反引号、没有闭合的代码块记为问题。
func segments(s Skill) ([]segment, []Problem) {
	var segs []segment
	var problems []Problem
	lines := strings.Split(s.Content, "\n")
	start := 0
	if len(lines) > 0 && lines[0] == "---" { // 跳过 frontmatter（格式检查已经保证它存在且闭合）
		for i := 1; i < len(lines); i++ {
			if lines[i] == "---" {
				start = i + 1
				break
			}
		}
	}
	fenceLine := 0
	for i := start; i < len(lines); i++ {
		line, n := lines[i], i+1
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "```") {
			if fenceLine == 0 {
				fenceLine = n
			} else {
				fenceLine = 0
			}
			continue
		}
		if fenceLine != 0 {
			segs = append(segs, segment{line: n, text: line})
			continue
		}
		if strings.HasPrefix(trimmed, "~~~") {
			problems = append(problems, Problem{Skill: s.Name, Line: n, Text: line, Reason: "代码块只用 ``` 围栏，不用 ~~~"})
			continue
		}
		spans, ok := inlineCode(line)
		if !ok {
			problems = append(problems, Problem{Skill: s.Name, Line: n, Text: line,
				Reason: "这一行的反引号配不成对：行内代码要写在一行里，补上反引号"})
		}
		for _, sp := range spans {
			segs = append(segs, segment{line: n, text: sp})
		}
	}
	if fenceLine != 0 {
		problems = append(problems, Problem{Skill: s.Name, Line: fenceLine, Reason: "代码块没有用 ``` 闭合"})
	}
	return segs, problems
}

// inlineCode 取出一行里的行内代码：N 个反引号开头、到下一处恰好 N 个反引号为止；内容两头都有空格时各去掉一个（CommonMark）。
// 有反引号配不上对时 ok 为 false。
func inlineCode(line string) (spans []string, ok bool) {
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		n, end := j-i, -1
		for k := j; k < len(line); {
			if line[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-k == n {
				end = k
				break
			}
			k = m
		}
		if end < 0 {
			return spans, false
		}
		content := line[j:end]
		if len(content) >= 2 && content[0] == ' ' && content[len(content)-1] == ' ' && strings.TrimSpace(content) != "" {
			content = content[1 : len(content)-1]
		}
		spans = append(spans, content)
		i = end + n
	}
	return spans, true
}

// otherSpace 返回代码里半角空格与制表符以外的第一个空白字符的说明；没有返回空串。
func otherSpace(text string) string {
	for _, r := range text {
		if unicode.IsSpace(r) && r != ' ' && r != '\t' {
			return fmt.Sprintf("U+%04X", r)
		}
	}
	return ""
}

// commandLine 按规则 2 判断一段代码是不是 satchel 命令行：去掉开头的 "$ " 之后，依次可以有 名字=值、sudo 或 sudo -u <用户>、
// docker compose exec <服务>，再接 satchel 并且后面还有词。是的话按规则 3 切词，返回去掉提示符的原文、satchel 之后的词，
// 以及写法上的问题。
func commandLine(code string) (text string, words, syntax []string, ok bool) {
	text = strings.TrimPrefix(strings.TrimLeft(code, " \t"), "$ ")
	all, syntax := tokenize(text)
	at := satchelAt(all)
	if at < 0 {
		return "", nil, nil, false
	}
	if strings.Contains(text, "$") {
		syntax = append(syntax, "命令行里不能有 $（开头的提示符除外）")
	}
	if strings.Contains(text, "…") || strings.Contains(text, "...") {
		syntax = append(syntax, "命令行里不能有省略号")
	}
	return text, all[at+1:], syntax, true
}

// satchelAt 找出 satchel 这个词的位置（前面只能是规则 2 认的前缀），后面没有词或认不出时返回 -1。
func satchelAt(words []string) int {
	i := 0
	for i < len(words) && assignRe.MatchString(words[i]) {
		i++
	}
	if i < len(words) && words[i] == "sudo" {
		i++
		if i+1 < len(words) && words[i] == "-u" {
			i += 2
		}
	}
	if i+3 < len(words) && words[i] == "docker" && words[i+1] == "compose" && words[i+2] == "exec" {
		i += 4
	}
	if i < len(words)-1 && words[i] == "satchel" {
		return i
	}
	return -1
}

// tokenize 按规则 3 切词：只认半角空格与制表符作分隔；单引号、双引号括起的部分算在一个词里；<…>（中间没有空白，也没有别的 <）
// 是占位符，单独成词或在词里都行，里面同样不能有特殊字符与引号。不在引号里的特殊字符、不配对的 < 或 >、不成对的引号，记为写法上的问题（同一种只记一次）。
func tokenize(s string) (words, syntax []string) {
	seen := map[string]bool{}
	bad := func(reason string) {
		if !seen[reason] {
			seen[reason] = true
			syntax = append(syntax, reason)
		}
	}
	var cur strings.Builder
	inWord := false
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		switch {
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '<':
			j := i + 1
			for j < len(rs) && rs[j] != '>' && rs[j] != '<' && !unicode.IsSpace(rs[j]) {
				j++
			}
			if j > i+1 && j < len(rs) && rs[j] == '>' {
				for _, c := range rs[i+1 : j] {
					switch {
					case strings.ContainsRune(specialChars, c):
						bad(fmt.Sprintf("占位符里的 %c（可选的取值写在正文文字里）", c))
					case c == '\'' || c == '"':
						bad("占位符里不能有引号")
					}
				}
				cur.WriteString(string(rs[i : j+1]))
				i = j
			} else {
				bad("不在引号里的 < 没有配对的 >（占位符写成 <名字>，中间不能有空白）")
				cur.WriteRune(r)
			}
			inWord = true
		case strings.ContainsRune(specialChars, r):
			bad(fmt.Sprintf("不在引号里的 %c", r))
			cur.WriteRune(r)
			inWord = true
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		bad("引号不成对")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, syntax
}

// checker 带着命令表推出的信息：分组路径（各命令路径的真前缀）与根 flag。
type checker struct {
	table  *command.Table
	groups map[string]bool
	root   map[string]command.FlagType
}

func newChecker(t *command.Table) *checker {
	ck := &checker{table: t, groups: map[string]bool{}, root: map[string]command.FlagType{"help": command.TypeBool}}
	for _, c := range t.All() {
		for i := 1; i < len(c.Path); i++ {
			ck.groups[strings.Join(c.Path[:i], " ")] = true
		}
	}
	for _, f := range command.RootFlags {
		ck.root[f.Name] = f.Type
	}
	return ck
}

// flagsOf 是一条命令在 CLI 上接受的 flag（command.CLIFlags 加上 cobra 自带的 help）。
func flagsOf(c *command.Command) map[string]command.FlagType {
	out := map[string]command.FlagType{"help": command.TypeBool}
	for _, f := range command.CLIFlags(c) {
		out[f.Name] = f.Type
	}
	return out
}

// parse 按规则 4 到 8 检查 satchel 之后的词，返回解析出的命令名（解析不出为空）与问题。
func (ck *checker) parse(args []string) (string, []string) {
	var problems []string
	// 命令路径是 satchel 之后连续的词；空串与带空白的词（引号括起的几个词）不是路径段。
	lead := 0
	for lead < len(args) && args[lead] != "" && !strings.HasPrefix(args[lead], "-") && !strings.ContainsAny(args[lead], " \t") {
		lead++
	}
	var cmd *command.Command
	pathLen := 0
	for j := lead; j >= 1; j-- {
		if c, ok := ck.table.Lookup(strings.Join(args[:j], " ")); ok {
			cmd, pathLen = c, j
			break
		}
	}
	allowed := ck.root
	if cmd != nil {
		allowed = flagsOf(cmd)
	}
	var positional []string
	help, dashdash, flagBad := false, false, false
	for i := pathLen; i < len(args); i++ {
		a := args[i]
		switch {
		case dashdash:
			positional = append(positional, a)
		case a == "--":
			dashdash = true
		case a == "--help" || a == "-h":
			help = true
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a[2:], "=")
			typ, ok := allowed[name]
			if !ok {
				problems = append(problems, ck.unknownFlag(cmd, name))
				flagBad = true
				continue
			}
			if typ != command.TypeBool && !hasValue {
				if i+1 >= len(args) {
					problems = append(problems, "--"+name+" 缺少值（CLI 会报缺少值）")
					continue
				}
				i++
			}
		case strings.HasPrefix(a, "-"):
			problems = append(problems, "单个 - 开头的写法只认 -h："+a)
			flagBad = true
		default:
			positional = append(positional, a)
		}
	}
	if cmd == nil {
		path := args[:lead]
		if !help {
			return "", append(problems, ck.noCommand(path))
		}
		// 帮助行落在分组或没有命令路径上：除了分组的路径、根 flag 与它们的值，不能再有别的词。
		if (len(path) > 0 && !ck.groups[strings.Join(path, " ")]) || len(positional) != len(path) {
			problems = append(problems, "带 --help 时命令路径的位置上要是命令或分组，不能是拼错的子命令或占位符："+ck.noCommand(path))
		}
		return "", problems
	}
	// 有写错的 flag 时不知道它要不要带值，后面的词算不准，不再报位置参数的个数。
	if !help && !flagBad && len(positional) > len(cmd.Args) {
		problems = append(problems, fmt.Sprintf("%s 最多 %d 个位置参数，这里有 %d 个：%s", cmd.Name(), len(cmd.Args), len(positional), strings.Join(positional, " ")))
	}
	if cmd.Name() == "explain" && !help && len(positional) == 1 && !strings.Contains(positional[0], "<") {
		target := positional[0]
		if _, err := command.Explain(ck.table, target); err != nil {
			problems = append(problems, fmt.Sprintf("explain 的参数 %q 既不是命令也不是 kind", target))
		} else if c, ok := ck.table.Lookup(target); ok && c.Hidden {
			problems = append(problems, fmt.Sprintf("explain 的参数 %q 是隐藏命令", target))
		}
	}
	return cmd.Name(), problems
}

// unknownFlag 说明一个 CLI 上不存在的 flag；密码类与 --server、--token 给出具体原因。
func (ck *checker) unknownFlag(cmd *command.Command, name string) string {
	if cmd == nil {
		return "--" + name + " 不是根 flag（帮助行只认根 flag）"
	}
	if name == command.VerifyPasswordFlag {
		return "--" + name + " 在 CLI 上不存在：密码只从终端读，不能写在命令行上"
	}
	for _, f := range cmd.Flags {
		if f.Name == name && f.Type == command.TypePassword {
			return "--" + name + " 在 CLI 上不存在：密码只从终端读，不能写在命令行上"
		}
	}
	if (name == "server" || name == "token") && cmd.Class == command.ClassLocal {
		return "--" + name + " 只对经主控的命令与 " + strings.Join(command.ConnectingLocal, "、") + " 有效，" + cmd.Name() + " 不接受"
	}
	return "--" + name + " 不是 " + cmd.Name() + " 在 CLI 上的 flag"
}

// noCommand 说明一串词为什么不是命令：拼错的子命令、只写到分组、或者 satchel 后面没有紧跟命令路径。
func (ck *checker) noCommand(path []string) string {
	if len(path) == 0 {
		return "satchel 后面要紧跟命令路径（根 flag 写在命令路径之后）"
	}
	g := 0
	for j := len(path); j >= 1; j-- {
		if ck.groups[strings.Join(path[:j], " ")] {
			g = j
			break
		}
	}
	switch {
	case g == len(path):
		return strings.Join(path, " ") + " 是分组，要写到具体的子命令"
	case g == 0:
		return "没有子命令 " + path[0]
	}
	return strings.Join(path[:g], " ") + " 下没有 " + path[g]
}

// Coverage 返回命令表里没被任何 skill 引用的非隐藏命令（按命令路径排序）。
func Coverage(t *command.Table, refs []Ref) []string {
	used := map[string]bool{}
	for _, r := range refs {
		used[r.Command] = true
	}
	var missing []string
	for _, c := range t.All() {
		if !c.Hidden && !used[c.Name()] {
			missing = append(missing, c.Name())
		}
	}
	sort.Strings(missing)
	return missing
}
