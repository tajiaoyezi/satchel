package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/skills"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// embeddedSkills 是编进二进制的 skills：名字到 SKILL.md 的内容。
func embeddedSkills(t *testing.T) map[string][]byte {
	t.Helper()
	list, problems, err := skills.Load(skills.Files())
	if err != nil || len(problems) != 0 {
		t.Fatalf("编进二进制的 skills：%v %v", err, problems)
	}
	out := map[string][]byte{}
	for _, s := range list {
		out[s.Name] = []byte(s.Content)
	}
	return out
}

// wantSkillsIn 断言 dir 下每个 skill 的 SKILL.md 与编进二进制的逐字节相同。
func wantSkillsIn(t *testing.T, dir string) {
	t.Helper()
	for name, content := range embeddedSkills(t) {
		got, err := os.ReadFile(filepath.Join(dir, name, skills.FileName))
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("%s 下的 %s 应当与二进制里的相同：%v", dir, name, err)
		}
	}
}

func decodeSkillsOnly(t *testing.T, stdout string) skillsOnlyOutput {
	t.Helper()
	var out skillsOnlyOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("输出解不开：%v\n%s", err, stdout)
	}
	return out
}

func notExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s 不该被创建：%v", path, err)
	}
}

// master-mcp「接入时装上 skills」：七个 satchel-* 与二进制里的相同，别的 skill 不动；files 里只有配置文件，skills 单独一项；
// 提示有重启生效与会被覆盖，没有「skills 随 m1-10 交付」。
func TestMcpInitInstallsSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假 claude 是 shell 脚本")
	}
	home, _ := runtimeHome(t)
	dataDir, _ := startSocketMaster(t)
	dir := filepath.Join(home, ".claude", "skills")
	put(t, filepath.Join(dir, "my-notes", skills.FileName), "mine\n")
	opts, _ := initOptions("secret12", "")
	stdout, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "claude-code", "--verify-user", "admin", "--url", targetMaster(t), "--data-dir", dataDir, "--json")
	if code != 0 {
		t.Fatalf("mcp init：%d %s", code, stderr)
	}
	out := decodeInit(t, stdout)
	wantSkillsIn(t, dir)
	if raw, _ := os.ReadFile(filepath.Join(dir, "my-notes", skills.FileName)); string(raw) != "mine\n" {
		t.Fatalf("别的 skill 不该被动：%q", raw)
	}
	if len(out.Files) != 1 || !strings.HasSuffix(out.Files[0].Path, "settings.json") {
		t.Fatalf("files 里只有配置文件：%+v", out.Files)
	}
	if out.Skills.Dir != dir || len(out.Skills.Written) != 7 || len(out.Skills.Unchanged) != 0 {
		t.Fatalf("skills 一项：%+v", out.Skills)
	}
	notes := strings.Join(out.Notes, "\n")
	if !strings.Contains(notes, "重启 Claude Code 之后生效") || !strings.Contains(notes, "改了会被下次 mcp init 覆盖") || strings.Contains(notes, "随 m1-10") {
		t.Fatalf("提示：%v", out.Notes)
	}
	// 新文件 0600、新目录 0700。
	if info, _ := os.Stat(filepath.Join(dir, "satchel-basics", skills.FileName)); info.Mode().Perm() != 0o600 {
		t.Fatalf("新文件 0600：%v", info.Mode())
	}
	if info, _ := os.Stat(filepath.Join(dir, "satchel-basics")); info.Mode().Perm() != 0o700 {
		t.Fatalf("新目录 0700：%v", info.Mode())
	}
}

