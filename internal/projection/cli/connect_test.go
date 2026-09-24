package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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

// TestMain 让整个包的测试碰不到开发机上真的登录文件与环境变量：用户配置目录换成临时目录，两个环境变量清掉。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "satchel-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Unsetenv(EnvServer)
	os.Unsetenv(EnvToken)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// isolateHome 给单个测试一个空的用户配置目录。
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func putLogin(t *testing.T, rec loginRecord, perm os.FileMode) string {
	t.Helper()
	path, err := writeLogin(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

type masterLog struct {
	mu    sync.Mutex
	auths []string
	paths []string
}

func (l *masterLog) last() (auth, path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.auths) == 0 {
		return "", ""
	}
	return l.auths[len(l.auths)-1], l.paths[len(l.paths)-1]
}

func (l *masterLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.auths)
}

// fakeMaster 是 TCP 上的假主控：whoami 按 Authorization 头回身份，tokens 里没有的令牌与不带令牌都是 401。
func fakeMaster(t *testing.T, tokens map[string]v1.Identity) (*httptest.Server, *masterLog) {
	t.Helper()
	log := &masterLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.auths = append(log.auths, r.Header.Get("Authorization"))
		log.paths = append(log.paths, r.URL.Path)
		log.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/api/v1/whoami") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(v1.New(v1.CodeNotFound, "没有这个路径"))
			return
		}
		id, ok := tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(v1.New(v1.CodeUnauthenticated, "请求带的凭据无效"))
			return
		}
		_ = json.NewEncoder(w).Encode(id)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func tokenIdentity(id int64, scopes ...v1.Scope) v1.Identity {
	return v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &id, Scopes: scopes, Danger: []v1.Danger{}}
}

func TestNormalizeServer(t *testing.T) {
	good := map[string]string{
		"https://panel.example.com":          "https://panel.example.com",
		"https://panel.example.com/":         "https://panel.example.com",
		"https://panel.example.com/satchel/": "https://panel.example.com/satchel",
		"http://127.0.0.1:12889":             "http://127.0.0.1:12889",
		"http://[::1]:12889/":                "http://[::1]:12889",
	}
	for in, want := range good {
		if got, err := normalizeServer(in); err != nil || got != want {
			t.Errorf("%q 应当归一成 %q，得到 %q %v", in, want, got, err)
		}
	}
	for _, bad := range []string{"", "panel.example.com", "ftp://a.example", "https://", "https://user:pw@a.example", "https://a.example?x=1",
		"https://a.example/#f", "https://a.example:", "https://a.example/?"} {
		if got, err := normalizeServer(bad); err == nil {
			t.Errorf("%q 应当不合形状，得到 %q", bad, got)
		}
	}
}

