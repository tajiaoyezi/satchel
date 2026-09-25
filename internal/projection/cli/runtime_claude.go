package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
)

// Claude Code（design 第 14、14a 条）：MCP 服务器只经它自己的命令登记（不碰 ~/.claude.json），登记的是 stdio 垫片；
// 三个环境变量写进 ~/.claude/settings.json 的 env（官方文档：为每个会话及其子进程设置），垫片是子进程，读得到。

const claudeVerify = "claude mcp get satchel"

func claudeNotes() []string {
	return []string{"重启 Claude Code 后生效（已开的会话不会重新读 settings.json 的 env）", "skills 随 m1-09 交付"}
}

func claudeSettingsPath(env runtimeEnv) string {
	return filepath.Join(env.home, ".claude", "settings.json")
}

func claudeCommands(claude, executable string) []initCommand {
	return []initCommand{
		{args: []string{claude, "mcp", "remove", "--scope", "user", "satchel"}, mayFail: true},
		{args: []string{claude, "mcp", "add", "--scope", "user", "satchel", "--", executable, "mcp", "stdio"}},
	}
}

func planClaudeCode(env runtimeEnv, url, token string) (*initPlan, error) {
	claude, err := env.lookPath("claude")
	if err != nil {
		return nil, unsupportedf("claude", "PATH 里找不到 claude 命令")
	}
	path := claudeSettingsPath(env)
	before, err := readOptional(path)
	if err != nil {
		return nil, err
	}
	vars := satchelEnv(url, token)
	after, err := setJSONEnv(path, before, vars)
	if err != nil {
		return nil, err
	}
	if err := checkJSONEnv(path, before, after, vars); err != nil {
		return nil, err
	}
	plan := &initPlan{
		edits:    []fileEdit{{path: path, before: before, after: after, secret: true}},
		commands: claudeCommands(claude, env.executable),
		verify:   claudeVerify, notes: claudeNotes(),
	}
	var old struct {
		Env map[string]any `json:"env"`
	}
	if json.Unmarshal(before, &old) == nil {
		if prev, _ := old.Env[EnvToken].(string); prev != "" && prev != token {
			plan.notes = append(plan.notes, replacedTokenNote(path))
		}
	}
	// 已经登记了同样的垫片就不再 remove / add（重跑 mcp init 换令牌时不碰登记）；否则记下原有的登记，add 失败时交还。
	switch prev := claudeRegistration(env); {
	case prev == nil:
	case sameClaudeRegistration(prev, env.executable):
		plan.commands = nil
		plan.notes = append(plan.notes, "Claude Code 里已经登记了同样的 satchel（"+env.executable+" mcp stdio），没有重新登记")
	default:
		plan.previous = string(prev)
	}
	return plan, nil
}

// claudeRegistration 读 Claude Code 在用户级登记的 satchel（~/.claude.json 顶层 mcpServers.satchel 的原文）；
// 没有或读不懂返回 nil。只读，不改这个文件（Claude Code 自己频繁改写它）。
func claudeRegistration(env runtimeEnv) json.RawMessage {
	raw, err := readOptional(filepath.Join(env.home, ".claude.json"))
	if err != nil || raw == nil {
		return nil
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	return cfg.MCPServers["satchel"]
}

// sameClaudeRegistration 报告原有的登记是不是与 mcp init 要登记的一样：stdio、命令是这个 satchel、参数恰好 mcp stdio、没有 env。
func sameClaudeRegistration(prev json.RawMessage, executable string) bool {
	var reg struct {
		Type    string         `json:"type"`
		Command string         `json:"command"`
		Args    []string       `json:"args"`
		Env     map[string]any `json:"env"`
	}
	if json.Unmarshal(prev, &reg) != nil {
		return false
	}
	return (reg.Type == "" || reg.Type == "stdio") && reg.Command == executable &&
		len(reg.Args) == 2 && reg.Args[0] == "mcp" && reg.Args[1] == "stdio" && len(reg.Env) == 0
}

func snippetClaudeCode(env runtimeEnv, url, token string) string {
	var b strings.Builder
	b.WriteString("# 执行：\n")
	for _, c := range claudeCommands("claude", env.executable) {
		b.WriteString(strings.Join(c.args, " ") + "\n")
	}
	fmt.Fprintf(&b, "# 在 %s 的 \"env\" 对象里加上：\n", claudeSettingsPath(env))
	vars := satchelEnv(url, token)
	for i, kv := range vars {
		sep := ","
		if i == len(vars)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "%s: %s%s\n", jsonString(kv[0]), jsonString(kv[1]), sep)
	}
	return strings.TrimRight(b.String(), "\n")
}

