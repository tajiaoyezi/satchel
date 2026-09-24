package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

const issuedToken = "sat_issuedissuedissuedissuedissuedissuedissu"

// socketMaster 是数据目录 socket 上的假主控：whoami 回本机管理员，token create 记下请求体、回一把签好的令牌。
type socketMaster struct {
	mu      sync.Mutex
	creates []map[string]any
}

func (m *socketMaster) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.creates)
}

func startSocketMaster(t *testing.T) (dataDir string, m *socketMaster) {
	t.Helper()
	dataDir = shortTempDir(t)
	ln, err := net.Listen("unix", filepath.Join(dataDir, db.SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	m = &socketMaster{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/token/create":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			m.mu.Lock()
			m.creates = append(m.creates, body)
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "name": body["name"], "owner": body["verify-user"], "preset": body["preset"],
				"runtime": body["runtime"], "state": "active", "token": issuedToken})
		case "/api/v1/whoami":
			_ = json.NewEncoder(w).Encode(v1.LocalAdmin("root"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(v1.New(v1.CodeNotFound, "没有这个路径"))
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return dataDir, m
}

// runtimeHome 把 HOME、CODEX_HOME、HERMES_HOME 指到临时目录，PATH 里放一个把参数记进文件的假 claude。
func runtimeHome(t *testing.T) (home, claudeLog string) {
	t.Helper()
	isolateHome(t)
	home = os.Getenv("HOME")
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("HERMES_HOME", filepath.Join(home, ".hermes"))
	bin := filepath.Join(home, "bin")
	claudeLog = filepath.Join(home, "claude.log")
	script := "#!/bin/sh\necho \"$@\" >> " + claudeLog + "\n"
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return home, claudeLog
}

func initOptions(answers ...string) (Options, *fakePrompt) {
	fp := &fakePrompt{answers: answers}
	opts := testOptions()
	opts.Prompt = fp.prompt
	return opts, fp
}

func decodeInit(t *testing.T, stdout string) initOutput {
	t.Helper()
	var out initOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("输出解不开：%v\n%s", err, stdout)
	}
	return out
}

// master-mcp「接入 Claude Code」：本机管理员签 ops 令牌（当场验证经请求体发出），假 claude 先 remove 再 add（参数里没有令牌），
// settings.json 的 env 有三个变量、别的键与顺序不变，旁边有备份。
func TestMcpInitClaudeCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假 claude 是 shell 脚本")
	}
	home, claudeLog := runtimeHome(t)
	dataDir, master := startSocketMaster(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	put(t, settings, `{"theme":"dark","env":{"FOO":"1"},"model":"opus"}`)
	opts, fp := initOptions("secret12", "")
	stdout, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "claude-code", "--verify-user", "admin", "--data-dir", dataDir, "--json")
	if code != 0 {
		t.Fatalf("mcp init：%d %s", code, stderr)
	}
	out := decodeInit(t, stdout)
	host, _ := os.Hostname()
	if master.count() != 1 {
		t.Fatalf("应当签发一次：%d", master.count())
	}
	body := master.creates[0]
	if body["preset"] != "ops" || !strings.HasPrefix(body["name"].(string), "claude-code@") || body["runtime"] != body["name"] ||
		body[command.VerifyUserFlag] != "admin" || body[command.VerifyPasswordFlag] != "secret12" {
		t.Fatalf("token create 的请求：%v", body)
	}
	if host != "" && !strings.Contains(body["name"].(string), strings.Split(host, ".")[0]) {
		t.Fatalf("默认名字应当带主机名：%v", body["name"])
	}
	if len(fp.labels) != 2 || !strings.Contains(fp.labels[0], "admin 的密码") {
		t.Fatalf("应当问密码与验证码：%v", fp.labels)
	}
	if out.URL != "http://127.0.0.1:12889" || !out.Token.Issued || *out.Token.ID != 42 || out.Token.Token != "" || strings.Contains(stdout, issuedToken) {
		t.Fatalf("输出（不含明文）：%s", stdout)
	}
	logged, _ := os.ReadFile(claudeLog)
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if string(logged) != "mcp remove --scope user satchel\nmcp add --scope user satchel -- "+exe+" mcp stdio\n" {
		t.Fatalf("假 claude 收到的参数：\n%s", logged)
	}
	raw, _ := os.ReadFile(settings)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	env := got["env"].(map[string]any)
	if env["SATCHEL_TOKEN"] != issuedToken || env["SATCHEL_SERVER"] != "http://127.0.0.1:12889" || env["SATCHEL_OUTPUT"] != "json" || env["FOO"] != "1" {
		t.Fatalf("settings.json 的 env：%v", env)
	}
	if !strings.HasPrefix(string(raw), "{\n  \"theme\": \"dark\",\n  \"env\"") || !strings.Contains(string(raw), "\"model\": \"opus\"\n}") {
		t.Fatalf("顶层键的顺序应当不变：\n%s", raw)
	}
	if len(out.Files) != 1 || out.Files[0].Backup == "" {
		t.Fatalf("应当有备份：%+v", out.Files)
	}
	if backup, _ := os.ReadFile(out.Files[0].Backup); string(backup) != `{"theme":"dark","env":{"FOO":"1"},"model":"opus"}` {
		t.Fatalf("备份是改前的内容：%s", backup)
	}
	if info, _ := os.Stat(settings); info.Mode().Perm() != 0o640 {
		t.Fatalf("保留原权限：%v", info.Mode())
	}
	if text, _, _ := runWith(t, opts, "mcp", "init", "--runtime", "bogus", "--data-dir", dataDir); text != "" {
		t.Fatalf("不认识的 runtime 不该有输出：%s", text)
	}
}