// master-cli「经主控的命令连本机 socket 或远程主控」：flag、环境变量、登录文件的优先级；登录文件的令牌只发给它自己的 server。
func TestResolveConnection(t *testing.T) {
	isolateHome(t)
	dir := "/data"
	must := func(f connFlags) Connection {
		t.Helper()
		c, err := resolveConnection(f, dir)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := must(connFlags{}); c.Server != "" || c.Token != "" || c.Socket != filepath.Join(dir, db.SocketFile) {
		t.Fatalf("什么都没配就连本机 socket：%+v", c)
	}
	// 本机配了令牌：连 socket 也带令牌。
	t.Setenv(EnvToken, "sat_env")
	if c := must(connFlags{}); c.Server != "" || c.Token != "sat_env" {
		t.Fatalf("本机带环境变量里的令牌：%+v", c)
	}
	t.Setenv(EnvServer, "https://b.example/")
	if c := must(connFlags{}); c.Server != "https://b.example" || c.Token != "sat_env" {
		t.Fatalf("环境变量：%+v", c)
	}
	if c := must(connFlags{server: "https://c.example", serverSet: true, token: "sat_flag", tokenSet: true}); c.Server != "https://c.example" || c.Token != "sat_flag" {
		t.Fatalf("flag 优先于环境变量：%+v", c)
	}
	os.Unsetenv(EnvServer)
	os.Unsetenv(EnvToken)

	// 登录文件记着 A 与令牌 1。
	putLogin(t, loginRecord{Server: "https://a.example", Token: "sat_one"}, 0o600)
	if c := must(connFlags{}); c.Server != "https://a.example" || c.Token != "sat_one" {
		t.Fatalf("登录文件：%+v", c)
	}
	t.Setenv(EnvToken, "sat_two")
	if c := must(connFlags{}); c.Server != "https://a.example" || c.Token != "sat_two" {
		t.Fatalf("环境变量的令牌优先于登录文件，server 仍是 A：%+v", c)
	}
	if c := must(connFlags{token: "sat_three", tokenSet: true}); c.Server != "https://a.example" || c.Token != "sat_three" {
		t.Fatalf("flag 的令牌：%+v", c)
	}
	os.Unsetenv(EnvToken)
	if c := must(connFlags{server: "https://b.example", serverSet: true}); c.Server != "https://b.example" || c.Token != "" {
		t.Fatalf("登录文件的令牌不发给别的 server：%+v", c)
	}
	t.Setenv(EnvServer, "https://b.example")
	if c := must(connFlags{}); c.Server != "https://b.example" || c.Token != "" {
		t.Fatalf("环境变量指向别的 server 时也不带登录文件的令牌：%+v", c)
	}
	os.Unsetenv(EnvServer)
	if c := must(connFlags{server: "https://a.example/", serverSet: true}); c.Server != "https://a.example" || c.Token != "sat_one" {
		t.Fatalf("server 与登录文件的相同（归一后）时带它的令牌：%+v", c)
	}

	// 形状不对：flag 是 usage，环境变量与登录文件是 config。
	wantCode := func(f connFlags, code v1.Code) {
		t.Helper()
		if _, err := resolveConnection(f, dir); err == nil || v1.AsError(err).Code != code {
			t.Fatalf("%+v 应当 %s：%v", f, code, err)
		}
	}
	wantCode(connFlags{server: "panel.example.com", serverSet: true}, v1.CodeUsage)
	wantCode(connFlags{server: "", serverSet: true}, v1.CodeUsage)
	wantCode(connFlags{token: "a b", tokenSet: true}, v1.CodeUsage)
	wantCode(connFlags{token: "", tokenSet: true}, v1.CodeUsage)
	t.Setenv(EnvServer, "ftp://x")
	wantCode(connFlags{}, v1.CodeConfig)
	os.Unsetenv(EnvServer)
	t.Setenv(EnvToken, "has space")
	wantCode(connFlags{}, v1.CodeConfig)
	os.Unsetenv(EnvToken)
	if runtime.GOOS != "windows" {
		path := putLogin(t, loginRecord{Server: "https://a.example", Token: "sat_one"}, 0o644)
		_, err := resolveConnection(connFlags{}, dir)
		if e := v1.AsError(err); err == nil || e.Code != v1.CodeConfig || !strings.Contains(e.Next, "chmod 600") {
			t.Fatalf("登录文件对组与其他用户开放应当 config：%v", err)
		}
		// 显式给全了 server 与令牌就不读登录文件。
		if c := must(connFlags{server: "https://b.example", serverSet: true, token: "sat_x", tokenSet: true}); c.Token != "sat_x" {
			t.Fatalf("%+v", c)
		}
		os.Chmod(path, 0o600)
	}
	path, _ := LoginFile()
	os.WriteFile(path, []byte("{not json"), 0o600)
	wantCode(connFlags{}, v1.CodeConfig)
	os.WriteFile(path, []byte(`{"server":"panel.example.com","token":"sat_one"}`), 0o600)
	wantCode(connFlags{}, v1.CodeConfig)
}

// 远程执行：请求发到 server（含路径前缀）、带令牌；登录文件的令牌不发给别的 server；重定向不跟随。
func TestRemoteCLI(t *testing.T) {
	isolateHome(t)
	srv, log := fakeMaster(t, map[string]v1.Identity{"sat_good": tokenIdentity(7, v1.ScopeRead, v1.ScopeOperate)})
	stdout, stderr, code := run(t, "--server", srv.URL+"/prefix/", "--token", "sat_good", "whoami", "--json")
	if code != 0 {
		t.Fatalf("远程 whoami：%d %s", code, stderr)
	}
	got := decodeJSONObject(t, stdout)
	if string(got["actor_kind"]) != `"token"` || string(got["token_id"]) != "7" {
		t.Fatalf("身份：%s", stdout)
	}
	if auth, path := log.last(); auth != "Bearer sat_good" || path != "/prefix/api/v1/whoami" {
		t.Fatalf("请求：%q %q", auth, path)
	}
	if stderr != "" {
		t.Fatalf("发往回环地址不该有明文 HTTP 的提示：%s", stderr)
	}
	// 环境变量给法。
	t.Setenv(EnvServer, srv.URL)
	t.Setenv(EnvToken, "sat_good")
	if _, stderr, code := run(t, "whoami", "--json"); code != 0 {
		t.Fatalf("环境变量：%d %s", code, stderr)
	}
	os.Unsetenv(EnvServer)
	os.Unsetenv(EnvToken)

	// 登录文件记着别的 server：发往 srv 的请求不带令牌，结果 unauthenticated。
	putLogin(t, loginRecord{Server: "https://a.example", Token: "sat_good"}, 0o600)
	_, stderr, code = run(t, "--server", srv.URL, "whoami", "--json")
	if auth, _ := log.last(); auth != "" || code != v1.ExitUnauthenticated || decodeError(t, stderr).Code != v1.CodeUnauthenticated {
		t.Fatalf("登录文件的令牌不该发给别的 server：%q %d %s", auth, code, stderr)
	}

	// 重定向不跟随，报 config 并说明去向。
	redirect := httptest.NewServer(http.RedirectHandler("https://elsewhere.example/api/v1/whoami", http.StatusMovedPermanently))
	defer redirect.Close()
	_, stderr, code = run(t, "--server", redirect.URL, "--token", "sat_good", "whoami", "--json")
	if e := decodeError(t, stderr); e.Code != v1.CodeConfig || !strings.Contains(e.Reason, "elsewhere.example") || code != v1.ExitCodeOf(v1.New(v1.CodeConfig, "")) {
		t.Fatalf("重定向：%d %+v", code, e)
	}

	// 连不上远程主控：unavailable，next 提示检查地址。
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()
	_, stderr, code = run(t, "--server", dead, "whoami", "--json")
	if e := decodeError(t, stderr); e.Code != v1.CodeUnavailable || code != v1.ExitFailure || !strings.Contains(e.Next, "地址") || !strings.Contains(e.Reason, dead) {
		t.Fatalf("连不上远程：%d %+v", code, e)
	}
	// --server 不合形状：usage，不发请求。
	before := log.count()
	if _, stderr, code := run(t, "--server", "panel.example.com", "whoami", "--json"); code != v1.ExitUsage || decodeError(t, stderr).Code != v1.CodeUsage || log.count() != before {
		t.Fatalf("--server 不合形状：%d %s", code, stderr)
	}
}

func TestPlaintextWarning(t *testing.T) {
	cases := []struct {
		conn Connection
		warn bool
	}{
		{Connection{Server: "http://10.0.0.5:12889", Token: "sat_x"}, true},
		{Connection{Server: "http://panel.example.com/sub", Token: "sat_x"}, true},
		{Connection{Server: "http://10.0.0.5:12889"}, false},
		{Connection{Server: "https://panel.example.com", Token: "sat_x"}, false},
		{Connection{Server: "http://127.0.0.1:12889", Token: "sat_x"}, false},
		{Connection{Server: "http://127.8.0.1:12889", Token: "sat_x"}, false},
		{Connection{Server: "http://[::1]:12889", Token: "sat_x"}, false},
		{Connection{Server: "http://LocalHost:12889", Token: "sat_x"}, false},
		{Connection{Token: "sat_x"}, false},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		warnPlaintext(&buf, tc.conn)
		if got := buf.Len() > 0; got != tc.warn {
			t.Errorf("%+v：提示 %v，想要 %v（%s）", tc.conn, got, tc.warn, buf.String())
		}
		if tc.warn && strings.Count(buf.String(), "\n") != 1 {
			t.Errorf("提示应当恰好一行：%q", buf.String())
		}
	}
	// Connect 解析完连法就提示，写到本次执行的 stderr。
	isolateHome(t)
	var buf bytes.Buffer
	ctx := withStderr(withConnFlags(context.Background(), connFlags{server: "http://10.0.0.5:12889", serverSet: true, token: "sat_x", tokenSet: true}), &buf)
	if _, err := Connect(ctx); err != nil || !strings.Contains(buf.String(), "10.0.0.5:12889") || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("connect 应当提示一行：%v %q", err, buf.String())
	}
}

