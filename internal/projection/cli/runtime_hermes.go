package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Hermes（design 第 14 条，notes/runtime-configs.md B1、B2）：$HERMES_HOME/.env 里设三个变量（Hermes 启动时载入进程环境，
// 默认的 local 终端能继承）；config.yaml 的 mcp_servers.satchel 里设 url 与 headers.Authorization（写 Bearer ${SATCHEL_TOKEN}，
// Hermes 从 .env 展开，令牌只在 .env 一处），条目里其余的键原样保留（用户的 enabled、timeout、trust、tools 与别的头都不动），
// 并把三个变量名并进 terminal.env_passthrough（execute_code 与非本机的终端后端默认会删掉名字含 TOKEN 的变量）。
// YAML 经 yaml.v3 的节点树改，注释保留，缩进统一成两格。

const hermesVerify = "hermes mcp test satchel"

// hermesConfigFile 是 Hermes 自己的配置文件名（在 $HERMES_HOME 下），与 Satchel 数据目录里的 config.yaml 无关。
const hermesConfigFile = "config.yaml"

func hermesNotes() []string {
	return []string{"在 Hermes 会话里执行 /reload-mcp 或重启 Hermes 后生效", "skills 随 m1-09 交付"}
}

const hermesAuthorization = "Bearer ${" + EnvToken + "}"

func planHermes(env runtimeEnv, url, token string) (*initPlan, error) {
	envPath := filepath.Join(env.hermesHome, ".env")
	cfgPath := filepath.Join(env.hermesHome, hermesConfigFile)
	vars := satchelEnv(url, token)
	beforeEnv, err := readOptional(envPath)
	if err != nil {
		return nil, err
	}
	afterEnv := setDotenv(beforeEnv, vars)
	if err := checkDotenv(envPath, beforeEnv, afterEnv, vars); err != nil {
		return nil, err
	}
	beforeCfg, err := readOptional(cfgPath)
	if err != nil {
		return nil, err
	}
	afterCfg, err := setHermesConfig(cfgPath, beforeCfg, url)
	if err != nil {
		return nil, err
	}
	if err := checkHermesConfig(cfgPath, beforeCfg, afterCfg, url); err != nil {
		return nil, err
	}
	plan := &initPlan{
		edits:  []fileEdit{{path: envPath, before: beforeEnv, after: afterEnv, secret: true}, {path: cfgPath, before: beforeCfg, after: afterCfg}},
		verify: hermesVerify, notes: hermesNotes(),
	}
	if prev := parseDotenv(beforeEnv)[EnvToken]; prev != "" && prev != token {
		plan.notes = append(plan.notes, replacedTokenNote(envPath))
	}
	var old map[string]any
	if yaml.Unmarshal(beforeCfg, &old) == nil {
		if srv, ok := yamlDig(old, "mcp_servers", "satchel").(map[string]any); ok && srv["enabled"] == false {
			plan.notes = append(plan.notes, "config.yaml 的 mcp_servers.satchel 里 enabled 是 false，mcp init 没有改它，接入后仍是停用的；要启用就改成 true")
		}
	}
	return plan, nil
}

func snippetHermes(env runtimeEnv, url, token string) string {
	lines := []string{"# 加进 " + filepath.Join(env.hermesHome, ".env") + "："}
	for _, kv := range satchelEnv(url, token) {
		lines = append(lines, kv[0]+"="+dotenvValue(kv[1]))
	}
	lines = append(lines, "# 并进 "+filepath.Join(env.hermesHome, hermesConfigFile)+"：",
		"mcp_servers:", "  satchel:", fmt.Sprintf("    url: %q", url+"/mcp"), "    headers:", fmt.Sprintf("      Authorization: %q", hermesAuthorization),
		"terminal:", "  env_passthrough: ["+EnvToken+", "+EnvServer+", SATCHEL_OUTPUT]")
	return strings.Join(lines, "\n")
}

var dotenvKeyRe = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// setDotenv 在 .env 里设变量：同名的行就地替换（有几行换几行），其余行与注释原样保留，没有的追加在末尾。
func setDotenv(before []byte, vars [][2]string) []byte {
	lines := splitLines(before)
	for _, kv := range vars {
		line := kv[0] + "=" + dotenvValue(kv[1])
		found := false
		for i, l := range lines {
			if m := dotenvKeyRe.FindStringSubmatch(l); m != nil && m[1] == kv[0] {
				lines[i], found = line, true
			}
		}
		if !found {
			lines = append(lines, line)
		}
	}
	return joinLines(lines)
}

