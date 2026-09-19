package authz

import (
	"context"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 测试表：demo remove <name>（删除类、confirm 填对象名）、demo act（动作类）、demo lock（人类专属）、demo bulk（批量类、count）。
func testTable(t *testing.T) *command.Table {
	t.Helper()
	tbl, err := command.New(
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
		&command.Command{Path: []string{"demo", "remove"}, Summary: "s", Class: command.ClassAction, Danger: v1.DangerDelete,
			Confirm: &command.Confirm{Kind: command.ConfirmObject, Arg: "name"}, Args: []command.Arg{{Name: "name"}}},
		&command.Command{Path: []string{"demo", "act"}, Summary: "s", Class: command.ClassAction},
		&command.Command{Path: []string{"demo", "lock"}, Summary: "s", Class: command.ClassAction, HumanOnly: true},
		&command.Command{Path: []string{"demo", "bulk"}, Summary: "s", Class: command.ClassAction, Danger: v1.DangerBatch, Confirm: &command.Confirm{Kind: command.ConfirmCount}},
		&command.Command{Path: []string{"version"}, Summary: "s", Class: command.ClassLocal},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

type calls struct{ n int }

func (c *calls) runner() command.Runner {
	return command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) {
		c.n++
		return map[string]any{"ok": true}, nil
	})
}

func token(scopes []v1.Scope, danger []v1.Danger) v1.Identity {
	return v1.Identity{Actor: "t1", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, Scopes: scopes, Danger: danger}
}

func run(t *testing.T, r command.Runner, id v1.Identity, path []string, args []string, confirm string) *v1.Error {
	t.Helper()
	ctx := v1.WithIdentity(context.Background(), id)
	_, err := r.Run(ctx, &command.Invocation{Path: path, Args: args, Flags: map[string]any{}, Confirm: confirm})
	if err == nil {
		return nil
	}
	return v1.AsError(err)
}

// master-identity-and-authz「检查顺序与错误码」：六步逐一。
func TestOrder(t *testing.T) {
	c := &calls{}
	r := Wrap(testTable(t), nil, c.runner())
	admin := v1.LocalAdmin("root")

	if e := run(t, r, v1.Anonymous(), []string{"whoami"}, nil, ""); e == nil || e.Code != v1.CodeUnauthenticated {
		t.Fatalf("anonymous 应当 unauthenticated：%+v", e)
	}
	// 顺序：anonymous 调危险命令不带 confirm，得到的是 unauthenticated 不是 confirm_required。
	if e := run(t, r, v1.Anonymous(), []string{"demo", "remove"}, []string{"alice"}, ""); e == nil || e.Code != v1.CodeUnauthenticated {
		t.Fatalf("先查身份：%+v", e)
	}
	if e := run(t, r, token([]v1.Scope{v1.ScopeRead}, nil), []string{"demo", "act"}, nil, ""); e == nil || e.Code != v1.CodeForbidden || e.State["required_scope"] != "operate" {
		t.Fatalf("缺 operate 应当 forbidden 并点名 scope：%+v", e)
	}
	if e := run(t, r, token([]v1.Scope{v1.ScopeRead, v1.ScopeOperate}, nil), []string{"demo", "remove"}, []string{"alice"}, "alice"); e == nil || e.Code != v1.CodeForbidden || e.State["required_danger"] != "delete" {
		t.Fatalf("缺危险类应当 forbidden 并点名删除类：%+v", e)
	}
	if e := run(t, r, admin, []string{"demo", "remove"}, []string{"alice"}, ""); e == nil || e.Code != v1.CodeConfirmRequired || e.State["expected"] != "alice" || e.State["kind"] != "object" {
		t.Fatalf("缺 confirm 应当 confirm_required 并给出 expected：%+v", e)
	}
	if e := run(t, r, admin, []string{"demo", "remove"}, []string{"alice"}, "bob"); e == nil || e.Code != v1.CodeConfirmRequired {
		t.Fatalf("confirm 填错应当 confirm_required：%+v", e)
	}
	if c.n != 0 {
		t.Fatalf("以上都不该执行命令，执行了 %d 次", c.n)
	}
	if e := run(t, r, admin, []string{"demo", "remove"}, []string{"alice"}, "alice"); e != nil {
		t.Fatalf("confirm 填对应当执行：%+v", e)
	}
	if e := run(t, r, admin, []string{"whoami"}, nil, ""); e != nil {
		t.Fatalf("读命令应当执行：%+v", e)
	}
	if e := run(t, r, admin, []string{"demo", "bulk"}, nil, ""); e == nil || e.Code != v1.CodeConfirmRequired || e.State["kind"] != "count" {
		t.Fatalf("count 口径缺 confirm：%+v", e)
	}
	if e := run(t, r, admin, []string{"demo", "bulk"}, nil, "12"); e != nil {
		t.Fatalf("count 口径带 confirm 应当放行：%+v", e)
	}
	if c.n != 3 {
		t.Fatalf("应当恰好执行 3 次，得到 %d", c.n)
	}
}

func TestHumanOnly(t *testing.T) {
	c := &calls{}
	r := Wrap(testTable(t), nil, c.runner())
	if e := run(t, r, token(v1.AllScopes, v1.AllDangers), []string{"demo", "lock"}, nil, ""); e == nil || e.Code != v1.CodeHumanRequired {
		t.Fatalf("令牌调人类专属应当 human_required：%+v", e)
	}
	if e := run(t, r, v1.LocalAdmin("root"), []string{"demo", "lock"}, nil, ""); e == nil || e.Code != v1.CodeHumanRequired {
		t.Fatalf("没有当场验证入口时本机管理员也拿不到：%+v", e)
	}
	if c.n != 0 {
		t.Fatal("不该执行")
	}
	// 装了验证器：验证通过才执行，失败原样返回。
	ok := Wrap(testTable(t), verifierFunc(func(context.Context, *command.Invocation) error { return nil }), c.runner())
	if e := run(t, ok, v1.LocalAdmin("root"), []string{"demo", "lock"}, nil, ""); e != nil || c.n != 1 {
		t.Fatalf("验证通过应当执行：%+v", e)
	}
	bad := Wrap(testTable(t), verifierFunc(func(context.Context, *command.Invocation) error { return v1.New(v1.CodeHumanRequired, "密码不对") }), c.runner())
	if e := run(t, bad, v1.LocalAdmin("root"), []string{"demo", "lock"}, nil, ""); e == nil || e.Reason != "密码不对" || c.n != 1 {
		t.Fatalf("验证失败应当原样返回：%+v", e)
	}
}

type verifierFunc func(context.Context, *command.Invocation) error

func (f verifierFunc) Verify(ctx context.Context, inv *command.Invocation) error { return f(ctx, inv) }

func TestLocalAndUnknownRejected(t *testing.T) {
	r := Wrap(testTable(t), nil, (&calls{}).runner())
	if e := run(t, r, v1.LocalAdmin("root"), []string{"version"}, nil, ""); e == nil || e.Code != v1.CodeBadRequest {
		t.Fatalf("本地命令不经主控：%+v", e)
	}
	if e := run(t, r, v1.LocalAdmin("root"), []string{"nosuch"}, nil, ""); e == nil || e.Code != v1.CodeNotFound {
		t.Fatalf("表外命令 not_found：%+v", e)
	}
}
