package cli

import (
	"fmt"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Codex（design 第 14 条，notes/runtime-configs.md A1、A3）：$CODEX_HOME/config.toml 的 [mcp_servers.satchel] 里设 url 与
// http_headers.Authorization（没有这张表就追加一块），块里其余的键原样保留——用户的 enabled、disabled_tools、超时与 tools 子表都不动；
// 三个变量写进 [shell_environment_policy] 的 set（set 在排除规则之后执行，能把被排除的变量加回来）。
// 按行定位表头，改完用 BurntSushi/toml 重新解析做整树比对。

const codexVerify = "codex mcp get satchel --json"

func codexNotes() []string {
	return []string{
		"重启 Codex 后生效",
		"Codex 0.76 之前的版本默认会滤掉名字含 TOKEN 的变量，这里写的 shell_environment_policy.set 不受影响",
		"默认的 workspace-write 沙箱不许出网：shell 里的 satchel 可能要你批准联网；MCP 工具走 Codex 自己的连接，不受沙箱影响",
		"Codex 把受信任（trusted）项目里的 .codex/config.toml 与这里按键合并：项目层只写一个 [mcp_servers.satchel] 的 url，令牌就会随 http_headers 发往那个地址。只把你信任的仓库标为 trusted；仓库里自己配了 mcp_servers.satchel 时先看清它指向哪里",
		"skills 随 m1-09 交付",
	}
}

func codexConfigPath(env runtimeEnv) string { return filepath.Join(env.codexHome, "config.toml") }

func codexURLLine(url string) string { return "url = " + tomlString(url+"/mcp") }

func codexAuth(token string) [][2]string { return [][2]string{{"Authorization", "Bearer " + token}} }

func codexServerBody(url, token string) []string {
	return []string{codexURLLine(url), inlineTable("http_headers", codexAuth(token))}
}

func snippetCodex(env runtimeEnv, url, token string) string {
	lines := []string{fmt.Sprintf("# 加进 %s（已有 [shell_environment_policy] 就把 set 里的三个变量并进去）：", codexConfigPath(env)), "[mcp_servers.satchel]"}
	lines = append(lines, codexServerBody(url, token)...)
	lines = append(lines, "", "[shell_environment_policy]", inlineTable("set", satchelEnv(url, token)))
	return strings.Join(lines, "\n")
}

func planCodex(env runtimeEnv, url, token string) (*initPlan, error) {
	path := codexConfigPath(env)
	before, err := readOptional(path)
	if err != nil {
		return nil, err
	}
	var tree map[string]any
	if _, err := toml.Decode(string(before), &tree); err != nil {
		return nil, unsupportedf(path, "不是合法的 TOML（%v）", err)
	}
	if err := checkCodexIncludes(path, tree); err != nil {
		return nil, err
	}
	lines := splitLines(before)
	if lines, err = setCodexServer(path, lines, tree, url, token); err != nil {
		return nil, err
	}
	if lines, err = setCodexPolicy(path, lines, tree, satchelEnv(url, token)); err != nil {
		return nil, err
	}
	after := joinLines(lines)
	if err := checkCodexTree(path, before, after, url, token); err != nil {
		return nil, err
	}
	plan := &initPlan{edits: []fileEdit{{path: path, before: before, after: after, secret: true}}, verify: codexVerify, notes: codexNotes()}
	srv := tomlTable(tree, "mcp_servers", "satchel")
	if prev, _ := tomlTable(tree, "shell_environment_policy", "set")[EnvToken].(string); prev != "" && prev != token {
		plan.notes = append(plan.notes, replacedTokenNote(path))
	} else if prev, _ := tomlTable(srv, "http_headers")["Authorization"].(string); prev != "" && prev != "Bearer "+token {
		plan.notes = append(plan.notes, replacedTokenNote(path))
	}
	if enabled, ok := srv["enabled"].(bool); ok && !enabled {
		plan.notes = append(plan.notes, "[mcp_servers.satchel] 里 enabled = false，mcp init 没有改它，接入后仍是停用的；要启用就改成 true")
	}
	return plan, nil
}

// checkCodexIncludes：用户配了 include 过滤（include_only 或 filters 里的 include）且不匹配 SATCHEL_* 时，
// Codex 在 set 之后才过滤，注入的变量会被删掉；这时停下，打印要加的 include，不去改用户的过滤规则。
func checkCodexIncludes(file string, tree map[string]any) error {
	policy := tomlTable(tree, "shell_environment_policy")
	var patterns []string
	hint := `[shell_environment_policy] 的 include_only 里加上 "SATCHEL_*"`
	if list, ok := policy["include_only"].([]any); ok {
		for _, p := range list {
			if s, ok := p.(string); ok {
				patterns = append(patterns, s)
			}
		}
	}
	for p, mode := range tomlTable(policy, "filters") {
		if mode == "include" {
			patterns = append(patterns, p)
			hint = `[shell_environment_policy.filters] 里加上 "SATCHEL_*" = "include"`
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	for _, kv := range satchelEnv("", "") {
		matched := false
		for _, p := range patterns {
			if ok, err := path.Match(strings.ToUpper(p), kv[0]); err == nil && ok {
				matched = true
			}
		}
		if !matched {
			return unsupportedf(file, "shell_environment_policy 的 include 过滤（%s）不匹配 %s，Codex 在 set 之后才按它过滤，注入的变量会被删掉；请先在 %s",
				strings.Join(patterns, "、"), kv[0], hint)
		}
	}
	return nil
}

// setCodexServer 在 [mcp_servers.satchel] 里设 url 与 http_headers.Authorization，块里其余的键原样保留：url 行就地换掉，
// http_headers 是单行内联表就并进 Authorization、是 [mcp_servers.satchel.http_headers] 子表就逐键替换或追加，缺的行插在表头下。
// 没有这张表就在文件末尾追加一块。块是 stdio 写法（有 command）或配了 bearer_token_env_var（Codex 会用它取令牌）时停下。
func setCodexServer(path string, lines []string, tree map[string]any, url, token string) ([]string, error) {
	existing := tomlTable(tree, "mcp_servers", "satchel")
	if _, ok := existing["command"]; ok {
		return nil, unsupportedf(path, "[mcp_servers.satchel] 是 stdio 写法（有 command），mcp init 写的是 HTTP 加 Bearer；删掉这一块后重跑")
	}
	if _, ok := existing["bearer_token_env_var"]; ok {
		return nil, unsupportedf(path, "[mcp_servers.satchel] 配了 bearer_token_env_var，Codex 会用它取令牌，与 mcp init 写的 Authorization 冲突；删掉这个键后重跑")
	}
	h := findHeader(lines, "mcp_servers", "satchel")
	if h < 0 {
		if _, ok := tree["mcp_servers"]; ok && !anyHeaderUnder(lines, "mcp_servers") {
			return nil, unsupportedf(path, "mcp_servers 写成了内联表或点号键，mcp init 只改 [mcp_servers.satchel] 这种写法")
		}
		for k := range existing {
			if k != "tools" && k != "http_headers" {
				return nil, unsupportedf(path, "mcp_servers.satchel 不是用 [mcp_servers.satchel] 表头写的")
			}
		}
		if _, ok := existing["http_headers"]; ok {
			// 只有 [mcp_servers.satchel.http_headers] 子表、没有 [mcp_servers.satchel] 表头：并进子表，再补一块只有 url 的表头。
			s := findHeader(lines, "mcp_servers", "satchel", "http_headers")
			if s < 0 {
				return nil, unsupportedf(path, "mcp_servers.satchel.http_headers 的写法不在支持范围内")
			}
			return append(withBlankLine(setSubtable(lines, s, codexAuth(token))), "[mcp_servers.satchel]", codexURLLine(url)), nil
		}
		return append(withBlankLine(lines), append([]string{"[mcp_servers.satchel]"}, codexServerBody(url, token)...)...), nil
	}
	out := append([]string{}, lines...)
	at, hasURL, hasHeaders := h, false, false // at：缺的行插在它后面（有 url 行就插在 url 行后，否则在表头下）
	for i := h + 1; i < nextHeader(out, h+1); i++ {
		key, dotted, ok := lineKey(out[i])
		switch {
		case !ok || (key != "url" && key != "http_headers"):
		case dotted:
			return nil, unsupportedf(path, "[mcp_servers.satchel] 里的 %s 写成了点号键，mcp init 只改 %s = … 这种写法", key, key)
		case key == "url":
			out[i], hasURL, at = indentOf(out[i])+codexURLLine(url), true, i
		default:
			merged, err := mergeInlineTable(path, out[i], "http_headers", "mcp_servers.satchel.http_headers", codexAuth(token))
			if err != nil {
				return nil, err
			}
			out[i], hasHeaders = merged, true
		}
	}
	var insert []string
	if !hasURL {
		insert = append(insert, codexURLLine(url))
	}
	subtable := findHeader(out, "mcp_servers", "satchel", "http_headers") >= 0
	if !hasHeaders && !subtable {
		insert = append(insert, inlineTable("http_headers", codexAuth(token)))
	}
	out = append(append(append([]string{}, out[:at+1]...), insert...), out[at+1:]...)
	if !hasHeaders && subtable {
		out = setSubtable(out, findHeader(out, "mcp_servers", "satchel", "http_headers"), codexAuth(token))
	}
	return out, nil
}

func indentOf(line string) string { return line[:len(line)-len(strings.TrimLeft(line, " \t"))] }

// setCodexPolicy 把变量并进 [shell_environment_policy] 的 set：表不存在就追加；有表没 set 就在表头下插一行；
// set 是单行内联表就按原顺序合并改写那一行；set 是 [shell_environment_policy.set] 子表就逐键替换或追加。其余写法停下。
func setCodexPolicy(path string, lines []string, tree map[string]any, vars [][2]string) ([]string, error) {
	h := findHeader(lines, "shell_environment_policy")
	if h < 0 {
		if _, ok := tree["shell_environment_policy"]; ok {
			return nil, unsupportedf(path, "shell_environment_policy 写成了内联表或点号键，mcp init 只改 [shell_environment_policy] 这种写法")
		}
		return append(withBlankLine(lines), "[shell_environment_policy]", inlineTable("set", vars)), nil
	}
	for i := h + 1; i < nextHeader(lines, h+1); i++ {
		key, dotted, ok := lineKey(lines[i])
		if !ok || key != "set" {
			continue
		}
		if dotted {
			return nil, unsupportedf(path, "shell_environment_policy.set 写成了点号键，mcp init 只改内联表或 [shell_environment_policy.set] 子表")
		}
		merged, err := mergeInlineTable(path, lines[i], "set", "shell_environment_policy.set", vars)
		if err != nil {
			return nil, err
		}
		out := append([]string{}, lines...)
		out[i] = merged
		return out, nil
	}
	if s := findHeader(lines, "shell_environment_policy", "set"); s >= 0 {
		return setSubtable(lines, s, vars), nil
	}
	if _, ok := tomlTable(tree, "shell_environment_policy")["set"]; ok {
		return nil, unsupportedf(path, "shell_environment_policy.set 的写法不在支持范围内")
	}
	return append(append(append([]string{}, lines[:h+1]...), inlineTable("set", vars)), lines[h+1:]...), nil
}

// mergeInlineTable 把键值并进 key = { … } 这一行（what 是报错里的全名）：原有的键按原顺序保留，同名的就地换值，
// 没有的追加；缩进与行尾注释保留。值都得是字符串，跨行、嵌套与别的写法停下。
func mergeInlineTable(path, line, key, what string, vars [][2]string) (string, error) {
	eq := strings.Index(line, "=")
	table, rest, ok := splitInlineTable(line[eq+1:])
	if !ok {
		return "", unsupportedf(path, "%s 不是写在一行里的内联表", what)
	}
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", unsupportedf(path, "%s 那一行在内联表后面还有别的内容", what)
	}
	var m map[string]any
	md, err := toml.Decode(key+" = "+table, &m)
	if err != nil {
		return "", unsupportedf(path, "%s 解析不了（%v）", what, err)
	}
	values := tomlTable(m, key)
	var pairs [][2]string
	for _, k := range md.Keys() {
		if len(k) != 2 || k[0] != key {
			if len(k) > 2 {
				return "", unsupportedf(path, "%s 里有嵌套的表", what)
			}
			continue
		}
		v, ok := values[k[1]].(string)
		if !ok {
			return "", unsupportedf(path, "%s.%s 的值不是字符串", what, k[1])
		}
		pairs = append(pairs, [2]string{k[1], v})
	}
	for _, kv := range vars {
		found := false
		for i := range pairs {
			if pairs[i][0] == kv[0] {
				pairs[i][1], found = kv[1], true
			}
		}
		if !found {
			pairs = append(pairs, kv)
		}
	}
	out := indentOf(line) + inlineTable(key, pairs)
	if rest != "" {
		out += " " + rest
	}
	return out, nil
}

// setSubtable 在一张子表（[shell_environment_policy.set] 或 [mcp_servers.satchel.http_headers]）里逐键替换，
// 没有的追加在子表最后一个键值之后。
func setSubtable(lines []string, s int, vars [][2]string) []string {
	end := nextHeader(lines, s+1)
	out := append([]string{}, lines...)
	var missing []string
	for _, kv := range vars {
		line := tomlKey(kv[0]) + " = " + tomlString(kv[1])
		found := false
		for i := s + 1; i < end; i++ {
			if key, dotted, ok := lineKey(out[i]); ok && !dotted && key == kv[0] {
				out[i], found = line, true
			}
		}
		if !found {
			missing = append(missing, line)
		}
	}
	last := lastContent(out, s+1, end)
	return append(append(append([]string{}, out[:last+1]...), missing...), out[last+1:]...)
}

// checkCodexTree 是改后的整树比对：Satchel 的键是要写的值；去掉它们后改前改后一模一样。
func checkCodexTree(path string, before, after []byte, url, token string) error {
	var b, a map[string]any
	if _, err := toml.Decode(string(before), &b); err != nil {
		return unsupportedf(path, "不是合法的 TOML（%v）", err)
	}
	if _, err := toml.Decode(string(after), &a); err != nil {
		return unsupportedf(path, "改完不是合法的 TOML（%v），多半是写法不在支持范围内", err)
	}
	srv := tomlTable(a, "mcp_servers", "satchel")
	if srv["url"] != url+"/mcp" || tomlTable(srv, "http_headers")["Authorization"] != "Bearer "+token {
		return unsupportedf(path, "改完 [mcp_servers.satchel] 不是要写的内容")
	}
	set := tomlTable(a, "shell_environment_policy", "set")
	for _, kv := range satchelEnv(url, token) {
		if set[kv[0]] != kv[1] {
			return unsupportedf(path, "改完 shell_environment_policy.set.%s 不是要写的值", kv[0])
		}
	}
	if !reflect.DeepEqual(stripCodex(b), stripCodex(a)) {
		return unsupportedf(path, "改完除了 Satchel 的键还有别的变化")
	}
	return nil
}

// stripCodex 去掉 Satchel 的键：mcp_servers.satchel 的 url 与 http_headers.Authorization、set 里的三个变量；因此变空的表一并去掉。
func stripCodex(m map[string]any) map[string]any {
	if m == nil {
		m = map[string]any{}
	}
	if servers := tomlTable(m, "mcp_servers"); servers != nil {
		if s := tomlTable(servers, "satchel"); s != nil {
			delete(s, "url")
			if headers := tomlTable(s, "http_headers"); headers != nil {
				delete(headers, "Authorization")
				if len(headers) == 0 {
					delete(s, "http_headers")
				}
			}
			if len(s) == 0 {
				delete(servers, "satchel")
			}
		}
		if len(servers) == 0 {
			delete(m, "mcp_servers")
		}
	}
	if policy := tomlTable(m, "shell_environment_policy"); policy != nil {
		if set := tomlTable(policy, "set"); set != nil {
			for _, kv := range satchelEnv("", "") {
				delete(set, kv[0])
			}
			if len(set) == 0 {
				delete(policy, "set")
			}
		}
		if len(policy) == 0 {
			delete(m, "shell_environment_policy")
		}
	}
	return m
}

func tomlTable(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			return nil
		}
		m = next
	}
	return m
}

