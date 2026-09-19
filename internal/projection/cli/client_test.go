package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// shortTempDir 给 unix socket 用：macOS 上 socket 路径不能超过 104 字节。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type captured struct {
	method, path, query, contentType string
	body                             map[string]any
}

// fakeServer 在临时 socket 上起一个假主控：记下请求，按 respond 回复。
func fakeServer(t *testing.T, respond func(w http.ResponseWriter, c captured)) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captured{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type")}
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &c.body)
			}
		}
		respond(w, c)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func testTable(t *testing.T) *command.Table {
	t.Helper()
	tbl, err := command.New(
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
		&command.Command{Path: []string{"audit", "list"}, Summary: "s", Class: command.ClassRead, List: true,
			Flags: []command.Flag{{Name: "actor", Type: command.TypeString}, {Name: "tag", Type: command.TypeStrings}}},
		&command.Command{Path: []string{"explain"}, Summary: "s", Class: command.ClassRead, Offline: true, Args: []command.Arg{{Name: "target", Optional: true}}},
		&command.Command{Path: []string{"demo", "remove"}, Summary: "s", Class: command.ClassAction, Danger: v1.DangerDelete,
			Confirm: &command.Confirm{Kind: command.ConfirmObject, Arg: "name"}, Args: []command.Arg{{Name: "name"}},
			Flags: []command.Flag{{Name: "force", Type: command.TypeBool}, {Name: "wait", Type: command.TypeDuration}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

// master-cli「经主控的命令走本机连接」：GET 的 flag 进查询参数、位置参数进路径、POST 的 flag 与 confirm 进 JSON 体。
func TestClientRequestShape(t *testing.T) {
	var got captured
	sock := fakeServer(t, func(w http.ResponseWriter, c captured) {
		got = c
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"satchel/v1","ok":true}`))
	})
	client := NewClient(testTable(t), sock)
	ctx := context.Background()
	res, err := client.Run(ctx, &command.Invocation{Path: []string{"audit", "list"}, Flags: map[string]any{"actor": "root", "tag": []string{"a", "b"}}, Page: &command.Page{Limit: 5, Cursor: "c1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.method != "GET" || got.path != "/api/v1/audit" || !strings.Contains(got.query, "actor=root") || !strings.Contains(got.query, "tag=a&tag=b") || !strings.Contains(got.query, "limit=5") || !strings.Contains(got.query, "cursor=c1") {
		t.Fatalf("GET 请求形状不对：%+v", got)
	}
	if res.(map[string]any)["ok"] != true {
		t.Fatalf("成功响应应当解成对象：%v", res)
	}
	if _, err := client.Run(ctx, &command.Invocation{Path: []string{"explain"}, Args: []string{"audit list"}}); err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/explain/audit%20list" {
		t.Fatalf("位置参数应当 PathEscape 进路径：%s", got.path)
	}
	if _, err := client.Run(ctx, &command.Invocation{Path: []string{"explain"}}); err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/explain" {
		t.Fatalf("可选参数没给时去掉那一段：%s", got.path)
	}
	if _, err := client.Run(ctx, &command.Invocation{Path: []string{"demo", "remove"}, Args: []string{"alice"}, Flags: map[string]any{"force": true, "wait": mustDuration("30s")}, Confirm: "alice"}); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/api/v1/demo/remove/alice" || !strings.HasPrefix(got.contentType, "application/json") {
		t.Fatalf("POST 请求形状不对：%+v", got)
	}
	if got.body["confirm"] != "alice" || got.body["force"] != true || got.body["wait"] != "30s" {
		t.Fatalf("请求体应当含 flag 与 confirm：%v", got.body)
	}
}

// 服务端的四字段错误原样带回，退出码由错误码折算。
func TestClientPassesErrorsThrough(t *testing.T) {
	sock := fakeServer(t, func(w http.ResponseWriter, c captured) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionRequired)
		_ = json.NewEncoder(w).Encode(v1.New(v1.CodeConfirmRequired, "要确认").WithState("expected", "alice").WithNext("加 --confirm alice"))
	})
	client := NewClient(testTable(t), sock)
	_, err := client.Run(context.Background(), &command.Invocation{Path: []string{"whoami"}})
	e := v1.AsError(err)
	if e.Code != v1.CodeConfirmRequired || e.State["expected"] != "alice" || e.Next != "加 --confirm alice" {
		t.Fatalf("应当原样带回四字段：%+v", e)
	}
	if v1.ExitCodeOf(err) != v1.ExitConfirmRequired {
		t.Fatal("退出码应当是 7")
	}
	// 非 JSON 的错误响应是 internal，不冒充四字段。
	plain := fakeServer(t, func(w http.ResponseWriter, c captured) { http.Error(w, "boom", 502) })
	_, err = NewClient(testTable(t), plain).Run(context.Background(), &command.Invocation{Path: []string{"whoami"}})
	if v1.AsError(err).Code != v1.CodeInternal {
		t.Fatalf("应当 internal：%v", err)
	}
}

func TestClientUnavailable(t *testing.T) {
	dir := shortTempDir(t)
	client := NewClient(testTable(t), filepath.Join(dir, "missing.sock"))
	_, err := client.Run(context.Background(), &command.Invocation{Path: []string{"whoami"}})
	e := v1.AsError(err)
	if e.Code != v1.CodeUnavailable || !strings.Contains(e.Reason, "socket 文件") || e.Next == "" {
		t.Fatalf("没有 socket 文件应当 unavailable：%+v", e)
	}
	// socket 文件在但没人听：先建再关。
	sock := filepath.Join(dir, "dead.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	_, err = NewClient(testTable(t), sock).Run(context.Background(), &command.Invocation{Path: []string{"whoami"}})
	if e := v1.AsError(err); e.Code != v1.CodeUnavailable || !strings.Contains(e.Reason, "没有进程在监听") {
		t.Fatalf("连接被拒应当 unavailable 并说明：%+v", e)
	}
}

func mustDuration(s string) any {
	d, _ := time.ParseDuration(s)
	return d
}