// master-cli「本地命令不接受 --server 与 --token」：显式给了是 usage，没开库、没问密码；环境变量不算。
func TestLocalCommandsRejectConnectionFlags(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	fp := &fakePrompt{}
	opts := testOptions()
	opts.Prompt = fp.prompt
	for _, args := range [][]string{
		{"--server", "https://panel.example.com", "admin", "reset-password", "admin", "--confirm", "admin"},
		{"--token", "sat_x", "admin", "reset-password", "admin", "--confirm", "admin"},
		{"--server", "https://panel.example.com", "version"},
		{"--server", "https://panel.example.com", "db", "status"},
		{"--token", "sat_x", "logout"},
		{"--server", "https://panel.example.com", "serve"},
	} {
		_, stderr, code := runWith(t, opts, append(args, "--data-dir", dir, "--json")...)
		if e := decodeError(t, stderr); code != v1.ExitUsage || e.Code != v1.CodeUsage || !strings.Contains(e.Reason, "不连主控") {
			t.Fatalf("%v 应当是用法错误：%d %+v", args, code, e)
		}
	}
	if len(fp.labels) != 0 {
		t.Fatalf("不该问任何东西：%v", fp.labels)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("不该碰数据目录：%v", entries)
	}
	t.Setenv(EnvServer, "https://panel.example.com")
	t.Setenv(EnvToken, "sat_x")
	if stdout, stderr, code := run(t, "version"); code != 0 || !strings.Contains(stdout, "satchel") {
		t.Fatalf("环境变量不算显式给出：%d %s", code, stderr)
	}
}

