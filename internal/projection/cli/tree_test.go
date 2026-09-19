package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-cli「根命令与帮助」：帮助里的顶层子命令集合与命令表一致；树里没有别名。
func TestTreeMatchesTable(t *testing.T) {
	opts := testOptions()
	root := NewRoot(opts)
	var leaves []string
	var walk func(c *cobra.Command, prefix []string)
	walk = func(c *cobra.Command, prefix []string) {
		for _, sub := range c.Commands() {
			if len(sub.Aliases) > 0 {
				t.Errorf("%s 有别名 %v，命令表不允许", sub.CommandPath(), sub.Aliases)
			}
			path := append(append([]string{}, prefix...), sub.Name())
			if name, ok := sub.Annotations[annotationName]; ok {
				leaves = append(leaves, name)
				if name != strings.Join(path, " ") {
					t.Errorf("命令 %s 挂在了 %v 下", name, path)
				}
			}
			walk(sub, path)
		}
	}
	walk(root, nil)
	if strings.Join(leaves, ",") != strings.Join(opts.Table.Names(), ",") {
		t.Fatalf("树里的命令 %v 与表 %v 不一致", leaves, opts.Table.Names())
	}
	stdout, _, code := run(t, "--help")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"whoami", "audit", "explain", "serve", "db", "version"} {
		if !strings.Contains(stdout, "  "+want) {
			t.Errorf("帮助里缺顶层子命令 %s：\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "__verify") {
		t.Error("隐藏命令不该出现在帮助里")
	}
	// 分组节点的说明来自表。
	if !strings.Contains(stdout, "数据库：执行迁移") || !strings.Contains(stdout, "审计记录") {
		t.Errorf("分组说明应当来自命令表：\n%s", stdout)
	}
}

// 分组节点不带子命令、多余片段、重复 flag 都是用法错误（master-mcp「解析器约束」在 CLI 上的同一份）。
func TestUsageFromTree(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"audit"}, "需要一个子命令"},
		{[]string{"audit", "nosuch"}, "没有子命令 nosuch"},
		{[]string{"whoami", "extra"}, "多余的参数 extra"},
		{[]string{"__verify", "onlyone"}, "缺少位置参数 sig"},
		{[]string{"audit", "list", "--limit", "1", "--limit", "2"}, "参数 --limit 重复"},
		{[]string{"audit", "list", "--limit=1", "--limit", "2"}, "参数 --limit 重复"},
		{[]string{"whoami", "--json", "--json"}, "参数 --json 重复"},
		{[]string{"audit", "list", "--bogus"}, "没有参数 --bogus"},
	}
	for _, tc := range cases {
		_, stderr, code := run(t, append(tc.args, "--json")...)
		if code != v1.ExitUsage {
			t.Errorf("satchel %v 应当退出码 2，得到 %d：%s", tc.args, code, stderr)
			continue
		}
		if e := decodeError(t, stderr); e.Code != v1.CodeUsage || !strings.Contains(e.Reason, tc.want) {
			t.Errorf("satchel %v 的 reason 应当含 %q：%+v", tc.args, tc.want, e)
		}
	}
	// -- 之后的片段是位置参数，不算 flag 重复。
	if err := checkDuplicateFlags(NewRoot(testOptions()), testOptions().Table, []string{"whoami", "--", "--json", "--json"}); err != nil {
		t.Fatalf("-- 之后不该查重复：%v", err)
	}
	// 可重复的 strings 类型 flag 出现两次不是错误。
	tbl := command.MustNew(&command.Command{Path: []string{"demo"}, Summary: "s", Class: command.ClassRead, Flags: []command.Flag{{Name: "tag", Type: command.TypeStrings}}})
	if err := checkDuplicateFlags(NewRoot(Options{Table: tbl}), tbl, []string{"demo", "--tag", "a", "--tag", "b"}); err != nil {
		t.Fatalf("strings 类型可以重复：%v", err)
	}
}

// master-cli「whoami、audit list 与 explain 子命令」：explain 不用主控。
func TestExplainOffline(t *testing.T) {
	// 数据目录指到一个空目录：没有 socket，经主控的命令会 unavailable，explain 不该受影响。
	dir := t.TempDir()
	stdout, stderr, code := run(t, "explain", "Task", "--json", "--data-dir", dir)
	if code != 0 {
		t.Fatalf("explain Task 退出码 %d：%s", code, stderr)
	}
	if !strings.Contains(stdout, `"title"`) || !strings.Contains(stdout, `"name":"Task"`) {
		t.Fatalf("explain Task 应当含 spec 字段 title：%s", stdout)
	}
	stdout, _, code = run(t, "explain", "audit list", "--json", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, `"path":"/api/v1/audit"`) {
		t.Fatalf("explain \"audit list\" 应当含 REST 路径：%d %s", code, stdout)
	}
	stdout, _, code = run(t, "explain", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, "whoami") || !strings.Contains(stdout, "Task") {
		t.Fatalf("不带 target 的文本总览应当列命令与 kind：%d\n%s", code, stdout)
	}
	stdout, _, code = run(t, "explain", "audit list", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, "GET /api/v1/audit") || !strings.Contains(stdout, "--limit") {
		t.Fatalf("单条命令的文本说明：%d\n%s", code, stdout)
	}
	stdout, _, code = run(t, "explain", "User", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, "password_hash") || !strings.Contains(stdout, "打码") {
		t.Fatalf("kind 的文本说明应当列字段与标记：%d\n%s", code, stdout)
	}
	_, stderr, code = run(t, "explain", "nosuch", "--json", "--data-dir", dir)
	if code != v1.ExitNotFound {
		t.Fatalf("explain nosuch 应当退出码 5，得到 %d", code)
	}
	if e := decodeError(t, stderr); e.Code != v1.CodeNotFound {
		t.Fatalf("应当 not_found：%+v", e)
	}
}