// tomlString 编码一个 TOML 基本字符串。
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

var bareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(k string) string {
	if bareKeyRe.MatchString(k) {
		return k
	}
	return tomlString(k)
}

func inlineTable(key string, pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, kv := range pairs {
		parts[i] = tomlKey(kv[0]) + " = " + tomlString(kv[1])
	}
	return key + " = { " + strings.Join(parts, ", ") + " }"
}

func splitLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func joinLines(lines []string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

// withBlankLine 在末尾追加一段之前补一个空行（文件为空或已以空行结尾就不补）。
func withBlankLine(lines []string) []string {
	out := append([]string{}, lines...)
	if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
		out = append(out, "")
	}
	return out
}

// tomlHeader 认一行表头（[a.b] 或 [[a.b]]，键可带引号，可带行尾注释），返回键路径；不是表头返回 ok=false。
// 多行字符串与多行数组里以 [ 开头的行可能被误认，改完的整树比对会把这种情况挡住。
func tomlHeader(line string) (keys []string, array bool, ok bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") {
		return nil, false, false
	}
	array = strings.HasPrefix(s, "[[")
	body := s[1:]
	if array {
		body = s[2:]
	}
	var seg strings.Builder
	var quote byte
	for i := 0; i < len(body); i++ {
		c := body[i]
		if quote != 0 {
			seg.WriteByte(c)
			if c == '\\' && quote == '"' && i+1 < len(body) {
				i++
				seg.WriteByte(body[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
			seg.WriteByte(c)
		case '.':
			keys = append(keys, unquoteKey(seg.String()))
			seg.Reset()
		case ']':
			keys = append(keys, unquoteKey(seg.String()))
			rest := body[i+1:]
			if array {
				if !strings.HasPrefix(rest, "]") {
					return nil, false, false
				}
				rest = rest[1:]
			}
			if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, false, false
			}
			for _, k := range keys {
				if k == "" {
					return nil, false, false
				}
			}
			return keys, array, true
		default:
			seg.WriteByte(c)
		}
	}
	return nil, false, false
}

func unquoteKey(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	case len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'':
		return s[1 : len(s)-1]
	}
	return s
}