// master-cli「login 与 logout」。
func TestLoginAndLogout(t *testing.T) {
	isolateHome(t)
	srv, log := fakeMaster(t, map[string]v1.Identity{
		"sat_good":  tokenIdentity(7, v1.ScopeRead, v1.ScopeOperate),
		"sat_local": {Actor: "root", ActorKind: v1.ActorLocalAdmin, Role: v1.RoleAdmin},
	})
	path, _ := LoginFile()
	fp := &fakePrompt{answers: []string{"sat_good\n"}}
	opts := testOptions()
	opts.Prompt = fp.prompt

	// 从终端读令牌，验过再写；输出里没有明文。
	stdout, stderr, code := runWith(t, opts, "login", "--server", srv.URL+"/", "--json")
	if code != 0 {
		t.Fatalf("login：%d %s", code, stderr)
	}
	got := decodeJSONObject(t, stdout)
	if strings.Contains(stdout, "sat_good") || string(got["actor_kind"]) != `"token"` || string(got["token_id"]) != "7" || string(got["server"]) != `"`+srv.URL+`"` {
		t.Fatalf("login 输出：%s", stdout)
	}
	if auth, _ := log.last(); auth != "Bearer sat_good" {
		t.Fatalf("login 应当用这把令牌调 whoami：%q", auth)
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("登录文件应当 0600：%v %v", err, info)
	}
	if dirInfo, _ := os.Stat(filepath.Dir(path)); runtime.GOOS != "windows" && dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("目录应当 0700：%v", dirInfo.Mode())
	}
	// 之后的命令经那个 server 带那把令牌。
	if _, stderr, code := run(t, "whoami", "--json"); code != 0 {
		t.Fatalf("登录后 whoami：%d %s", code, stderr)
	}
	if auth, p := log.last(); auth != "Bearer sat_good" || p != "/api/v1/whoami" {
		t.Fatalf("登录后的请求：%q %q", auth, p)
	}
	if stdout, _, _ := runWith(t, opts, "login", "--server", srv.URL, "--token", "sat_good"); !strings.Contains(stdout, "已登录") || strings.Contains(stdout, "sat_good") {
		t.Fatalf("login 文本输出：%s", stdout)
	}

	// 登出：文件不在了，whoami 回到本机 socket（没有主控 → unavailable、说明 socket）。
	stdout, _, code = run(t, "logout", "--json")
	if got := decodeJSONObject(t, stdout); code != 0 || string(got["removed"]) != "true" || !strings.Contains(string(got["note"]), "token revoke") {
		t.Fatalf("logout：%d %s", code, stdout)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("登录文件应当不在了：%v", err)
	}
	_, stderr, _ = run(t, "whoami", "--json", "--data-dir", shortTempDir(t))
	if e := decodeError(t, stderr); e.Code != v1.CodeUnavailable || !strings.Contains(e.Reason, "socket") {
		t.Fatalf("登出后连本机 socket：%+v", e)
	}
	if stdout, _, _ := run(t, "logout"); !strings.Contains(stdout, "没有登录文件") {
		t.Fatalf("再登出：%s", stdout)
	}
}

