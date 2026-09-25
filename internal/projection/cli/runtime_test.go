package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

const (
	testURL   = "https://panel.example.com"
	testToken = "sat_realtokenrealtokenrealtokenrealtokenreal0"
)

func testRuntimeEnv(t *testing.T) runtimeEnv {
	t.Helper()
	home := t.TempDir()
	return runtimeEnv{
		home: home, codexHome: filepath.Join(home, ".codex"), hermesHome: filepath.Join(home, ".hermes"), executable: "/opt/satchel/bin/satchel",
		lookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
		run:      func(string, ...string) ([]byte, error) { return nil, nil },
	}
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func wantUnsupported(t *testing.T, err error, contains string) {
	t.Helper()
	var u errUnsupported
	if !errors.As(err, &u) || !strings.Contains(u.Error(), contains) {
		t.Fatalf("应当停下并说明 %q：%v", contains, err)
	}
}

// Codex 的各种已有写法：能改的改对（整树只多 Satchel 的键），不能改的在预检停下。
func TestRuntimeCodexShapes(t *testing.T) {
	good := []struct {
		name, before string
		check        func(t *testing.T, after string)
	}{
		{"空文件", "", nil},
		{"注释、别的服务器与已有的 policy", "# my codex config\nmodel = \"o5\"\n\n[mcp_servers.docs]\nurl = \"https://docs.example/mcp\" # docs\n\n[shell_environment_policy]\ninherit = \"all\"\n",
			func(t *testing.T, after string) {
				for _, want := range []string{"# my codex config", `url = "https://docs.example/mcp" # docs`, "[shell_environment_policy]\nset = { SATCHEL_SERVER", "inherit = \"all\""} {
					if !strings.Contains(after, want) {
						t.Errorf("应当保留或写入 %q：\n%s", want, after)
					}
				}
				if !strings.HasSuffix(after, "[mcp_servers.satchel]\nurl = \"https://panel.example.com/mcp\"\nhttp_headers = { Authorization = \"Bearer "+testToken+"\" }\n") {
					t.Errorf("satchel 一块应当追加在末尾：\n%s", after)
				}
			}},
		{"已有 satchel 块与 tools 子表", "[mcp_servers.satchel]\nurl = \"http://old/mcp\"\nstartup_timeout_sec = 20\n\n# tools\n[mcp_servers.satchel.tools.satchel_run]\napproval_mode = \"approve\"\n",
			func(t *testing.T, after string) {
				want := "[mcp_servers.satchel]\nurl = \"https://panel.example.com/mcp\"\nhttp_headers = { Authorization = \"Bearer " + testToken + "\" }\nstartup_timeout_sec = 20\n\n# tools\n[mcp_servers.satchel.tools.satchel_run]\napproval_mode = \"approve\"\n"
				if !strings.HasPrefix(after, want) {
					t.Errorf("只换 url、补 http_headers，别的键与 tools 子表不动：\n%s", after)
				}
			}},
		{"用户关掉与收窄的设置保留", "[mcp_servers.satchel]\nurl = \"http://127.0.0.1:12889/mcp\"\nenabled = false\ndisabled_tools = [\"satchel_run\"]\nrequired = true\n",
			func(t *testing.T, after string) {
				for _, want := range []string{"enabled = false", `disabled_tools = ["satchel_run"]`, "required = true"} {
					if !strings.Contains(after, want) {
						t.Errorf("应当保留 %q：\n%s", want, after)
					}
				}
			}},
		{"http_headers 内联表并进 Authorization", "[mcp_servers.satchel]\nurl = \"http://old/mcp\"\nhttp_headers = { \"X-Env\" = \"prod\", Authorization = \"Bearer old\" } # h\n",
			func(t *testing.T, after string) {
				if !strings.Contains(after, `http_headers = { X-Env = "prod", Authorization = "Bearer `+testToken+`" } # h`) {
					t.Errorf("别的头与注释保留：\n%s", after)
				}
			}},
		{"http_headers 子表", "[mcp_servers.satchel]\nurl = \"http://old/mcp\"\n\n[mcp_servers.satchel.http_headers]\nX-Env = \"prod\"\n",
			func(t *testing.T, after string) {
				if !strings.Contains(after, "[mcp_servers.satchel.http_headers]\nX-Env = \"prod\"\nAuthorization = \"Bearer "+testToken+"\"") || strings.Count(after, "http_headers") != 1 {
					t.Errorf("子表里逐键追加，不另写内联表：\n%s", after)
				}
			}},
		{"只有 http_headers 子表", "[mcp_servers.satchel.http_headers]\nX-Env = \"prod\"\n", nil},
		{"只有 tools 子表", "[mcp_servers.satchel.tools.satchel_run]\napproval_mode = \"approve\"\n", nil},
		{"内联的 set 合并", "[shell_environment_policy]\nset = { FOO = \"1\", SATCHEL_TOKEN = \"old\" } # keep\n",
			func(t *testing.T, after string) {
				if !strings.Contains(after, `set = { FOO = "1", SATCHEL_TOKEN = "`+testToken+`", SATCHEL_SERVER = "https://panel.example.com", SATCHEL_OUTPUT = "json" } # keep`) {
					t.Errorf("内联 set 按原顺序合并：\n%s", after)
				}
			}},
		{"set 子表", "[shell_environment_policy]\ninherit = \"core\"\n\n[shell_environment_policy.set]\nFOO = \"1\"\nSATCHEL_OUTPUT = \"text\"\n\n[other]\nx = 1\n",
			func(t *testing.T, after string) {
				if !strings.Contains(after, "[shell_environment_policy.set]\nFOO = \"1\"\nSATCHEL_OUTPUT = \"json\"\nSATCHEL_SERVER = \"https://panel.example.com\"\nSATCHEL_TOKEN = ") {
					t.Errorf("子表逐键替换或追加：\n%s", after)
				}
			}},
		{"include 过滤已放行 SATCHEL_*", "[shell_environment_policy]\ninclude_only = [\"PATH\", \"satchel_*\"]\n", nil},
		{"没有换行结尾", "model = \"o5\"", nil},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			env := testRuntimeEnv(t)
			path := codexConfigPath(env)
			if tc.before != "" {
				put(t, path, tc.before)
			}
			plan, err := planCodex(env, testURL, testToken)
			if err != nil {
				t.Fatal(err)
			}
			after := string(plan.edits[0].after)
			var tree map[string]any
			if _, err := toml.Decode(after, &tree); err != nil {
				t.Fatalf("改完应当是合法的 TOML：%v\n%s", err, after)
			}
			if tc.check != nil {
				tc.check(t, after)
			}
			// 再跑一次是幂等的：内容不再变化。
			put(t, path, after)
			again, err := planCodex(env, testURL, testToken)
			if err != nil || string(again.edits[0].after) != after {
				t.Fatalf("第二次应当不变：%v\n%s", err, again.edits[0].after)
			}
		})
	}
	bad := []struct{ name, before, why string }{
		{"不是 TOML", "model = \n", "不是合法的 TOML"},
		{"include_only 不放行", "[shell_environment_policy]\ninclude_only = [\"PATH\", \"HOME\"]\n", "include_only"},
		{"filters 里的 include 不放行", "[shell_environment_policy.filters]\n\"PATH\" = \"include\"\n", "filters"},
		{"mcp_servers 写成内联表", "mcp_servers = { docs = { url = \"https://docs.example/mcp\" } }\n", "内联表或点号键"},
		{"satchel 写成点号键", "[mcp_servers]\nsatchel.url = \"http://old/mcp\"\n", "不是用 [mcp_servers.satchel] 表头写的"},
		{"satchel 是 stdio 写法", "[mcp_servers.satchel]\ncommand = \"satchel\"\nargs = [\"mcp\", \"stdio\"]\n", "stdio 写法"},
		{"配了 bearer_token_env_var", "[mcp_servers.satchel]\nurl = \"http://old/mcp\"\nbearer_token_env_var = \"SATCHEL_TOKEN\"\n", "bearer_token_env_var"},
		{"http_headers 写成点号键", "[mcp_servers.satchel]\nhttp_headers.Authorization = \"Bearer old\"\n", "点号键"},
		{"policy 写成点号键", "shell_environment_policy.inherit = \"all\"\n", "内联表或点号键"},
		{"set 跨行（TOML 1.0 不许，照样停下）", "[shell_environment_policy]\nset = {\n  FOO = \"1\" }\n", "不是合法的 TOML"},
		{"set 的值不是字符串", "[shell_environment_policy]\nset = { FOO = 1 }\n", "不是字符串"},
		{"set 写成点号键", "[shell_environment_policy]\nset.FOO = \"1\"\n", "点号键"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			env := testRuntimeEnv(t)
			put(t, codexConfigPath(env), tc.before)
			_, err := planCodex(env, testURL, testToken)
			wantUnsupported(t, err, tc.why)
		})
	}
}

