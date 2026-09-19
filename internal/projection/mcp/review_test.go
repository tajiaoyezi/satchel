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
	"github.com/satchel/satchel/internal/projection/cli"
	"github.com/satchel/satchel/internal/projection/rest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-mcp「解析器约束与拒绝清单」补的反向用例：cobra 自带命令、-- 之后选不了命令、帮助 flag、客户端专用命令与 flag、-f=。
func TestRejectionListEdges(t *testing.T) {
	s := connect(t, testOptions(t), admin())
	usage := [][]string{
		{}, {"help"}, {"help", "serve"}, {"help", "__verify"}, {"completion", "bash"}, {"__complete", "whoami", ""},
		{"--", "serve"}, {"--", "db", "migrate"}, {"--", "__verify", "a", "b"}, {"nosuch", "--", "x"},
		{"whoami", "--", "--token", "x"}, // -- 之后是位置参数：whoami 没有位置参数，是多余参数的用法错误
	}
	for _, args := range usage {
		text, isErr := s.run(args, "")
		if e := decodeErr(t, text); !isErr || e.Code != v1.CodeUsage {
			t.Errorf("%v 应当 usage：%v %s", args, isErr, text)
		}
	}
	badRequest := map[string][]string{
		"帮助 flag":        {"whoami", "--help"},
		"短帮助 flag":       {"whoami", "-h"},
		"分组的帮助":          {"audit", "--help"},
		"login":          {"login"},
		"mcp init":       {"mcp", "init"},
		"--data-dir":     {"whoami", "--data-dir", "/tmp/x"},
		"--data-dir=":    {"--data-dir=/tmp/x", "whoami"},
		"-f=":            {"whoami", "-f=/etc/passwd"},
		"--file= 在命令前":   {"--file=/x", "whoami"},
		"-- 之前的 --token": {"whoami", "--token", "x", "--", "y"},
	}
	for name, args := range badRequest {
		text, isErr := s.run(args, "")
		if e := decodeErr(t, text); !isErr || e.Code != v1.CodeBadRequest {
			t.Errorf("%s %v 应当 bad_request：%v %s", name, args, isErr, text)
		}
		if strings.Contains(text, "Usage:") || strings.Contains(text, "Available Commands") {
			t.Errorf("%s 不该漏出帮助文本：%s", name, text)
		}
	}
}

// master-mcp「身份只来自 HTTP 认证」：无身份时 explain 也进不去（不论 satchel_run 还是 satchel_explain）。
func TestAnonymousCannotExplain(t *testing.T) {
	anon := connect(t, testOptions(t), nil)
	for _, args := range [][]string{{"explain"}, {"explain", "Task"}, {"audit", "list"}} {
		text, isErr := anon.run(args, "")
		if e := decodeErr(t, text); !isErr || e.Code != v1.CodeUnauthenticated {
			t.Errorf("无身份 satchel_run %v 应当 unauthenticated：%v %s", args, isErr, text)
		}
	}
	text, isErr := anon.explain("Task")
	if e := decodeErr(t, text); !isErr || e.Code != v1.CodeUnauthenticated {
		t.Fatalf("无身份 satchel_explain 应当 unauthenticated：%v %s", isErr, text)
	}
}

// master-identity-and-authz「confirm 是字符串」：MCP 的 confirm 给布尔或数字视为缺失。
func TestNonStringConfirm(t *testing.T) {
	remove := &command.Command{Path: []string{"demo", "remove"}, Summary: "s", Class: command.ClassAction, Danger: v1.DangerDelete,
		Confirm: &command.Confirm{Kind: command.ConfirmObject, Arg: "name"}, Args: []command.Arg{{Name: "name"}}}
	s := connect(t, testOptions(t, remove), admin())
	for _, confirm := range []any{true, 1, []string{"alice"}, map[string]any{"a": 1}} {
		res, err := s.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "satchel_run", Arguments: map[string]any{"args": []string{"demo", "remove", "alice"}, "confirm": confirm}})
		if err != nil {
			t.Fatal(err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		if e := decodeErr(t, text); !res.IsError || e.Code != v1.CodeConfirmRequired {
			t.Errorf("confirm=%v 应当 confirm_required（四字段）：%v %s", confirm, res.IsError, text)
		}
	}
}

// master-command-table「三个投影从表构造」：同一张表加一条命令，CLI、REST、MCP 三处都出现；去掉后三处都没有。
func TestOneTableThreeProjections(t *testing.T) {
	ping := &command.Command{Path: []string{"demo", "ping"}, Summary: "s", Class: command.ClassRead}
	check := func(t *testing.T, opts cli.Options, want bool) {
		t.Helper()
		// CLI
		var out bytes.Buffer
		code := cli.ExecuteContext(v1.WithIdentity(context.Background(), *admin()), opts, []string{"demo", "ping", "--json"}, &out, &out)
		if (code == 0) != want {
			t.Errorf("CLI 里 demo ping 存在=%v，想要 %v：%s", code == 0, want, out.String())
		}
		// REST
		runner := opts.Remote("")
		srv := httptest.NewServer(identity(admin(), rest.NewHandler(opts.Table, runner)))
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/api/v1/demo/ping")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if (resp.StatusCode == 200) != want {
			t.Errorf("REST 里 /api/v1/demo/ping 存在=%v，想要 %v", resp.StatusCode == 200, want)
		}
		// MCP
		s := connect(t, opts, admin())
		text, isErr := s.run([]string{"demo", "ping"}, "")
		if !isErr != want {
			t.Errorf("MCP 里 demo ping 存在=%v，想要 %v：%s", !isErr, want, text)
		}
		if !want {
			if e := decodeErr(t, text); e.Code != v1.CodeUsage {
				t.Errorf("去掉后应当 usage：%+v", e)
			}
		}
	}
	check(t, testOptions(t, ping), true)
	check(t, testOptions(t), false)
	_ = json.Marshal
}