func TestLoginFailures(t *testing.T) {
	isolateHome(t)
	srv, _ := fakeMaster(t, map[string]v1.Identity{"sat_local": {Actor: "root", ActorKind: v1.ActorLocalAdmin, Role: v1.RoleAdmin}})
	path, _ := LoginFile()
	check := func(name string, wantCode v1.Code, opts Options, args ...string) v1.Error {
		t.Helper()
		_, stderr, code := runWith(t, opts, append(args, "--json")...)
		e := decodeError(t, stderr)
		if e.Code != wantCode || code != v1.ExitCodeOf(&e) {
			t.Fatalf("%s：想要 %s，得到 %d %+v", name, wantCode, code, e)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s：不该写登录文件", name)
		}
		return e
	}
	check("无效令牌", v1.CodeUnauthenticated, testOptions(), "login", "--server", srv.URL, "--token", "sat_bogus")
	check("身份不是令牌", v1.CodeUnauthenticated, testOptions(), "login", "--server", srv.URL, "--token", "sat_local")
	check("没有 server", v1.CodeUsage, testOptions(), "login", "--token", "sat_bogus")
	check("server 不合形状", v1.CodeUsage, testOptions(), "login", "--server", "panel.example.com", "--token", "sat_bogus")
	fp := &fakePrompt{}
	opts := testOptions()
	opts.Prompt = fp.prompt
	e := check("没有终端", v1.CodeBadRequest, opts, "login", "--server", srv.URL)
	if !strings.Contains(e.Reason, "终端") || !strings.Contains(e.Next, EnvServer) || !strings.Contains(e.Next, EnvToken) {
		t.Fatalf("没有终端的提示：%+v", e)
	}
	t.Setenv(EnvServer, srv.URL)
	check("环境变量给 server、令牌无效", v1.CodeUnauthenticated, testOptions(), "login", "--token", "sat_bogus")
}

// master-cli「token 与 mcp 子命令」的帮助：顶层有新命令、全局 flag 有 --server 与 --token（与 command.ClientOnlyFlags 同一份）。
func TestHelpListsRemoteAccess(t *testing.T) {
	root := NewRoot(testOptions())
	for _, name := range command.ClientOnlyFlags {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("根命令缺少全局 flag --%s", name)
		}
	}
	checks := map[string][]string{
		"--help":       {"token", "mcp", "login", "logout", "--server", "--token"},
		"token --help": {"create", "list", "update", "revoke"},
		"mcp --help":   {"stdio", "init", "status"},
	}
	for args, wants := range checks {
		stdout, _, code := run(t, strings.Fields(args)...)
		for _, want := range wants {
			if code != 0 || !strings.Contains(stdout, want) {
				t.Errorf("satchel %s 里应当有 %s：%d\n%s", args, want, code, stdout)
			}
		}
	}
}
