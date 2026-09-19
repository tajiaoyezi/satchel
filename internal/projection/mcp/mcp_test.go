package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/middleware/authz"
	"github.com/satchel/satchel/internal/projection/cli"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// testOptions 是主控进程内的装配：目录（可再加测试命令）+ whoami / audit list / explain 的绑定 + authz 链。
func testOptions(t *testing.T, extra ...*command.Command) cli.Options {
	t.Helper()
	cmds := append(command.Catalog().All(), extra...)
	table, err := command.New(cmds...)
	if err != nil {
		t.Fatal(err)
	}
	bindings := command.Bindings{
		"whoami": func(ctx context.Context, _ *command.Invocation) (any, error) { return v1.IdentityFrom(ctx), nil },
		"audit list": func(_ context.Context, inv *command.Invocation) (any, error) {
			return &command.PageResult{Items: []any{}, Total: 0}, nil
		},
		"explain": func(_ context.Context, inv *command.Invocation) (any, error) {
			return command.Explain(table, inv.Arg(0))
		},
	}
	for _, c := range extra {
		name := c.Name()
		bindings[name] = func(context.Context, *command.Invocation) (any, error) { return map[string]any{"ran": name}, nil }
	}
	if err := table.CheckBindings(bindings); err != nil {
		t.Fatal(err)
	}
	runner := authz.Wrap(table, nil, command.Dispatch(bindings))
	opts := cli.DefaultOptions()
	opts.Table = table
	opts.Remote = func(string) command.Runner { return runner }
	opts.Local["serve"] = func(context.Context, *command.Invocation) (any, error) {
		t.Fatal("serve 不该被执行")
		return nil, nil
	}
	return opts
}

// identity 模拟 authn：给每个请求塞一个身份（nil 表示 anonymous）。
func identity(id *v1.Identity, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id != nil {
			r = r.WithContext(v1.WithIdentity(r.Context(), *id))
		}
		next.ServeHTTP(w, r)
	})
}

type session struct {
	t  *testing.T
	cs *sdk.ClientSession
}

func connect(t *testing.T, opts cli.Options, id *v1.Identity) *session {
	t.Helper()
	srv := httptest.NewServer(identity(id, NewHandler(opts)))
	t.Cleanup(srv.Close)
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: srv.URL + Path, HTTPClient: srv.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &session{t: t, cs: cs}
}

func (s *session) run(args []string, confirm string) (text string, isError bool) {
	s.t.Helper()
	params := map[string]any{"args": args}
	if confirm != "" {
		params["confirm"] = confirm
	}
	res, err := s.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "satchel_run", Arguments: params})
	if err != nil {
		s.t.Fatal(err)
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

func (s *session) explain(target string) (text string, isError bool) {
	s.t.Helper()
	res, err := s.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "satchel_explain", Arguments: map[string]any{"target": target}})
	if err != nil {
		s.t.Fatal(err)
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

func decodeErr(t *testing.T, text string) v1.Error {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &keys); err != nil || len(keys) != 4 {
		t.Fatalf("应当是恰好四个键的四字段错误：%s", text)
	}
	var e v1.Error
	_ = json.Unmarshal([]byte(text), &e)
	return e
}

func admin() *v1.Identity {
	id := v1.LocalAdmin("root")
	return &id
}