// jsonMember 是 JSON 对象的一个成员：键与值的原文。
type jsonMember struct {
	key   string
	value json.RawMessage
}

// parseJSONObject 按原顺序拆开一个 JSON 对象的成员（值保留原文）；空白当作空对象。
func parseJSONObject(raw []byte) ([]jsonMember, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("不是 JSON 对象")
	}
	var members []jsonMember
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, jsonMember{key: tok.(string), value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("对象后面还有别的内容")
	}
	return members, nil
}

// encodeJSONObject 按成员顺序编回对象；indent 为真时整体缩进两格（顶层的写法），否则紧凑。
func encodeJSONObject(members []jsonMember, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonString(m.key))
		buf.WriteByte(':')
		buf.Write(m.value)
	}
	buf.WriteByte('}')
	if !indent {
		return buf.Bytes(), nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// jsonString 编码一个 JSON 字符串，不把 & < > 转成 \u 形式（URL 里常有 &）。
func jsonString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// setJSONEnv 在 settings.json 的 env 对象里设变量：顶层成员与 env 里已有的键按原顺序保留，同名的就地换值，没有的追加。
// 顶层有重复的 env 时改最后一个（与 encoding/json 读到的一致）。
func setJSONEnv(path string, before []byte, vars [][2]string) ([]byte, error) {
	top, err := parseJSONObject(before)
	if err != nil {
		return nil, unsupportedf(path, "不是合法的 JSON 对象（%v）", err)
	}
	idx := -1
	for i, m := range top {
		if m.key == "env" {
			idx = i
		}
	}
	var env []jsonMember
	if idx >= 0 && !bytes.Equal(bytes.TrimSpace(top[idx].value), []byte("null")) {
		if env, err = parseJSONObject(top[idx].value); err != nil {
			return nil, unsupportedf(path, "env 不是 JSON 对象")
		}
	}
	for _, kv := range vars {
		found := false
		for i := range env {
			if env[i].key == kv[0] {
				env[i].value, found = jsonString(kv[1]), true
			}
		}
		if !found {
			env = append(env, jsonMember{key: kv[0], value: jsonString(kv[1])})
		}
	}
	raw, err := encodeJSONObject(env, false)
	if err != nil {
		return nil, err
	}
	if idx >= 0 {
		top[idx].value = raw
	} else {
		top = append(top, jsonMember{key: "env", value: raw})
	}
	return encodeJSONObject(top, true)
}

// checkJSONEnv 是改后的整树比对：env 里恰好是要设的值；去掉这几个变量后，改前改后一模一样。
func checkJSONEnv(path string, before, after []byte, vars [][2]string) error {
	var b, a map[string]any
	if len(bytes.TrimSpace(before)) > 0 {
		if err := json.Unmarshal(before, &b); err != nil {
			return unsupportedf(path, "不是合法的 JSON 对象（%v）", err)
		}
	}
	if err := json.Unmarshal(after, &a); err != nil {
		return unsupportedf(path, "改后不是合法的 JSON（%v）", err)
	}
	env, _ := a["env"].(map[string]any)
	for _, kv := range vars {
		if env[kv[0]] != kv[1] {
			return unsupportedf(path, "改后 env.%s 不是要设的值", kv[0])
		}
	}
	strip := func(m map[string]any) map[string]any {
		if m == nil {
			m = map[string]any{}
		}
		if v, ok := m["env"]; ok {
			env, isMap := v.(map[string]any)
			if isMap {
				for _, kv := range vars {
					delete(env, kv[0])
				}
			}
			if v == nil || (isMap && len(env) == 0) {
				delete(m, "env")
			}
		}
		return m
	}
	if !reflect.DeepEqual(strip(b), strip(a)) {
		return unsupportedf(path, "改完除了 env 里的三个变量还有别的变化")
	}
	return nil
}