// master-cli「经主控的命令走本机连接」：主控没起时 unavailable、退出码 1、next 非空。
func TestRemoteWithoutServerIsUnavailable(t *testing.T) {
	dir := shortTempDir(t) // socket 路径在 macOS 上不能超过 104 字节，t.TempDir() 太长
	_, stderr, code := run(t, "whoami", "--json", "--data-dir", dir)
	if code != v1.ExitFailure {
		t.Fatalf("应当退出码 1，得到 %d：%s", code, stderr)
	}
	e := decodeError(t, stderr)
	if e.Code != v1.CodeUnavailable || e.Next == "" || !strings.Contains(e.Reason, "socket") {
		t.Fatalf("应当是 unavailable 并说明 socket：%+v", e)
	}
	_, stderr, code = run(t, "whoami", "--data-dir", dir)
	if code != v1.ExitFailure || !strings.HasPrefix(stderr, "错误 unavailable：") || !strings.Contains(stderr, "下一步：") {
		t.Fatalf("文本形式：\n%s", stderr)
	}
}

// Remote 执行器可替换（MCP 用进程内的链）：服务端错误折成退出码，成功按 --json 或文本渲染。
func TestRemoteRunnerAndExitCodes(t *testing.T) {
	newOpts := func(r command.Runner) Options {
		opts := testOptions()
		opts.Remote = func(string) command.Runner { return r }
		return opts
	}
	errs := map[v1.Code]int{v1.CodeForbidden: 4, v1.CodeNotFound: 5, v1.CodeConfirmRequired: 7, v1.CodeUnauthenticated: 3, v1.CodeHumanRequired: 9, v1.CodeVersionConflict: 6, v1.CodePartialFailure: 8}
	for code, exit := range errs {
		opts := newOpts(command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) { return nil, v1.New(code, "x") }))
		var out, errOut strings.Builder
		if got := Execute(opts, []string{"whoami", "--json"}, &out, &errOut); got != exit {
			t.Errorf("%s 应当退出码 %d，得到 %d", code, exit, got)
		}
		if e := decodeError(t, errOut.String()); e.Code != code {
			t.Errorf("stderr 应当原样带回 %s：%+v", code, e)
		}
	}
	id := v1.LocalAdmin("root")
	opts := newOpts(command.RunnerFunc(func(_ context.Context, inv *command.Invocation) (any, error) {
		if inv.Name() != "whoami" {
			t.Fatalf("应当解析成 whoami：%v", inv.Path)
		}
		return id, nil
	}))
	var out strings.Builder
	if got := Execute(opts, []string{"whoami", "--json"}, &out, &out); got != 0 {
		t.Fatalf("退出码 %d：%s", got, out.String())
	}
	fields := decodeJSONObject(t, out.String())
	if string(fields["actor_kind"]) != `"local_admin"` {
		t.Fatalf("JSON 输出应当是身份对象：%s", out.String())
	}
	out.Reset()
	if got := Execute(opts, []string{"whoami"}, &out, &out); got != 0 || !strings.Contains(out.String(), "actor_kind：local_admin") {
		t.Fatalf("文本形式应当逐行键值：%d\n%s", got, out.String())
	}
	// 列表命令：flag 与分页进 Invocation，文本渲染成表。
	var seen *command.Invocation
	opts = newOpts(command.RunnerFunc(func(_ context.Context, inv *command.Invocation) (any, error) {
		seen = inv
		return &command.PageResult{Items: []any{map[string]any{"id": 2, "at": "t", "actor": "root", "actor_kind": "local_admin", "command": "whoami", "result": "ok", "args_digest": "{}", "token_id": nil, "plan_id": nil}}, Total: 7, NextCursor: "c2"}, nil
	}))
	out.Reset()
	if got := Execute(opts, []string{"audit", "list", "--actor", "root", "--limit", "3"}, &out, &out); got != 0 {
		t.Fatalf("退出码 %d：%s", got, out.String())
	}
	if seen.Flags["actor"] != "root" || seen.Page == nil || seen.Page.Limit != 3 || seen.Name() != "audit list" {
		t.Fatalf("Invocation 不对：%+v %+v", seen, seen.Page)
	}
	text := out.String()
	if !strings.Contains(text, "ID") || !strings.Contains(text, "ACTOR_KIND") || !strings.Contains(text, "local_admin") || !strings.Contains(text, "共 7 条，还有下一页：--cursor c2") {
		t.Fatalf("表格渲染不对：\n%s", text)
	}
	if strings.Contains(text, "args_digest") {
		t.Fatalf("Columns 没登记的列不该出现：\n%s", text)
	}
}

func TestResolve(t *testing.T) {
	opts := testOptions()
	if c, ok := Resolve(opts, []string{"audit", "list", "--limit", "3"}); !ok || c.Name() != "audit list" {
		t.Fatalf("应当解析到 audit list：%v %v", c, ok)
	}
	if c, ok := Resolve(opts, []string{"--json", "db", "migrate"}); !ok || c.Name() != "db migrate" {
		t.Fatalf("flag 在前也能解析：%v %v", c, ok)
	}
	if _, ok := Resolve(opts, []string{"audit"}); ok {
		t.Fatal("分组节点不是命令")
	}
	if _, ok := Resolve(opts, []string{"nosuch"}); ok {
		t.Fatal("未知命令")
	}
}
