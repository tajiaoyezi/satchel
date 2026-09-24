package mcp

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satchel/satchel/internal/middleware/authn"
	"github.com/satchel/satchel/internal/projection/cli"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// tokenTable 是假的令牌解析器：表里有的令牌解析成对应身份，其余无效。
type tokenTable map[string]v1.Identity

func (t tokenTable) Resolve(_ context.Context, token string) (v1.Identity, bool) {
	id, ok := t[token]
	return id, ok
}

func tokenID(n int64, scopes ...v1.Scope) v1.Identity {
	return v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &n, Scopes: scopes, Danger: []v1.Danger{}}
}

// upstream 起一个带真 authn 的主控 /mcp（令牌走上面的假解析器）。
func upstream(t *testing.T) *httptest.Server {
	t.Helper()
	tokens := tokenTable{
		"sat_ops": tokenID(1, v1.ScopeRead, v1.ScopeOperate),
		"sat_ro":  tokenID(2, v1.ScopeRead),
	}
	srv := httptest.NewServer(authn.Middleware(nil, tokens, NewHandler(testOptions(t))))
	t.Cleanup(srv.Close)
	return srv
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// startShim 在一对管道上起垫片，返回接在另一头的客户端会话与垫片的结束结果。
func startShim(t *testing.T, conn cli.Connection) (*sdk.ClientSession, chan error) {
	t.Helper()
	shimIn, clientOut := io.Pipe()
	clientIn, shimOut := io.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- Stdio(ctx, conn, &sdk.IOTransport{Reader: shimIn, Writer: shimOut}) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "runtime", Version: "0"}, nil).
		Connect(ctx, &sdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		cancel()
		t.Fatalf("接不上垫片：%v（垫片：%v）", err, <-done)
	}
	t.Cleanup(func() {
		cs.Close()
		cancel()
	})
	return cs, done
}

func call(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

// master-mcp「stdio 垫片」：工具清单与主控一致；经垫片调用与直连结果相同；权限由主控判；stdin 结束后正常退出。
func TestStdioShimForwards(t *testing.T) {
	srv := upstream(t)
	cs, done := startShim(t, cli.Connection{Server: srv.URL, Token: "sat_ops"})
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "satchel_explain,satchel_run" {
		t.Fatalf("工具清单应当与主控一致：%v", names)
	}
	viaShim, isErr := call(t, cs, "satchel_run", map[string]any{"args": []string{"whoami"}})
	if isErr || !strings.Contains(viaShim, `"actor_kind":"token"`) {
		t.Fatalf("经垫片 whoami：%v %s", isErr, viaShim)
	}
	direct, err := sdk.NewClient(&sdk.Implementation{Name: "direct", Version: "0"}, nil).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: srv.URL + Path, HTTPClient: cli.Connection{Server: srv.URL, Token: "sat_ops"}.HTTPClient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if text, _ := call(t, direct, "satchel_run", map[string]any{"args": []string{"whoami"}}); text != viaShim {
		t.Fatalf("经垫片与直连应当一样：\n%s\n%s", viaShim, text)
	}
	if text, isErr := call(t, cs, "satchel_explain", map[string]any{"target": "whoami"}); isErr || !strings.Contains(text, "whoami") {
		t.Fatalf("经垫片 explain：%v %s", isErr, text)
	}
	cs.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stdin 结束后垫片应当正常退出：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdin 结束后垫片没有退出")
	}
}

func TestStdioShimPermissionsFromMaster(t *testing.T) {
	srv := upstream(t)
	cs, _ := startShim(t, cli.Connection{Server: srv.URL, Token: "sat_ro"})
	text, isErr := call(t, cs, "satchel_run", map[string]any{"args": []string{"settings", "set", "--set", "heartbeat_interval=45", "--resource-version", "1"}})
	if !isErr || decodeErr(t, text).Code != v1.CodeForbidden {
		t.Fatalf("只读令牌写设置应当 forbidden：%v %s", isErr, text)
	}
}

// 连不上或令牌无效：返回四字段错误（退出码 1 与 3），transport 上什么都没写。
func TestStdioShimStartupFailures(t *testing.T) {
	srv := upstream(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()
	for _, tc := range []struct {
		conn cli.Connection
		code v1.Code
		exit int
	}{
		{cli.Connection{Server: dead, Token: "sat_ops"}, v1.CodeUnavailable, v1.ExitFailure},
		{cli.Connection{Server: srv.URL, Token: "sat_revoked"}, v1.CodeUnauthenticated, v1.ExitUnauthenticated},
		{cli.Connection{Server: srv.URL}, v1.CodeUnauthenticated, v1.ExitUnauthenticated},
	} {
		var out bytes.Buffer
		err := Stdio(context.Background(), tc.conn, &sdk.IOTransport{Reader: io.NopCloser(strings.NewReader("")), Writer: nopWriteCloser{&out}})
		if e := v1.AsError(err); err == nil || e.Code != tc.code || v1.ExitCodeOf(err) != tc.exit || e.Next == "" && tc.code == v1.CodeUnavailable {
			t.Fatalf("%+v 应当 %s（退出码 %d）：%v", tc.conn, tc.code, tc.exit, err)
		}
		if out.Len() != 0 {
			t.Fatalf("启动失败时 stdout 不该有输出：%q", out.String())
		}
	}
}
