package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func testTable(t *testing.T) *command.Table {
	t.Helper()
	tbl, err := command.New(
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
		&command.Command{Path: []string{"audit", "list"}, Summary: "s", Class: command.ClassRead, List: true,
			Flags: []command.Flag{{Name: "actor", Type: command.TypeString}, {Name: "tag", Type: command.TypeStrings}, {Name: "n", Type: command.TypeInt}}},
		&command.Command{Path: []string{"explain"}, Summary: "s", Class: command.ClassRead, Args: []command.Arg{{Name: "target", Optional: true}}},
		&command.Command{Path: []string{"demo", "remove"}, Summary: "s", Class: command.ClassAction, Danger: v1.DangerDelete,
			Confirm: &command.Confirm{Kind: command.ConfirmObject, Arg: "name"}, Args: []command.Arg{{Name: "name"}},
			Flags: []command.Flag{{Name: "force", Type: command.TypeBool}, {Name: "spec", Type: command.TypeFile}}},
		&command.Command{Path: []string{"custom"}, Summary: "s", Class: command.ClassRead, REST: &command.REST{Method: "GET", Path: "/api/v1/my/custom"}},
		&command.Command{Path: []string{"version"}, Summary: "s", Class: command.ClassLocal},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

// echo 把收到的 Invocation 原样返回，测试据此断言解码结果。
type echo struct {
	last *command.Invocation
	err  error
	page *command.PageResult
}

func (e *echo) Run(_ context.Context, inv *command.Invocation) (any, error) {
	e.last = inv
	if e.err != nil {
		return nil, e.err
	}
	if e.page != nil && inv.Page != nil {
		return e.page, nil
	}
	return map[string]any{"name": inv.Name(), "args": inv.Args, "flags": inv.Flags, "confirm": inv.Confirm}, nil
}

func do(t *testing.T, h http.Handler, method, path string, body string, header map[string]string) (int, map[string]json.RawMessage) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s %s 的 Content-Type 应当是 JSON，得到 %q", method, path, ct)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("%s %s 的 body 不是 JSON 对象：%s", method, path, rec.Body.String())
	}
	return rec.Code, fields
}

func errorOf(t *testing.T, fields map[string]json.RawMessage) v1.Error {
	t.Helper()
	if len(fields) != 4 {
		t.Fatalf("四字段错误应当恰好四个键：%v", fields)
	}
	raw, _ := json.Marshal(fields)
	var e v1.Error
	_ = json.Unmarshal(raw, &e)
	return e
}

// master-rest-api「路径与方法从命令表推出」「失败响应」。
func TestRoutes(t *testing.T) {
	e := &echo{}
	h := NewHandler(testTable(t), e)
	code, fields := do(t, h, "GET", "/api/v1/whoami", "", nil)
	if code != 200 || string(fields["apiVersion"]) != `"satchel/v1"` || string(fields["name"]) != `"whoami"` {
		t.Fatalf("GET /api/v1/whoami：%d %v", code, fields)
	}
	if code, _ := do(t, h, "GET", "/api/v1/audit?limit=3", "", nil); code != 200 || e.last.Name() != "audit list" || e.last.Page.Limit != 3 {
		t.Fatalf("列表路由去掉 list 段：%d %+v", code, e.last)
	}
	if code, _ := do(t, h, "GET", "/api/v1/explain/audit%20list", "", nil); code != 200 || e.last.Args[0] != "audit list" {
		t.Fatalf("位置参数从路径解码：%d %+v", code, e.last)
	}
	if code, _ := do(t, h, "GET", "/api/v1/explain", "", nil); code != 200 || len(e.last.Args) != 0 {
		t.Fatalf("可选参数可以不给：%d %+v", code, e.last)
	}
	if code, _ := do(t, h, "GET", "/api/v1/my/custom", "", nil); code != 200 || e.last.Name() != "custom" {
		t.Fatalf("显式覆盖的路径：%d %+v", code, e.last)
	}
	for _, p := range []string{"/api/v1/admin/system-settings", "/api/v1/nosuch", "/api/v1/version", "/api/v1/audit/list", "/nosuch"} {
		code, fields := do(t, h, "GET", p, "", nil)
		if code != 404 || errorOf(t, fields).Code != v1.CodeNotFound {
			t.Errorf("%s 应当 404 not_found：%d %v", p, code, fields)
		}
	}
	// 方法不匹配也是 404。
	if code, fields := do(t, h, "POST", "/api/v1/whoami", "{}", map[string]string{"Content-Type": "application/json"}); code != 404 || errorOf(t, fields).Code != v1.CodeNotFound {
		t.Fatalf("POST 到 GET 路由：%d %v", code, fields)
	}
	if code, fields := do(t, h, "GET", "/api/v1/healthz", "", nil); code != 200 || string(fields["status"]) != `"ok"` || string(fields["apiVersion"]) != `"satchel/v1"` {
		t.Fatalf("healthz：%d %v", code, fields)
	}
}