// dotenvValue 给含空白、引号、# 或 $ 的值加双引号；地址、令牌与 json 都用不上。
func dotenvValue(v string) string {
	if !strings.ContainsAny(v, " \t#\"'$\\") {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(v) + `"`
}

// parseDotenv 把 .env 解成键到值（去掉 export 与成对的引号）；只用于改前改后的比对。
func parseDotenv(raw []byte) map[string]string {
	out := map[string]string{}
	for _, l := range splitLines(raw) {
		m := dotenvKeyRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v := strings.TrimSpace(l[len(m[0]):])
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\$`, `$`).Replace(v[1 : len(v)-1])
		} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		out[m[1]] = v
	}
	return out
}

func checkDotenv(path string, before, after []byte, vars [][2]string) error {
	b, a := parseDotenv(before), parseDotenv(after)
	for _, kv := range vars {
		if a[kv[0]] != kv[1] {
			return unsupportedf(path, "改完 %s 不是要写的值", kv[0])
		}
		delete(a, kv[0])
		delete(b, kv[0])
	}
	if !reflect.DeepEqual(b, a) {
		return unsupportedf(path, "改完除了三个 SATCHEL_ 变量还有别的变化")
	}
	return nil
}

// setHermesConfig 在 config.yaml 的节点树上设 mcp_servers.satchel 并把三个变量名并进 terminal.env_passthrough（去重）。
func setHermesConfig(path string, before []byte, url string) ([]byte, error) {
	doc, err := parseYAMLDocument(path, before)
	if err != nil {
		return nil, err
	}
	root := doc.Content[0]
	servers, err := yamlMapping(path, root, "mcp_servers", "mcp_servers")
	if err != nil {
		return nil, err
	}
	satchel, err := yamlMapping(path, servers, "satchel", "mcp_servers.satchel")
	if err != nil {
		return nil, err
	}
	if yamlGet(satchel, "command") != nil {
		return nil, unsupportedf(path, "mcp_servers.satchel 是 stdio 写法（有 command），mcp init 写的是 HTTP 加 Bearer；删掉这一条后重跑")
	}
	yamlSet(satchel, "url", yamlString(url+"/mcp"))
	headers, err := yamlMapping(path, satchel, "headers", "mcp_servers.satchel.headers")
	if err != nil {
		return nil, err
	}
	yamlSet(headers, "Authorization", yamlString(hermesAuthorization))
	terminal, err := yamlMapping(path, root, "terminal", "terminal")
	if err != nil {
		return nil, err
	}
	pass := yamlGet(terminal, "env_passthrough")
	if pass == nil || (pass.Kind == yaml.ScalarNode && pass.Tag == "!!null") {
		pass = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		yamlSet(terminal, "env_passthrough", pass)
	}
	if pass.Kind != yaml.SequenceNode {
		return nil, unsupportedf(path, "terminal.env_passthrough 不是列表")
	}
	for _, kv := range satchelEnv("", "") {
		found := false
		for _, item := range pass.Content {
			if item.Kind == yaml.ScalarNode && item.Value == kv[0] {
				found = true
			}
		}
		if !found {
			pass.Content = append(pass.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[0]})
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, unsupportedf(path, "改完编码不出来（%v）", err)
	}
	if err := enc.Close(); err != nil {
		return nil, unsupportedf(path, "改完编码不出来（%v）", err)
	}
	return buf.Bytes(), nil
}

// parseYAMLDocument 解析出唯一的一个文档，顶层必须是映射；空文件当作空映射。有多个文档就停下（只改第一个会丢掉别的）。
func parseYAMLDocument(path string, raw []byte) (*yaml.Node, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var first yaml.Node
	switch err := dec.Decode(&first); {
	case errors.Is(err, io.EOF):
		return doc, nil
	case err != nil:
		return nil, unsupportedf(path, "不是合法的 YAML（%v）", err)
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		return nil, unsupportedf(path, "有多个 YAML 文档，mcp init 只改单个文档的文件")
	}
	if len(first.Content) != 1 || first.Content[0].Kind != yaml.MappingNode {
		if len(first.Content) == 1 && first.Content[0].Tag == "!!null" {
			first.Content[0] = doc.Content[0]
			return &first, nil
		}
		return nil, unsupportedf(path, "顶层不是映射")
	}
	return &first, nil
}

func yamlGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlSet 设映射里一个键的值：已有就换值节点（键节点与它的注释留着），没有就追加。
func yamlSet(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// yamlMapping 取映射里一个键的值作为映射：没有或是 null 就建一个空映射，是别的东西就停下（what 是报错里的全名）。
func yamlMapping(path string, m *yaml.Node, key, what string) (*yaml.Node, error) {
	v := yamlGet(m, key)
	if v == nil || (v.Kind == yaml.ScalarNode && v.Tag == "!!null") {
		v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		yamlSet(m, key, v)
	}
	if v.Kind != yaml.MappingNode {
		return nil, unsupportedf(path, "%s 不是映射", what)
	}
	return v, nil
}

func yamlString(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s, Style: yaml.DoubleQuotedStyle}
}

// checkHermesConfig 是改后的整树比对：mcp_servers.satchel 与 env_passthrough 是要写的样子；去掉它们后改前改后一模一样。
func checkHermesConfig(path string, before, after []byte, url string) error {
	var b, a map[string]any
	if err := yaml.Unmarshal(before, &b); err != nil {
		return unsupportedf(path, "不是合法的 YAML（%v）", err)
	}
	if err := yaml.Unmarshal(after, &a); err != nil {
		return unsupportedf(path, "改完不是合法的 YAML（%v）", err)
	}
	srv, _ := yamlDig(a, "mcp_servers", "satchel").(map[string]any)
	headers, _ := srv["headers"].(map[string]any)
	if srv["url"] != url+"/mcp" || headers["Authorization"] != hermesAuthorization {
		return unsupportedf(path, "改完 mcp_servers.satchel 不是要写的内容")
	}
	pass, _ := yamlDig(a, "terminal", "env_passthrough").([]any)
	for _, kv := range satchelEnv("", "") {
		n := 0
		for _, p := range pass {
			if p == kv[0] {
				n++
			}
		}
		if n != 1 {
			return unsupportedf(path, "改完 terminal.env_passthrough 里的 %s 有 %d 个", kv[0], n)
		}
	}
	if !reflect.DeepEqual(stripHermes(b), stripHermes(a)) {
		return unsupportedf(path, "改完除了 Satchel 的键还有别的变化")
	}
	return nil
}

func yamlDig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}

// stripHermes 去掉 Satchel 的键：mcp_servers.satchel 的 url 与 headers.Authorization、env_passthrough 里的三个变量名；
// 因此变空的键一并去掉。
func stripHermes(m map[string]any) map[string]any {
	if m == nil {
		m = map[string]any{}
	}
	if v, ok := m["mcp_servers"]; ok {
		servers, isMap := v.(map[string]any)
		if isMap {
			if sv, ok := servers["satchel"]; ok {
				satchel, isMap := sv.(map[string]any)
				if isMap {
					delete(satchel, "url")
					if hv, ok := satchel["headers"]; ok {
						headers, isMap := hv.(map[string]any)
						if isMap {
							delete(headers, "Authorization")
						}
						if hv == nil || (isMap && len(headers) == 0) {
							delete(satchel, "headers")
						}
					}
				}
				if sv == nil || (isMap && len(satchel) == 0) {
					delete(servers, "satchel")
				}
			}
		}
		if v == nil || (isMap && len(servers) == 0) {
			delete(m, "mcp_servers")
		}
	}
	if v, ok := m["terminal"]; ok {
		terminal, isMap := v.(map[string]any)
		if isMap {
			if p, ok := terminal["env_passthrough"]; ok {
				list, isList := p.([]any)
				var kept []any
				for _, item := range list {
					if item != EnvToken && item != EnvServer && item != "SATCHEL_OUTPUT" {
						kept = append(kept, item)
					}
				}
				if p == nil || (isList && len(kept) == 0) {
					delete(terminal, "env_passthrough")
				} else if isList {
					terminal["env_passthrough"] = kept
				}
			}
		}
		if v == nil || (isMap && len(terminal) == 0) {
			delete(m, "terminal")
		}
	}
	return m
}