// Hermes：.env 按行替换或追加；config.yaml 的 satchel 条目与 env_passthrough 去重，别的内容与注释保留。
func TestRuntimeHermes(t *testing.T) {
	env := testRuntimeEnv(t)
	envPath, cfgPath := filepath.Join(env.hermesHome, ".env"), filepath.Join(env.hermesHome, hermesConfigFile)
	put(t, envPath, "# keys\nOPENAI_API_KEY=sk-1\nexport SATCHEL_TOKEN=old\n")
	put(t, cfgPath, "# hermes\nmodel: nous\nmcp_servers:\n  docs:\n    url: https://docs.example/mcp # docs\nterminal:\n  env_passthrough: [FOO, SATCHEL_SERVER]\n")
	plan, err := planHermes(env, testURL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	gotEnv := string(plan.edits[0].after)
	if gotEnv != "# keys\nOPENAI_API_KEY=sk-1\nSATCHEL_TOKEN="+testToken+"\nSATCHEL_SERVER=https://panel.example.com\nSATCHEL_OUTPUT=json\n" {
		t.Fatalf(".env：\n%s", gotEnv)
	}
	gotCfg := string(plan.edits[1].after)
	var tree map[string]any
	if err := yaml.Unmarshal([]byte(gotCfg), &tree); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotCfg, testToken) || !strings.Contains(gotCfg, `Authorization: "Bearer ${SATCHEL_TOKEN}"`) || !strings.Contains(gotCfg, "# hermes") || !strings.Contains(gotCfg, "# docs") {
		t.Fatalf("config.yaml 里没有令牌明文、保留注释：\n%s", gotCfg)
	}
	pass := yamlDig(tree, "terminal", "env_passthrough").([]any)
	if len(pass) != 4 || pass[0] != "FOO" || pass[1] != "SATCHEL_SERVER" {
		t.Fatalf("env_passthrough 应当是 FOO 加三个变量名、不重复：%v", pass)
	}
	if yamlDig(tree, "mcp_servers", "docs", "url") != "https://docs.example/mcp" || yamlDig(tree, "mcp_servers", "satchel", "url") != testURL+"/mcp" {
		t.Fatalf("mcp_servers：%v", tree["mcp_servers"])
	}
	// 再跑一次不变。
	put(t, envPath, gotEnv)
	put(t, cfgPath, gotCfg)
	again, err := planHermes(env, testURL, testToken)
	if err != nil || string(again.edits[0].after) != gotEnv || string(again.edits[1].after) != gotCfg {
		t.Fatalf("第二次应当不变：%v", err)
	}
	// 文件都不存在时新建；写不了的形状停下。
	fresh := testRuntimeEnv(t)
	if plan, err := planHermes(fresh, testURL, testToken); err != nil || plan.edits[0].before != nil || plan.edits[1].before != nil {
		t.Fatalf("没有文件时新建：%v", err)
	}
	for why, content := range map[string]string{
		"不是合法的 YAML":                       "a: [\n",
		"顶层不是映射":                           "- a\n- b\n",
		"terminal 不是映射":                    "terminal: local\n",
		"env_passthrough 不是列表":             "terminal:\n  env_passthrough: FOO\n",
		"多个 YAML 文档":                       "a: 1\n---\nb: 2\n",
		"stdio 写法":                         "mcp_servers:\n  satchel:\n    command: satchel\n    args: [mcp, stdio]\n",
		"mcp_servers.satchel.headers 不是映射": "mcp_servers:\n  satchel:\n    headers: x\n",
	} {
		e := testRuntimeEnv(t)
		put(t, filepath.Join(e.hermesHome, hermesConfigFile), content)
		_, err := planHermes(e, testURL, testToken)
		wantUnsupported(t, err, why)
	}
}