// master-mcp「Claude Code 的配置跟着 CLAUDE_CONFIG_DIR」：~/.claude.json 里同样的登记不算数，照常 remove 与 add；
// settings.json 与 skills 写在 X 下，~/.claude 不被创建；X/.claude.json 里不同的登记在 add 失败时交还。
func TestMcpInitClaudeConfigDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假 claude 是 shell 脚本")
	}
	home, claudeLog := runtimeHome(t)
	dataDir, _ := startSocketMaster(t)
	target := targetMaster(t)
	x := t.TempDir()
	t.Setenv(claudeConfigEnv, x)
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	put(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"satchel":{"type":"stdio","command":`+string(jsonString(exe))+`,"args":["mcp","stdio"],"env":{}}}}`)
	opts, _ := initOptions("secret12", "")
	if _, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "claude-code", "--verify-user", "admin", "--url", target, "--data-dir", dataDir, "--json"); code != 0 {
		t.Fatalf("mcp init：%d %s", code, stderr)
	}
	if logged, _ := os.ReadFile(claudeLog); !strings.Contains(string(logged), "mcp remove") || !strings.Contains(string(logged), "mcp add") {
		t.Fatalf("~/.claude.json 里的登记不算数，应当照常登记：%q", logged)
	}
	raw, _ := os.ReadFile(filepath.Join(x, "settings.json"))
	var got struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(raw, &got) != nil || got.Env[EnvToken] != issuedToken || got.Env[EnvServer] != target || got.Env["SATCHEL_OUTPUT"] != "json" {
		t.Fatalf("X/settings.json 的 env：%s", raw)
	}
	wantSkillsIn(t, filepath.Join(x, "skills"))
	notExist(t, filepath.Join(home, ".claude"))

	previous := `{"type":"stdio","command":"/old/satchel","args":["mcp","stdio"],"env":{}}`
	put(t, filepath.Join(x, ".claude.json"), `{"mcpServers":{"satchel":`+previous+`}}`)
	t.Setenv("CLAUDE_FAIL_ADD", "1")
	opts, _ = initOptions("secret12", "")
	_, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "claude-code", "--verify-user", "admin", "--url", target, "--data-dir", dataDir, "--json")
	if e := decodeError(t, stderr); code != v1.ExitPartialFailure || e.State["previous_registration"] != previous {
		t.Fatalf("应当交还 X/.claude.json 里的登记：%d %+v", code, e)
	}
}

// 用户级登记文件：配置目录下的 .config.json 存在时用它；否则设了 CLAUDE_CONFIG_DIR 是 X/.claude.json，没设是 ~/.claude.json。
func TestClaudeRegistrationPath(t *testing.T) {
	env := testRuntimeEnv(t)
	if got := claudeRegistrationPath(env); got != filepath.Join(env.home, ".claude.json") {
		t.Fatalf("没设：%s", got)
	}
	env.claudeDir, env.claudeSet = t.TempDir(), true
	if got := claudeRegistrationPath(env); got != filepath.Join(env.claudeDir, ".claude.json") {
		t.Fatalf("设了：%s", got)
	}
	put(t, filepath.Join(env.claudeDir, ".config.json"), "{}")
	if got := claudeRegistrationPath(env); got != filepath.Join(env.claudeDir, ".config.json") {
		t.Fatalf(".config.json 存在：%s", got)
	}
}

// master-mcp「CLAUDE_CONFIG_DIR 不可用时不签发」：五次都在问密码之前以 config 停下，点名那个值或文件；没有签发，没有写文件。
func TestMcpInitClaudeConfigDirUnusable(t *testing.T) {
	home, _ := runtimeHome(t)
	dataDir, master := startSocketMaster(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	run := func(label, want, wantNext string, args ...string) {
		t.Helper()
		opts, fp := initOptions("secret12", "")
		_, stderr, code := runWith(t, opts, append(append([]string{"mcp", "init", "--runtime", "claude-code"}, args...), "--data-dir", dataDir, "--json")...)
		e := decodeError(t, stderr)
		if code != v1.ExitFailure || e.Code != v1.CodeConfig || !strings.Contains(e.Reason, want) || !strings.Contains(e.Next, wantNext) {
			t.Fatalf("%s：%d %+v", label, code, e)
		}
		if len(fp.labels) != 0 || master.count() != 0 {
			t.Fatalf("%s：不该问密码、不该签发", label)
		}
		notExist(t, filepath.Join(home, ".claude", "skills"))
		notExist(t, "~")
	}
	t.Setenv(claudeConfigEnv, "~/cfg")
	run("完整接入", `"~/cfg"`, "绝对路径", "--verify-user", "admin")
	run("--print", `"~/cfg"`, "绝对路径", "--print", "--verify-user", "admin")
	run("--skills-only", `"~/cfg"`, "绝对路径", "--skills-only")
	os.Unsetenv(claudeConfigEnv)
	put(t, settings, `{"env":{"CLAUDE_CONFIG_DIR":"/elsewhere"}}`)
	run("settings.json 的 env 里设了", settings, "启动 Claude Code 的环境", "--verify-user", "admin")
	put(t, settings, "{not json")
	run("settings.json 不是 JSON", settings, "修好 "+settings, "--print", "--verify-user", "admin")
	if raw, _ := os.ReadFile(settings); string(raw) != "{not json" {
		t.Fatal("settings.json 不该被改动")
	}
}

// master-mcp「只更新 skills」：不连主控、不签发、不改配置；第二次全部没变；第三次只覆盖改过的那份、不留备份，别的文件还在。
func TestMcpInitSkillsOnly(t *testing.T) {
	home, _ := runtimeHome(t)
	dir := filepath.Join(home, ".hermes", "skills")
	opts, fp := initOptions()
	stdout, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--skills-only", "--data-dir", t.TempDir(), "--json")
	if code != 0 {
		t.Fatalf("--skills-only：%d %s", code, stderr)
	}
	out := decodeSkillsOnly(t, stdout)
	if out.Runtime != "hermes" || out.Skills.Dir != dir || len(out.Skills.Written) != 7 || len(fp.labels) != 0 {
		t.Fatalf("第一次写出七份：%s", stdout)
	}
	wantSkillsIn(t, dir)
	notExist(t, filepath.Join(home, ".hermes", ".env"))
	notExist(t, filepath.Join(home, ".hermes", hermesConfigFile))
	var keys map[string]json.RawMessage
	_ = json.Unmarshal([]byte(stdout), &keys)
	if _, ok := keys["url"]; ok {
		t.Fatalf("输出里不该有主控地址：%s", stdout)
	}
	if _, ok := keys["token"]; ok {
		t.Fatalf("输出里不该有令牌：%s", stdout)
	}
	if notes := strings.Join(out.Notes, "\n"); !strings.Contains(notes, "重启 Hermes 之后生效") || !strings.Contains(notes, "改了会被下次 mcp init 覆盖") {
		t.Fatalf("提示：%v", out.Notes)
	}

	stdout, _, _ = runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--skills-only", "--json")
	if out := decodeSkillsOnly(t, stdout); len(out.Skills.Written) != 0 || len(out.Skills.Unchanged) != 7 {
		t.Fatalf("第二次什么都不写：%s", stdout)
	}

	basics := filepath.Join(dir, "satchel-basics", skills.FileName)
	put(t, basics, "改过\n")
	put(t, filepath.Join(dir, "satchel-basics", "notes.md"), "我的笔记\n")
	stdout, _, _ = runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--skills-only", "--json")
	if out := decodeSkillsOnly(t, stdout); len(out.Skills.Written) != 1 || out.Skills.Written[0] != basics || len(out.Skills.Unchanged) != 6 {
		t.Fatalf("第三次只覆盖改过的那份：%s", stdout)
	}
	wantSkillsIn(t, dir)
	if raw, _ := os.ReadFile(filepath.Join(dir, "satchel-basics", "notes.md")); string(raw) != "我的笔记\n" {
		t.Fatal("同目录里别的文件不该被动")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "satchel-basics"))
	if len(entries) != 2 {
		t.Fatalf("不该留备份或临时文件：%v", entries)
	}

	// 文本形式：有 skills 目录与重启提示。
	stdout, _, _ = runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--skills-only")
	if !strings.Contains(stdout, "skills 目录："+dir) || !strings.Contains(stdout, "重启 Hermes 之后生效") {
		t.Fatalf("文本输出：\n%s", stdout)
	}
}

// master-mcp「Codex 的 skills 目录」：写在 ~/.agents/skills 下，与 CODEX_HOME 无关；~/.agents/skills 是符号链接时写进它指向的目录。
func TestMcpInitCodexSkillsDir(t *testing.T) {
	home, _ := runtimeHome(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	opts, _ := initOptions()
	if _, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--skills-only", "--json"); code != 0 {
		t.Fatalf("--skills-only：%d %s", code, stderr)
	}
	link := filepath.Join(home, ".agents", "skills")
	wantSkillsIn(t, link)
	if entries, _ := os.ReadDir(codexHome); len(entries) != 0 {
		t.Fatalf("CODEX_HOME 里不该有新文件：%v", entries)
	}
	real := t.TempDir()
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--skills-only", "--json"); code != 0 {
		t.Fatalf("skills 目录是符号链接：%d %s", code, stderr)
	}
	wantSkillsIn(t, real)
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("~/.agents/skills 应当仍是符号链接：%v", err)
	}
}

// master-mcp「skills-only 不和别的 flag 混用」：三个都是 usage，没有写任何文件。
func TestMcpInitSkillsOnlyAlone(t *testing.T) {
	home, _ := runtimeHome(t)
	for _, args := range [][]string{
		{"mcp", "init", "--runtime", "codex", "--skills-only", "--print"},
		{"mcp", "init", "--runtime", "codex", "--skills-only", "--use-token"},
		{"--server", "https://panel.example.com", "mcp", "init", "--runtime", "hermes", "--skills-only"},
	} {
		opts, _ := initOptions()
		_, stderr, code := runWith(t, opts, append(args, "--json")...)
		if e := decodeError(t, stderr); code != v1.ExitUsage || e.Code != v1.CodeUsage {
			t.Fatalf("%v：%d %+v", args, code, e)
		}
	}
	notExist(t, filepath.Join(home, ".agents"))
	notExist(t, filepath.Join(home, ".hermes"))
}

// master-mcp「skills 的位置被占用就不签发」：普通文件占着 skills 目录、satchel-basics 是符号链接，都在问密码之前以 config 停下，
// 点名那个路径、请人修好后重跑（不提 --skills-only）；配置与链接指向的文件都没动。
func TestMcpInitSkillsPathTaken(t *testing.T) {
	home, _ := runtimeHome(t)
	dataDir, master := startSocketMaster(t)
	codexCfg := filepath.Join(home, ".codex", "config.toml")
	put(t, codexCfg, "model = \"o5\"\n")
	dir := filepath.Join(home, ".agents", "skills")
	elsewhere := filepath.Join(t.TempDir(), "satchel-basics")
	put(t, filepath.Join(elsewhere, skills.FileName), "别处\n")
	for _, tc := range []struct {
		label string
		setup func() string
	}{
		{"普通文件", func() string { put(t, dir, "x"); return dir }},
		{"符号链接", func() string {
			os.RemoveAll(dir)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "satchel-basics")
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	} {
		label, path := tc.label, tc.setup()
		opts, fp := initOptions("secret12", "")
		_, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--verify-user", "admin", "--data-dir", dataDir, "--json")
		e := decodeError(t, stderr)
		if code != v1.ExitFailure || e.Code != v1.CodeConfig || !strings.Contains(e.Reason, path) || !strings.Contains(e.Next, "修好 "+path) ||
			strings.Contains(e.Next, "--skills-only") || len(fp.labels) != 0 || master.count() != 0 {
			t.Fatalf("%s：%d %+v", label, code, e)
		}
		if raw, _ := os.ReadFile(codexCfg); string(raw) != "model = \"o5\"\n" {
			t.Fatalf("%s：config.toml 不该被改动", label)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(elsewhere, skills.FileName)); string(raw) != "别处\n" {
		t.Fatal("链接指向的文件不该被改动")
	}
}

// master-mcp「写 skills 失败时配置已经写好」：配置写好，partial_failure 不带明文；state 带令牌 id、配置文件与备份、原来令牌的提示；
// next 指向 --skills-only。
func TestMcpInitSkillsWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("靠目录权限制造写失败")
	}
	home, _ := runtimeHome(t)
	dataDir, _ := startSocketMaster(t)
	hermes := filepath.Join(home, ".hermes")
	put(t, filepath.Join(hermes, ".env"), "SATCHEL_TOKEN=sat_old\n")
	dir := filepath.Join(hermes, "skills")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	opts, _ := initOptions("secret12", "")
	_, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--verify-user", "admin", "--url", targetMaster(t), "--data-dir", dataDir, "--json")
	e := decodeError(t, stderr)
	if code != v1.ExitPartialFailure || e.Code != v1.CodePartialFailure || strings.Contains(stderr, issuedToken) {
		t.Fatalf("partial_failure、不带明文：%d %s", code, stderr)
	}
	if raw, _ := os.ReadFile(filepath.Join(hermes, ".env")); !strings.Contains(string(raw), "SATCHEL_TOKEN="+issuedToken) {
		t.Fatalf(".env 应当已经写好：%s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(hermes, hermesConfigFile)); !strings.Contains(string(raw), "mcp_servers") {
		t.Fatalf("config.yaml 应当已经写好：%s", raw)
	}
	files, _ := json.Marshal(e.State["files"])
	notes, _ := json.Marshal(e.State["notes"])
	if e.State["token_id"] != float64(42) || !strings.Contains(string(files), "satchel-bak-") || !strings.Contains(string(notes), "原来有一把别的令牌") ||
		!strings.Contains(e.Next, "satchel mcp init --runtime hermes --skills-only") || e.State["token_name"] == nil {
		t.Fatalf("state 与 next：%+v", e)
	}
	// --use-token 只知道令牌的 id：state 里没有名字与预设，也不是空串。
	srv, _ := fakeMaster(t, map[string]v1.Identity{"sat_mine": tokenIdentity(9, v1.ScopeRead, v1.ScopeOperate)})
	opts, _ = initOptions("sat_mine")
	_, stderr, code = runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--use-token", "--url", srv.URL, "--data-dir", dataDir, "--json")
	e = decodeError(t, stderr)
	_, hasName := e.State["token_name"]
	_, hasPreset := e.State["token_preset"]
	if code != v1.ExitPartialFailure || e.State["token_id"] != float64(9) || hasName || hasPreset {
		t.Fatalf("--use-token 的 state：%d %+v", code, e)
	}
}

// master-mcp「只打印时不写 skills」：PATH 里没有 claude 也照常成功；输出有带明文的片段、skills 该装的目录与 --skills-only 的用法。
func TestMcpInitPrintSkipsSkills(t *testing.T) {
	home, _ := runtimeHome(t)
	t.Setenv("PATH", t.TempDir())
	dataDir, _ := startSocketMaster(t)
	opts, _ := initOptions("secret12", "")
	stdout, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "claude-code", "--print", "--verify-user", "admin", "--data-dir", dataDir, "--json")
	if code != 0 {
		t.Fatalf("--print：%d %s", code, stderr)
	}
	out := decodeInit(t, stdout)
	dir := filepath.Join(home, ".claude", "skills")
	if !strings.Contains(strings.Join(out.Snippet, "\n"), issuedToken) || out.Skills.Dir != dir ||
		!strings.Contains(strings.Join(out.Notes, "\n"), "satchel mcp init --runtime claude-code --skills-only") {
		t.Fatalf("--print 的输出：%s", stdout)
	}
	notExist(t, dir)
}

// skills 目录是指向不存在位置的符号链接、上一级被普通文件占着时，预检点名 skills 目录本身。
func TestPlanSkillsDirEdges(t *testing.T) {
	home := t.TempDir()
	dangling := filepath.Join(home, "skills")
	if err := os.Symlink(filepath.Join(home, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(home, "agents"), "x")
	for _, dir := range []string{dangling, filepath.Join(home, "agents", "skills")} {
		var p errSkillsPath
		if _, _, err := planSkills(dir); !errors.As(err, &p) || p.path != dir {
			t.Fatalf("%s：应当点名 skills 目录本身：%v", dir, err)
		}
	}
}

// 预检之后有人把 satchel-* 目录或其中的 SKILL.md 换成指向别处的符号链接：写 skills 停下，链接指向的文件不被改动。
func TestWriteSkillsDoesNotFollowSwappedLinks(t *testing.T) {
	for _, swapFile := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "skills")
		edits, unchanged, err := planSkills(dir)
		if err != nil || len(edits) != 7 {
			t.Fatalf("预检：%v %d", err, len(edits))
		}
		elsewhere := t.TempDir()
		target := filepath.Join(elsewhere, skills.FileName)
		put(t, target, "别处\n")
		link := filepath.Join(dir, "satchel-basics")
		if swapFile {
			if err := os.MkdirAll(link, 0o700); err != nil {
				t.Fatal(err)
			}
			link = filepath.Join(link, skills.FileName)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Fatal(err)
			}
		}
		res, err := writeSkills(dir, edits, unchanged)
		var p errSkillsPath
		if !errors.As(err, &p) || p.path != link {
			t.Fatalf("换成符号链接（文件 %v）应当停下并点名 %s：%v", swapFile, link, err)
		}
		if raw, _ := os.ReadFile(target); string(raw) != "别处\n" {
			t.Fatalf("链接指向的文件被改了（文件 %v）：%q", swapFile, raw)
		}
		if len(res.Written) != 2 { // access、backup 已经写了，basics 停下
			t.Fatalf("应当写到 basics 之前：%v", res.Written)
		}
	}
}