// master-mcp「接入 Codex 不破坏已有配置」「接入 Hermes」：签发后写文件，新文件 0600、新目录 0700。
func TestMcpInitCodexAndHermes(t *testing.T) {
	home, _ := runtimeHome(t)
	dataDir, master := startSocketMaster(t)
	codexCfg := filepath.Join(home, ".codex", "config.toml")
	put(t, codexCfg, "# mine\n[mcp_servers.docs]\nurl = \"https://docs.example/mcp\"\n\n[shell_environment_policy]\ninherit = \"all\"\n")
	opts, _ := initOptions("secret12", "")
	if _, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--verify-user", "admin", "--url", "https://panel.example.com/", "--data-dir", dataDir); code != 0 {
		t.Fatalf("codex：%d %s", code, stderr)
	}
	raw, _ := os.ReadFile(codexCfg)
	for _, want := range []string{"# mine", "[mcp_servers.docs]", `url = "https://panel.example.com/mcp"`, `Authorization = "Bearer ` + issuedToken + `"`, `SATCHEL_TOKEN = "` + issuedToken + `"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("config.toml 应当有 %q：\n%s", want, raw)
		}
	}
	opts, _ = initOptions("secret12", "")
	stdout, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--verify-user", "admin", "--name", "hermes-box", "--preset", "readonly", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("hermes：%d %s", code, stderr)
	}
	if !strings.Contains(stdout, "已把 hermes 接上主控") || !strings.Contains(stdout, "新建 "+filepath.Join(home, ".hermes", ".env")) || !strings.Contains(stdout, "hermes mcp test satchel") {
		t.Fatalf("文本输出：\n%s", stdout)
	}
	if body := master.creates[1]; body["name"] != "hermes-box" || body["runtime"] != "hermes-box" || body["preset"] != "readonly" {
		t.Fatalf("--name 与 --preset：%v", body)
	}
	envFile := filepath.Join(home, ".hermes", ".env")
	if info, err := os.Stat(envFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf(".env 新建为 0600：%v %v", err, info)
	}
	if info, _ := os.Stat(filepath.Join(home, ".hermes")); info.Mode().Perm() != 0o700 {
		t.Fatalf("新目录 0700：%v", info.Mode())
	}
	if raw, _ := os.ReadFile(filepath.Join(home, ".hermes", hermesConfigFile)); strings.Contains(string(raw), issuedToken) || !strings.Contains(string(raw), "${SATCHEL_TOKEN}") {
		t.Fatalf("config.yaml 里不放明文：\n%s", raw)
	}
}

// master-mcp「解析不了就不签发」「Codex 的 include 过滤先停」：config 错误、没有签发、文件没动、片段打印出来。
func TestMcpInitStopsBeforeIssuing(t *testing.T) {
	home, _ := runtimeHome(t)
	dataDir, master := startSocketMaster(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	put(t, settings, "{not json")
	codexCfg := filepath.Join(home, ".codex", "config.toml")
	put(t, codexCfg, "[shell_environment_policy]\ninclude_only = [\"PATH\", \"HOME\"]\n")
	for _, tc := range []struct{ runtime, file, before, why string }{
		{"claude-code", settings, "{not json", "不是合法的 JSON"},
		{"codex", codexCfg, "[shell_environment_policy]\ninclude_only = [\"PATH\", \"HOME\"]\n", "include_only"},
	} {
		opts, fp := initOptions("secret12", "")
		_, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", tc.runtime, "--verify-user", "admin", "--data-dir", dataDir, "--json")
		e := decodeError(t, stderr)
		if code != v1.ExitFailure || e.Code != v1.CodeConfig || !strings.Contains(e.Reason, tc.why) || e.State["snippet"] == nil {
			t.Fatalf("%s：%d %+v", tc.runtime, code, e)
		}
		if master.count() != 0 || len(fp.labels) != 0 {
			t.Fatalf("%s：停下时不该签发、不该问密码", tc.runtime)
		}
		if raw, _ := os.ReadFile(tc.file); string(raw) != tc.before {
			t.Fatalf("%s：文件不该被改动", tc.runtime)
		}
	}
	// 片段在文本形式里逐行打印。
	opts, _ := initOptions()
	_, stderr, _ := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--verify-user", "admin", "--data-dir", dataDir)
	if !strings.Contains(stderr, "[mcp_servers.satchel]") || !strings.Contains(stderr, shownToken) {
		t.Fatalf("文本形式应当打印片段：\n%s", stderr)
	}
}

// master-mcp「只打印与用已有令牌」「远程令牌身份签不了」。
func TestMcpInitPrintUseTokenAndRemote(t *testing.T) {
	home, _ := runtimeHome(t)
	dataDir, master := startSocketMaster(t)
	opts, _ := initOptions("secret12", "")
	stdout, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--print", "--verify-user", "admin", "--data-dir", dataDir, "--json")
	if code != 0 {
		t.Fatalf("--print：%d %s", code, stderr)
	}
	out := decodeInit(t, stdout)
	if !out.Printed || out.Token.Token != issuedToken || !strings.Contains(strings.Join(out.Snippet, "\n"), "SATCHEL_TOKEN="+issuedToken) || master.count() != 1 {
		t.Fatalf("--print 签出令牌、片段带明文：%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(home, ".hermes")); !os.IsNotExist(err) {
		t.Fatal("--print 不该写任何文件")
	}

	// --use-token：对 --url 调 whoami，身份是令牌才用；不签新的。
	srv, log := fakeMaster(t, map[string]v1.Identity{"sat_mine": tokenIdentity(9, v1.ScopeRead, v1.ScopeOperate)})
	opts, _ = initOptions("sat_mine")
	stdout, stderr, code = runWith(t, opts, "mcp", "init", "--runtime", "hermes", "--use-token", "--url", srv.URL, "--data-dir", dataDir, "--json")
	if code != 0 {
		t.Fatalf("--use-token：%d %s", code, stderr)
	}
	if out := decodeInit(t, stdout); out.Token.Issued || *out.Token.ID != 9 || master.count() != 1 {
		t.Fatalf("--use-token 不签新的：%s", stdout)
	}
	if auth, _ := log.last(); auth != "Bearer sat_mine" {
		t.Fatalf("--use-token 应当用输入的令牌调 whoami：%q", auth)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, ".hermes", ".env")); !strings.Contains(string(raw), "SATCHEL_TOKEN=sat_mine") || !strings.Contains(string(raw), "SATCHEL_SERVER="+srv.URL) {
		t.Fatalf(".env 用的是输入的令牌与 --url：\n%s", raw)
	}
	opts, _ = initOptions("sat_bogus")
	if _, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--use-token", "--url", srv.URL, "--data-dir", dataDir, "--json"); code != v1.ExitUnauthenticated {
		t.Fatalf("无效令牌：%d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatal("无效令牌不该写文件")
	}

	// 远程只有令牌的 CLI：human_required，next 指向 --use-token，没问密码、没改文件。
	opts, fp := initOptions("secret12", "")
	_, stderr, code = runWith(t, opts, "--server", srv.URL, "--token", "sat_mine", "mcp", "init", "--runtime", "codex", "--json")
	if e := decodeError(t, stderr); code != v1.ExitHumanRequired || e.Code != v1.CodeHumanRequired || !strings.Contains(e.Next, "--use-token") || len(fp.labels) != 0 {
		t.Fatalf("远程令牌身份：%d %+v", code, e)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatal("远程令牌身份不该写文件")
	}
	// 本机管理员没给 --verify-user：在问密码之前就停下。
	opts, fp = initOptions("secret12", "")
	if _, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--data-dir", dataDir, "--json"); code != v1.ExitHumanRequired || len(fp.labels) != 0 || !strings.Contains(stderr, "verify-user") {
		t.Fatalf("没给 --verify-user：%d %s", code, stderr)
	}
}

// 写进 runtime 配置的主控地址的三种来源。
func TestMcpInitAddress(t *testing.T) {
	isolateHome(t)
	dataDir, _ := startSocketMaster(t)
	ctx := context.WithValue(context.Background(), dataDirKey{}, dataDir)
	inv := func(flags map[string]any) *command.Invocation { return &command.Invocation{Flags: flags} }
	sock := Connection{Socket: filepath.Join(dataDir, db.SocketFile)}
	if u, err := masterURL(ctx, inv(map[string]any{"url": "https://a.example/sub/"}), sock); err != nil || u != "https://a.example/sub" {
		t.Fatalf("--url：%q %v", u, err)
	}
	if _, err := masterURL(ctx, inv(map[string]any{"url": "a.example"}), sock); v1.AsError(err).Code != v1.CodeUsage {
		t.Fatalf("--url 不合形状：%v", err)
	}
	if u, err := masterURL(ctx, inv(nil), Connection{Server: "https://b.example"}); err != nil || u != "https://b.example" {
		t.Fatalf("CLI 连的 server：%q %v", u, err)
	}
	if u, err := masterURL(ctx, inv(nil), sock); err != nil || u != "http://127.0.0.1:12889" {
		t.Fatalf("默认监听地址推出本机地址：%q %v", u, err)
	}
	put(t, filepath.Join(dataDir, db.ConfigYAMLFile), "listen: \"[::]:23456\"\n")
	if u, err := masterURL(ctx, inv(nil), sock); err != nil || u != "http://127.0.0.1:23456" {
		t.Fatalf(":: 推成 127.0.0.1：%q %v", u, err)
	}
	put(t, filepath.Join(dataDir, db.ConfigYAMLFile), "listen: 192.168.1.5:23456\n")
	if u, err := masterURL(ctx, inv(nil), sock); err != nil || u != "http://192.168.1.5:23456" {
		t.Fatalf("具体地址原样：%q %v", u, err)
	}
	if _, err := masterURL(ctx, inv(nil), Connection{Socket: filepath.Join(t.TempDir(), "none.sock")}); v1.AsError(err).Code != v1.CodeConfig {
		t.Fatalf("没有 socket 也没给地址：%v", err)
	}
}

// 令牌签出来之后写配置失败：partial_failure（退出码 8），带令牌明文与片段。
func TestMcpInitPartialFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("靠目录权限制造写失败")
	}
	home, _ := runtimeHome(t)
	dataDir, master := startSocketMaster(t)
	dir := filepath.Join(home, ".codex")
	put(t, filepath.Join(dir, "config.toml"), "model = \"o5\"\n")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	opts, _ := initOptions("secret12", "")
	_, stderr, code := runWith(t, opts, "mcp", "init", "--runtime", "codex", "--verify-user", "admin", "--data-dir", dataDir, "--json")
	e := decodeError(t, stderr)
	if code != v1.ExitPartialFailure || e.Code != v1.CodePartialFailure || e.State["token"] != issuedToken || e.State["snippet"] == nil || master.count() != 1 {
		t.Fatalf("partial_failure：%d %+v", code, e)
	}
}