func findHeader(lines []string, keys ...string) int {
	for i, l := range lines {
		if k, array, ok := tomlHeader(l); ok && !array && reflect.DeepEqual(k, keys) {
			return i
		}
	}
	return -1
}

func anyHeaderUnder(lines []string, first string) bool {
	for _, l := range lines {
		if k, _, ok := tomlHeader(l); ok && k[0] == first {
			return true
		}
	}
	return false
}

func nextHeader(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if _, _, ok := tomlHeader(lines[i]); ok {
			return i
		}
	}
	return len(lines)
}

// lastContent 是 [from, to) 里最后一行既不是空行也不是注释的行；没有就返回 from-1。
func lastContent(lines []string, from, to int) int {
	for i := to - 1; i >= from; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			return i
		}
	}
	return from - 1
}

var keyLineRe = regexp.MustCompile(`^\s*("(?:[^"\\]|\\.)*"|'[^']*'|[A-Za-z0-9_-]+)\s*([.=])`)

// lineKey 取一行键值的键（点号键取第一段）；不是键值行返回 ok=false。
func lineKey(line string) (key string, dotted bool, ok bool) {
	m := keyLineRe.FindStringSubmatch(line)
	if m == nil {
		return "", false, false
	}
	return unquoteKey(m[1]), m[2] == ".", true
}

// splitInlineTable 从 s 的开头切出一个写在一行里的内联表 { … }（引号里的括号不算），返回表的原文与后面剩下的内容。
func splitInlineTable(s string) (table, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t")
	if !strings.HasPrefix(s, "{") {
		return "", "", false
	}
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '{', '[':
			depth++
		case '}', ']':
			if depth--; depth == 0 {
				return s[:i+1], strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}