// master-rest-api「请求解码」。
func TestDecodeQuery(t *testing.T) {
	e := &echo{}
	h := NewHandler(testTable(t), e)
	cases := []struct {
		path string
		want string
	}{
		{"/api/v1/audit?limt=10", "limt"},
		{"/api/v1/audit?limit=ten", "ten"},
		{"/api/v1/audit?n=1&n=2", "只能给一次"},
		{"/api/v1/audit?f=/etc/passwd", "文件"},
		{"/api/v1/audit?filename=x", "文件"},
	}
	for _, tc := range cases {
		code, fields := do(t, h, "GET", tc.path, "", nil)
		if code != 400 {
			t.Errorf("%s 应当 400，得到 %d", tc.path, code)
			continue
		}
		if err := errorOf(t, fields); err.Code != v1.CodeBadRequest || !strings.Contains(err.Reason, tc.want) {
			t.Errorf("%s 的错误应当是 bad_request 且含 %q：%+v", tc.path, tc.want, err)
		}
	}
	if code, _ := do(t, h, "GET", "/api/v1/audit?actor=root&tag=a&tag=b&n=7&cursor=c1", "", nil); code != 200 {
		t.Fatal(code)
	}
	if e.last.Flags["actor"] != "root" || e.last.Flags["n"] != 7 || strings.Join(e.last.Flags["tag"].([]string), ",") != "a,b" || e.last.Page.Cursor != "c1" || e.last.Page.Limit != 0 {
		t.Fatalf("查询参数解码不对：%+v %+v", e.last.Flags, e.last.Page)
	}
}

func TestDecodeBody(t *testing.T) {
	e := &echo{}
	h := NewHandler(testTable(t), e)
	js := map[string]string{"Content-Type": "application/json; charset=utf-8"}
	if code, _ := do(t, h, "POST", "/api/v1/demo/remove/alice", `{"force":true,"confirm":"alice"}`, js); code != 200 {
		t.Fatal(code)
	}
	if e.last.Args[0] != "alice" || e.last.Flags["force"] != true || e.last.Confirm != "alice" {
		t.Fatalf("请求体解码不对：%+v", e.last)
	}
	// 非字符串的 confirm 视为缺失，交给 authz 报 confirm_required；这里的假执行器只回显。
	if code, _ := do(t, h, "POST", "/api/v1/demo/remove/alice", `{"confirm":true}`, js); code != 200 || e.last.Confirm != "" {
		t.Fatalf("布尔 confirm 应当视为缺失：%d %q", code, e.last.Confirm)
	}
	cases := []struct {
		body, ct, want string
	}{
		{`{"bogus":1}`, "application/json", "bogus"},
		{`{"force":"yes"}`, "application/json", "force"},
		{`{"spec":"/tmp/x.yaml"}`, "application/json", "文件"},
		{`{"file":"/tmp/x.yaml"}`, "application/json", "文件"},
		{`[1,2]`, "application/json", "JSON 对象"},
		{`{"force":true}`, "text/plain", "application/json"},
		{`{"force":true} {"x":1}`, "application/json", "一个 JSON 对象"},
	}
	for _, tc := range cases {
		code, fields := do(t, h, "POST", "/api/v1/demo/remove/alice", tc.body, map[string]string{"Content-Type": tc.ct})
		if code != 400 {
			t.Errorf("%s 应当 400，得到 %d", tc.body, code)
			continue
		}
		if err := errorOf(t, fields); err.Code != v1.CodeBadRequest || !strings.Contains(err.Reason, tc.want) {
			t.Errorf("%s 的错误应当含 %q：%+v", tc.body, tc.want, err)
		}
	}
	big := `{"force":true,"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	code, fields := do(t, h, "POST", "/api/v1/demo/remove/alice", big, js)
	if code != 400 || !strings.Contains(errorOf(t, fields).Reason, "超过") {
		t.Fatalf("2 MiB 请求体应当 bad_request：%d %v", code, fields)
	}
}

// master-rest-api「失败响应」：状态码按折算表；「成功响应」：列表形状。
func TestStatusCodesAndPage(t *testing.T) {
	for code, status := range map[v1.Code]int{v1.CodeUnauthenticated: 401, v1.CodeForbidden: 403, v1.CodeNotFound: 404, v1.CodeConfirmRequired: 428, v1.CodeVersionConflict: 409, v1.CodeInternal: 500} {
		h := NewHandler(testTable(t), &echo{err: v1.New(code, "x")})
		got, fields := do(t, h, "GET", "/api/v1/whoami", "", nil)
		if got != status || errorOf(t, fields).Code != code {
			t.Errorf("%s 应当 %d，得到 %d %v", code, status, got, fields)
		}
	}
	page := &command.PageResult{Items: []any{map[string]any{"id": 1}}, Total: 120, NextCursor: "c2"}
	h := NewHandler(testTable(t), &echo{page: page})
	code, fields := do(t, h, "GET", "/api/v1/audit", "", nil)
	if code != 200 || string(fields["total"]) != "120" || string(fields["nextCursor"]) != `"c2"` || !bytes.HasPrefix(fields["items"], []byte("[")) || string(fields["apiVersion"]) != `"satchel/v1"` {
		t.Fatalf("列表响应形状：%d %v", code, fields)
	}
}
