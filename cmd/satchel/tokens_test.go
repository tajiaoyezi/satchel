package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/projection/cli"
	"github.com/satchel/satchel/internal/projection/mcp"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// TestMain 让端到端测试碰不到开发机上真的登录文件与环境变量。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "satchel-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Unsetenv(cli.EnvServer)
	os.Unsetenv(cli.EnvToken)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// cliPrompt 像 h.cli，但终端读取换成按顺序吐出 answers 的假终端。
func (h *harness) cliPrompt(answers []string, args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	opts := options()
	opts.Prompt = func(string) (string, error) {
		if len(answers) == 0 {
			return "", cli.ErrNoTerminal
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
	var out, errOut bytes.Buffer
	code = cli.Execute(opts, append(args, "--data-dir", h.dataDir), &out, &errOut)
	return out.String(), errOut.String(), code
}

type identityJSON struct {
	Actor     string   `json:"actor"`
	ActorKind string   `json:"actor_kind"`
	Role      string   `json:"role"`
	TokenID   *int64   `json:"token_id"`
	Scopes    []string `json:"scopes"`
	Danger    []string `json:"danger"`
}

func decodeInto(t *testing.T, raw string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		t.Fatalf("解不开：%v\n%s", err, raw)
	}
}

func fieldsJSON(fields map[string]json.RawMessage) string {
	raw, _ := json.Marshal(fields)
	return string(raw)
}

// withToken 经 TCP 带令牌发一个请求（新的客户端，不带 cookie）。
func withToken(t *testing.T, method, url, token, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	status, fields, _ := newBrowser(t).call(method, url, body, map[string]string{"Authorization": "Bearer " + token})
	return status, fields
}

// mcpOverTCP 经 TCP 带令牌连主控的 /mcp。
func mcpOverTCP(t *testing.T, base, token string) *sdk.ClientSession {
	t.Helper()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: base + mcp.Path, HTTPClient: cli.Connection{Server: base, Token: token}.HTTPClient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func runTool(t *testing.T, cs *sdk.ClientSession, args ...string) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "satchel_run", Arguments: map[string]any{"args": args}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

type createdToken struct {
	ID     int64  `json:"id"`
	Owner  string `json:"owner"`
	Preset string `json:"preset"`
	Token  string `json:"token"`
	State  string `json:"state"`
}

// API 令牌与远程接入的端到端（双库）：本机管理员签令牌 → TCP 上的 REST 与 MCP 用它 → 改权限立刻生效 → 本机带令牌只有令牌的权限
// → 远程 CLI 的三种给法与 login → mcp stdio 经管道 → settings show 的原文与打码 → 审计的 token_id → 普通用户与上限 → 吊销 → 无效凭据。
func TestTokensEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		isolateHome(t)
		h := start(t, bdb)
		base := h.tcpURL
		alice := newBrowser(t) // setup init 顺手下发管理员的会话
		if status, fields, _ := alice.call("POST", base+"/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil); status != 200 {
			t.Fatalf("setup init：%d %v", status, fields)
		}
		seedUser(t, bdb, "bob", "secret12")

		// 本机管理员经 socket 签一把日常运维令牌：当场验证从终端读，令牌挂在 admin 名下；没有终端是 human_required。
		if _, _, code := h.cliPrompt(nil, "token", "create", "--name", "x", "--verify-user", "admin", "--json"); code != v1.ExitHumanRequired {
			t.Fatalf("没有终端应当 human_required：%d", code)
		}
		if _, stderr, code := h.cliPrompt([]string{"wrong-pw", ""}, "token", "create", "--name", "x", "--verify-user", "admin", "--json"); code != v1.ExitHumanRequired {
			t.Fatalf("密码错应当 human_required：%d %s", code, stderr)
		}
		stdout, stderr, code := h.cliPrompt([]string{"secret12", ""}, "token", "create", "--name", "ops", "--preset", "ops", "--verify-user", "admin", "--json")
		if code != 0 {
			t.Fatalf("token create：%d %s", code, stderr)
		}
		var ops createdToken
		decodeInto(t, stdout, &ops)
		if ops.Owner != "admin" || ops.Preset != "ops" || !strings.HasPrefix(ops.Token, "sat_") || len(ops.Token) != 47 {
			t.Fatalf("签出的令牌：%s", stdout)
		}
		tok := ops.Token

		// TCP 上 REST 用它：令牌身份（master-api-tokens「令牌身份」）。
		status, fields := withToken(t, "GET", base+"/api/v1/whoami", tok, "")
		var id identityJSON
		decodeInto(t, fieldsJSON(fields), &id)
		if status != 200 || id.ActorKind != "token" || id.Actor != "admin" || id.Role != "admin" || id.TokenID == nil || *id.TokenID != ops.ID ||
			strings.Join(id.Scopes, ",") != "read,operate" || len(id.Danger) != 0 {
			t.Fatalf("令牌的 whoami：%d %v", status, fields)
		}

		// 改权限立刻生效：改成只读后同一把令牌只剩 read。
		if _, stderr, code := h.cliPrompt([]string{"secret12", ""}, "token", "update", strconv.FormatInt(ops.ID, 10), "--preset", "readonly", "--verify-user", "admin", "--json"); code != 0 {
			t.Fatalf("token update：%d %s", code, stderr)
		}
		_, fields = withToken(t, "GET", base+"/api/v1/whoami", tok, "")
		if str(fields["actor_kind"]) != "token" || string(fields["scopes"]) != `["read"]` {
			t.Fatalf("改权限应当立刻生效：%v", fields)
		}

		// 本机进程配了只读令牌：经 socket 也只有令牌的权限。
		t.Setenv(cli.EnvToken, tok)
		stdout, _, _ = h.cli("whoami", "--json")
		decodeInto(t, stdout, &id)
		if id.ActorKind != "token" || strings.Join(id.Scopes, ",") != "read" {
			t.Fatalf("本机带令牌：%s", stdout)
		}
		if _, stderr, code := h.cli("settings", "set", "--set", "heartbeat_interval=45", "--resource-version", "1", "--json"); code != v1.ExitForbidden {
			t.Fatalf("只读令牌写设置应当 forbidden：%d %s", code, stderr)
		}
		// 远程 CLI：环境变量给 server 与令牌、--server 加 --token。
		t.Setenv(cli.EnvServer, base)
		if stdout, stderr, code := h.cli("whoami", "--json"); code != 0 || !strings.Contains(stdout, `"actor_kind":"token"`) {
			t.Fatalf("环境变量的远程 CLI：%d %s", code, stderr)
		}
		os.Unsetenv(cli.EnvServer)
		os.Unsetenv(cli.EnvToken)
		if stdout, stderr, code := h.cli("--server", base, "--token", tok, "whoami", "--json"); code != 0 || !strings.Contains(stdout, `"actor_kind":"token"`) || stderr != "" {
			t.Fatalf("--server 加 --token：%d %s", code, stderr)
		}
		// login 后直接用，logout 后回到本机 socket（本机管理员）。
		if _, stderr, code := h.cli("login", "--server", base, "--token", tok, "--json"); code != 0 {
			t.Fatalf("login：%d %s", code, stderr)
		}
		if stdout, _, _ := h.cli("whoami", "--json"); !strings.Contains(stdout, `"actor_kind":"token"`) {
			t.Fatalf("login 后 whoami 经那个主控带那把令牌：%s", stdout)
		}
		h.cli("logout")
		if stdout, _, _ := h.cli("whoami", "--json"); !strings.Contains(stdout, `"actor_kind":"local_admin"`) {
			t.Fatalf("logout 后回到本机 socket：%s", stdout)
		}

		// MCP 经 TCP 带只读令牌：whoami 成功，写设置 forbidden，签令牌 human_required。
		cs := mcpOverTCP(t, base, tok)
		if text, isErr := runTool(t, cs, "whoami"); isErr || !strings.Contains(text, `"actor_kind":"token"`) {
			t.Fatalf("MCP whoami：%v %s", isErr, text)
		}
		if text, _ := runTool(t, cs, "settings", "set", "--set", "heartbeat_interval=45", "--resource-version", "1"); !strings.Contains(text, `"code":"forbidden"`) {
			t.Fatalf("MCP 只读令牌写设置：%s", text)
		}
		if text, _ := runTool(t, cs, "token", "create", "--name", "x"); !strings.Contains(text, `"code":"human_required"`) {
			t.Fatalf("MCP 签令牌：%s", text)
		}
		// 令牌经 REST 也签不了令牌。
		if status, fields := withToken(t, "POST", base+"/api/v1/token/create", tok, `{"name":"x"}`); status != 403 || str(fields["code"]) != "human_required" {
			t.Fatalf("REST 令牌签令牌：%d %v", status, fields)
		}

		// mcp stdio 经管道转发：与直连结果相同。
		shimIn, clientOut := io.Pipe()
		clientIn, shimOut := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go mcp.Stdio(ctx, cli.Connection{Server: base, Token: tok}, &sdk.IOTransport{Reader: shimIn, Writer: shimOut})
		shim, err := sdk.NewClient(&sdk.Implementation{Name: "runtime", Version: "0"}, nil).Connect(ctx, &sdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
		if err != nil {
			t.Fatal(err)
		}
		viaShim, _ := runTool(t, shim, "whoami")
		direct, _ := runTool(t, cs, "whoami")
		if viaShim != direct {
			t.Fatalf("经垫片与直连应当一样：\n%s\n%s", viaShim, direct)
		}
		shim.Close()

		// settings show 的原文与打码：只读令牌看 ***，带 secrets 的令牌与本机管理员看原文。
		secret := strings.Repeat("cd", 32)
		if _, stderr, code := h.cli("settings", "set", "--set", "probe_external_token_sha256="+secret, "--resource-version", "1", "--json"); code != 0 {
			t.Fatalf("设打码字段：%s", stderr)
		}
		stdout, stderr, code = h.cliPrompt([]string{"secret12", ""}, "token", "create", "--name", "reader", "--preset", "full", "--secrets", "--verify-user", "admin", "--json")
		var full createdToken
		decodeInto(t, stdout, &full)
		if code != 0 || full.Preset != "full" {
			t.Fatalf("签带 secrets 的全权令牌：%d %s", code, stderr)
		}
		_, fields = withToken(t, "GET", base+"/api/v1/settings/show", tok, "")
		if !strings.Contains(fieldsJSON(fields), `"probe_external_token_sha256":"***"`) || strings.Contains(fieldsJSON(fields), secret) {
			t.Fatalf("只读令牌应当看到 ***：%v", fields["spec"])
		}
		_, fields = withToken(t, "GET", base+"/api/v1/settings/show", full.Token, "")
		if !strings.Contains(fieldsJSON(fields), `"probe_external_token_sha256":"`+secret+`"`) {
			t.Fatalf("带 secrets 的令牌应当看到原文：%v", fields["spec"])
		}
		if stdout, _, _ := h.cli("settings", "show", "--json"); !strings.Contains(stdout, secret) {
			t.Fatal("本机管理员应当看到原文")
		}

		// 审计：令牌的记录带 token_id 与签发者；审计与列表里都没有令牌明文与哈希。
		stdout, _, _ = h.cli("audit", "list", "--json", "--limit", "100")
		var audits struct {
			Items []map[string]any `json:"items"`
		}
		decodeInto(t, stdout, &audits)
		found := false
		for _, e := range audits.Items {
			if e["actor_kind"] == "token" && e["actor"] == "admin" && e["token_id"] == float64(ops.ID) {
				found = true
			}
		}
		listed, _, _ := h.cli("token", "list", "--json")
		if !found || strings.Contains(stdout, tok) || strings.Contains(listed, tok) || strings.Contains(listed, "token_hash") || strings.Contains(stdout, secret) {
			t.Fatalf("审计与列表：found=%v\n%s\n%s", found, stdout, listed)
		}

		// 普通用户：签全权被拒并点名危险类，签 ops 成功；看不到、吊销不了别人的令牌。
		bob := newBrowser(t)
		if status, _ := bob.login(base, "bob", "secret12", ""); status != 200 {
			t.Fatalf("bob 登录：%d", status)
		}
		status, fields, _ = bob.call("POST", base+"/api/v1/token/create", `{"name":"b","preset":"full","runtime":"codex@bob","verify-password":"secret12"}`, nil)
		if status != 403 || str(fields["code"]) != "forbidden" || !strings.Contains(str(fields["reason"]), "delete") {
			t.Fatalf("bob 签全权：%d %v", status, fields)
		}
		status, fields, _ = bob.call("POST", base+"/api/v1/token/create", `{"name":"b","preset":"ops","runtime":"codex@bob","verify-password":"secret12"}`, nil)
		var bobs createdToken
		decodeInto(t, fieldsJSON(fields), &bobs)
		if status != 200 || bobs.Owner != "bob" {
			t.Fatalf("bob 签 ops：%d %v", status, fields)
		}
		if _, fields := withToken(t, "GET", base+"/api/v1/whoami", bobs.Token, ""); str(fields["role"]) != "user" || string(fields["scopes"]) != `["read","operate"]` {
			t.Fatalf("bob 的令牌：%v", fields)
		}
		// master-web-session「令牌优先于 cookie」：管理员的会话同时带 bob 的令牌，按令牌算。
		if _, fields, _ := alice.call("GET", base+"/api/v1/whoami", "", map[string]string{"Authorization": "Bearer " + bobs.Token}); str(fields["actor_kind"]) != "token" || str(fields["role"]) != "user" {
			t.Fatalf("会话加令牌应当按令牌算：%v", fields)
		}
		status, fields, _ = bob.call("GET", base+"/api/v1/token", "", nil)
		if status != 200 || string(fields["total"]) != "1" {
			t.Fatalf("bob 只看得到自己的：%d %v", status, fields)
		}
		if status, fields, _ := bob.call("POST", base+"/api/v1/token/revoke/"+strconv.FormatInt(ops.ID, 10), `{"verify-password":"secret12"}`, nil); status != 404 || str(fields["code"]) != "not_found" {
			t.Fatalf("bob 吊销别人的：%d %v", status, fields)
		}
		// mcp status：只列绑了 runtime 的（bob 那把），bob 自己也看得到。
		if stdout, _, _ := h.cli("mcp", "status", "--json"); !strings.Contains(stdout, `"runtime":"codex@bob"`) || strings.Contains(stdout, `"name":"ops"`) {
			t.Fatalf("mcp status：%s", stdout)
		}

		// 吊销后立刻失效：401；再吊销是 bad_request；列表里是 revoked。
		if _, stderr, code := h.cliPrompt([]string{"secret12", ""}, "token", "revoke", strconv.FormatInt(ops.ID, 10), "--verify-user", "admin", "--json"); code != 0 {
			t.Fatalf("token revoke：%d %s", code, stderr)
		}
		if status, fields := withToken(t, "GET", base+"/api/v1/whoami", tok, ""); status != 401 || str(fields["code"]) != "unauthenticated" {
			t.Fatalf("吊销后：%d %v", status, fields)
		}
		if _, _, code := h.cliPrompt([]string{"secret12", ""}, "token", "revoke", strconv.FormatInt(ops.ID, 10), "--verify-user", "admin", "--json"); code != v1.ExitFailure {
			t.Fatalf("再吊销应当 bad_request：%d", code)
		}
		if listed, _, _ := h.cli("token", "list", "--json"); !strings.Contains(listed, `"state":"revoked"`) {
			t.Fatalf("列表里应当是 revoked：%s", listed)
		}

		// 无效凭据：连 setup status 也拒且不记审计；socket 上与带会话时都不退回；reason 相同；健康检查照常。
		before := h.auditCount()
		status, invalid := withToken(t, "GET", base+"/api/v1/setup/status", tok, "")
		if status != 401 || str(invalid["code"]) != "unauthenticated" || h.auditCount() != before {
			t.Fatalf("无效令牌调 setup status：%d %v 审计 %d→%d", status, invalid, before, h.auditCount())
		}
		req, _ := http.NewRequest("GET", "http://satchel/api/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer sat_bogus")
		resp, err := h.unixClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var viaSocket map[string]json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&viaSocket)
		resp.Body.Close()
		if resp.StatusCode != 401 || str(viaSocket["reason"]) != str(invalid["reason"]) {
			t.Fatalf("socket 上的无效令牌不退回本机管理员：%d %v", resp.StatusCode, viaSocket)
		}
		if status, fields, _ := bob.call("GET", base+"/api/v1/whoami", "", map[string]string{"Authorization": "Bearer " + tok}); status != 401 || str(fields["reason"]) != str(invalid["reason"]) {
			t.Fatalf("带会话与无效令牌不退回会话：%d %v", status, fields)
		}
		if status, _, _ := newBrowser(t).call("GET", base+"/api/v1/whoami", "", map[string]string{"Authorization": "Basic YWRtaW46eA=="}); status != 401 {
			t.Fatalf("Basic 头：%d", status)
		}
		if status, _ := withToken(t, "GET", base+"/api/v1/healthz", "sat_bogus", ""); status != 200 {
			t.Fatalf("健康检查不看身份：%d", status)
		}
		if h.auditCount() != before {
			t.Fatalf("无效凭据都不记审计：%d→%d", before, h.auditCount())
		}
		// 令牌的最后使用时间在列表里看得到（按分钟节流写）。
		if listed, _, _ := h.cli("token", "list", "--json"); strings.Count(listed, `"last_used_at":null`) == 3 {
			t.Fatalf("用过的令牌应当有 last_used_at：%s", listed)
		}
	})
}