// Claude Code：settings.json 的顶层键顺序与 env 里已有的键保留；命令登记垫片的绝对路径，参数里没有令牌。
func TestRuntimeClaudeSettings(t *testing.T) {
	env := testRuntimeEnv(t)
	path := claudeSettingsPath(env)
	put(t, path, `{"theme": "dark", "env": {"FOO": "1", "SATCHEL_OUTPUT": "text"}, "permissions": {"allow": ["Bash(ls:*)"]}, "model": "opus"}`)
	plan, err := planClaudeCode(env, testURL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	after := string(plan.edits[0].after)
	want := "{\n  \"theme\": \"dark\",\n  \"env\": {\n    \"FOO\": \"1\",\n    \"SATCHEL_OUTPUT\": \"json\",\n    \"SATCHEL_SERVER\": \"https://panel.example.com\",\n    \"SATCHEL_TOKEN\": \"" + testToken + "\"\n  },\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(ls:*)\"\n    ]\n  },\n  \"model\": \"opus\"\n}\n"
	if after != want {
		t.Fatalf("settings.json：\n%s\n想要：\n%s", after, want)
	}
	if len(plan.commands) != 2 || strings.Join(plan.commands[0].args, " ") != "/usr/local/bin/claude mcp remove --scope user satchel" || !plan.commands[0].mayFail ||
		strings.Join(plan.commands[1].args, " ") != "/usr/local/bin/claude mcp add --scope user satchel -- /opt/satchel/bin/satchel mcp stdio" {
		t.Fatalf("命令：%+v", plan.commands)
	}
	for _, c := range plan.commands {
		if strings.Contains(strings.Join(c.args, " "), testToken) {
			t.Fatal("命令参数里不能有令牌")
		}
	}
	// 没有 settings.json 就新建；解析不了、env 不是对象、找不到 claude 都停下。
	fresh := testRuntimeEnv(t)
	if plan, err := planClaudeCode(fresh, testURL, testToken); err != nil || !strings.Contains(string(plan.edits[0].after), `"SATCHEL_TOKEN"`) {
		t.Fatalf("新建：%v", err)
	}
	for why, content := range map[string]string{"不是合法的 JSON 对象": "{not json", "env 不是 JSON 对象": `{"env": "x"}`} {
		e := testRuntimeEnv(t)
		put(t, claudeSettingsPath(e), content)
		_, err := planClaudeCode(e, testURL, testToken)
		wantUnsupported(t, err, why)
	}
	noClaude := testRuntimeEnv(t)
	noClaude.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	_, err = planClaudeCode(noClaude, testURL, testToken)
	wantUnsupported(t, err, "找不到 claude")
}

// Hermes 的 mcp_servers.satchel 里用户写的别的键（enabled、timeout、trust、tools、别的头）原样保留，只换 url 与 Authorization；
// enabled 是 false 时提示接入后仍是停用的。
func TestRuntimeHermesKeepsUserSettings(t *testing.T) {
	env := testRuntimeEnv(t)
	put(t, filepath.Join(env.hermesHome, hermesConfigFile), `mcp_servers:
  docs:
    url: https://docs.example/mcp
  satchel:
    url: https://old.example/mcp
    enabled: false
    timeout: 15
    trust: untrusted
    headers:
      Authorization: "Bearer ${SATCHEL_TOKEN}"
      X-Custom: keep-me
    tools:
      include: [satchel_explain]
`)
	plan, err := planHermes(env, testURL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(plan.edits[1].after, &tree); err != nil {
		t.Fatal(err)
	}
	srv := yamlDig(tree, "mcp_servers", "satchel").(map[string]any)
	headers := srv["headers"].(map[string]any)
	include, _ := yamlDig(srv, "tools", "include").([]any)
	if srv["url"] != testURL+"/mcp" || srv["enabled"] != false || srv["timeout"] != 15 || srv["trust"] != "untrusted" ||
		headers["X-Custom"] != "keep-me" || headers["Authorization"] != hermesAuthorization || len(include) != 1 || include[0] != "satchel_explain" {
		t.Fatalf("用户的设置应当原样保留：%v", srv)
	}
	if yamlDig(tree, "mcp_servers", "docs", "url") != "https://docs.example/mcp" {
		t.Fatal("别的服务器不动")
	}
	notes := strings.Join(plan.notes, "\n")
	if !strings.Contains(notes, "enabled 是 false") {
		t.Fatalf("应当提示仍是停用的：%v", plan.notes)
	}
}