// master-mcp「服务与两个工具」：工具恰好两个，加命令不变；run 的输出与 CLI 相同；失败是四字段。
func TestToolsAndRun(t *testing.T) {
	s := connect(t, testOptions(t), admin())
	tools, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "satchel_explain,satchel_run" && strings.Join(names, ",") != "satchel_run,satchel_explain" {
		t.Fatalf("工具应当恰好两个：%v", names)
	}
	extra := &command.Command{Path: []string{"demo", "ping"}, Summary: "s", Class: command.ClassRead}
	s2 := connect(t, testOptions(t, extra), admin())
	tools, _ = s2.cs.ListTools(context.Background(), nil)
	if len(tools.Tools) != 2 {
		t.Fatalf("命令表加一条后工具仍应当两个，得到 %d", len(tools.Tools))
	}
	if text, isErr := s2.run([]string{"demo", "ping"}, ""); isErr || !strings.Contains(text, `"ran":"demo ping"`) {
		t.Fatalf("新命令经 satchel_run 可达：%v %s", isErr, text)
	}

	text, isErr := s.run([]string{"whoami"}, "")
	if isErr {
		t.Fatalf("whoami 应当成功：%s", text)
	}
	var viaMCP, viaCLI map[string]json.RawMessage
	_ = json.Unmarshal([]byte(text), &viaMCP)
	var out bytes.Buffer
	opts := testOptions(t)
	if code := cli.ExecuteContext(v1.WithIdentity(context.Background(), *admin()), opts, []string{"whoami", "--json"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	_ = json.Unmarshal(out.Bytes(), &viaCLI)
	if len(viaMCP) != len(viaCLI) || len(viaMCP) == 0 {
		t.Fatalf("MCP 与 CLI 的输出键数不同：%v %v", viaMCP, viaCLI)
	}
	for k, v := range viaCLI {
		if string(viaMCP[k]) != string(v) {
			t.Fatalf("键 %s 不同：%s vs %s", k, viaMCP[k], v)
		}
	}
	if string(viaMCP["actor_kind"]) != `"local_admin"` || string(viaMCP["apiVersion"]) != `"satchel/v1"` {
		t.Fatalf("输出应当是身份对象并带 apiVersion：%s", text)
	}
	text, isErr = s.run([]string{"nosuch"}, "")
	if !isErr || decodeErr(t, text).Code != v1.CodeUsage {
		t.Fatalf("未知命令应当 usage：%v %s", isErr, text)
	}
}

// master-mcp「身份只来自 HTTP 认证」。
func TestIdentityOnlyFromConnection(t *testing.T) {
	s := connect(t, testOptions(t), admin())
	for _, args := range [][]string{{"--token", "abc", "whoami"}, {"whoami", "--token=abc"}, {"--server", "http://x", "whoami"}} {
		text, isErr := s.run(args, "")
		if e := decodeErr(t, text); !isErr || e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "身份") {
			t.Errorf("%v 应当 bad_request 并说明身份只来自连接：%v %s", args, isErr, text)
		}
	}
	anon := connect(t, testOptions(t), nil)
	text, isErr := anon.run([]string{"whoami"}, "")
	if e := decodeErr(t, text); !isErr || e.Code != v1.CodeUnauthenticated {
		t.Fatalf("TCP 上没有身份应当 unauthenticated：%v %s", isErr, text)
	}
}

// master-mcp「解析器约束与拒绝清单」。
func TestParserConstraints(t *testing.T) {
	lock := &command.Command{Path: []string{"demo", "lock"}, Summary: "s", Class: command.ClassAction, HumanOnly: true}
	withFile := &command.Command{Path: []string{"demo", "apply"}, Summary: "s", Class: command.ClassAction, Flags: []command.Flag{{Name: "spec", Type: command.TypeFile}}}
	s := connect(t, testOptions(t, lock, withFile), admin())

	for _, args := range [][]string{{"db", "migrate"}, {"serve"}, {"__verify", "a", "b"}, {"version"}} {
		text, isErr := s.run(args, "")
		if e := decodeErr(t, text); !isErr || e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "本地命令") {
			t.Errorf("%v 应当被拒为本地命令：%v %s", args, isErr, text)
		}
	}
	text, isErr := s.run([]string{"demo", "lock"}, "")
	if e := decodeErr(t, text); !isErr || e.Code != v1.CodeHumanRequired {
		t.Fatalf("人类专属应当 human_required：%v %s", isErr, text)
	}
	for _, args := range [][]string{{"whoami", "-f", "/nonexistent/spec.yaml"}, {"whoami", "--filename", "/x"}, {"whoami", "--file=/x"}, {"demo", "apply", "--spec", "/nonexistent/spec.yaml"}} {
		text, isErr := s.run(args, "")
		e := decodeErr(t, text)
		if !isErr || e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "文件") || strings.Contains(e.Reason, "不存在") {
			t.Errorf("%v 应当被拒为文件路径参数、且没有去打开：%v %s", args, isErr, text)
		}
	}
	for _, args := range [][]string{{"audit"}, {"whoami", "extra"}, {"audit", "list", "--limit", "1", "--limit", "2"}, {"audit", "list", "--bogus"}} {
		text, isErr := s.run(args, "")
		if e := decodeErr(t, text); !isErr || e.Code != v1.CodeUsage {
			t.Errorf("%v 应当 usage：%v %s", args, isErr, text)
		}
	}
	plain, _ := s.run([]string{"whoami"}, "")
	for _, args := range [][]string{{"whoami", "--json"}, {"whoami", "--json=false"}, {"--json", "whoami", "--json"}} {
		text, isErr := s.run(args, "")
		if isErr || text != plain {
			t.Errorf("%v 应当无害：%v %s", args, isErr, text)
		}
	}
}

// confirm 字段经 satchel_run 传到执行链，与 CLI 的 --confirm 是同一个字符串。
func TestConfirmPassthrough(t *testing.T) {
	remove := &command.Command{Path: []string{"demo", "remove"}, Summary: "s", Class: command.ClassAction, Danger: v1.DangerDelete,
		Confirm: &command.Confirm{Kind: command.ConfirmObject, Arg: "name"}, Args: []command.Arg{{Name: "name"}}}
	s := connect(t, testOptions(t, remove), admin())
	text, isErr := s.run([]string{"demo", "remove", "alice"}, "")
	if e := decodeErr(t, text); !isErr || e.Code != v1.CodeConfirmRequired {
		t.Fatalf("缺 confirm：%v %s", isErr, text)
	}
	if text, isErr := s.run([]string{"demo", "remove", "alice"}, "alice"); isErr || !strings.Contains(text, `"ran":"demo remove"`) {
		t.Fatalf("带 confirm 应当执行：%v %s", isErr, text)
	}
	if text, isErr := s.run([]string{"demo", "remove", "alice", "--confirm", "alice"}, ""); isErr {
		t.Fatalf("命令数组里的 --confirm 也认：%s", text)
	}
}

// master-mcp「explain 工具」：三种 target，不需要身份。
func TestExplainTool(t *testing.T) {
	s := connect(t, testOptions(t), nil)
	text, isErr := s.explain("audit list")
	if isErr || !strings.Contains(text, `"path":"/api/v1/audit"`) || !strings.Contains(text, `"list":true`) || !strings.Contains(text, `"limit"`) {
		t.Fatalf("解释命令：%v %s", isErr, text)
	}
	text, isErr = s.explain("Task")
	if isErr || !strings.Contains(text, `"class":"action"`) || !strings.Contains(text, `"name":"title","type":"string"`) || !strings.Contains(text, `"expires_at"`) {
		t.Fatalf("解释 kind：%v %s", isErr, text)
	}
	text, isErr = s.explain("")
	if isErr || !strings.Contains(text, `"commands"`) || !strings.Contains(text, `"kinds"`) {
		t.Fatalf("总览：%v %s", isErr, text)
	}
	text, isErr = s.explain("nosuch")
	if e := decodeErr(t, text); !isErr || e.Code != v1.CodeNotFound {
		t.Fatalf("都不是应当 not_found：%v %s", isErr, text)
	}
}
