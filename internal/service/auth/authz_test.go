package auth

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/middleware/authz"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-identity-and-authz「检查顺序」接上真验证器：人类专属命令在 scope 之前验人；令牌不验直接拒；验过才执行。
func TestVerifierThroughAuthz(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		admin := seedUser(t, bdb, "admin", "admin", true)
		bob := seedUser(t, bdb, "bob", "user", true)
		tbl, err := command.New(
			&command.Command{Path: []string{"demo", "human"}, Summary: "s", Class: command.ClassAction, HumanOnly: true},
			&command.Command{Path: []string{"demo", "plain"}, Summary: "s", Class: command.ClassAction},
		)
		if err != nil {
			t.Fatal(err)
		}
		ran := 0
		r := authz.Wrap(tbl, s.Verifier(), command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) {
			ran++
			return "ok", nil
		}))
		call := func(id v1.Identity, path string, ver *command.Verification) *v1.Error {
			_, err := r.Run(v1.WithIdentity(context.Background(), id), &command.Invocation{Path: []string{"demo", path}, Flags: map[string]any{}, Verify: ver})
			if err == nil {
				return nil
			}
			return v1.AsError(err)
		}
		// 令牌：不看验证值，直接 human_required；普通命令照常按 scope。
		token := v1.Identity{Actor: "t", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, Scopes: []v1.Scope{v1.ScopeOperate}}
		if e := call(token, "human", &command.Verification{Password: password}); e == nil || e.Code != v1.CodeHumanRequired {
			t.Fatalf("令牌调人类专属：%+v", e)
		}
		if e := call(token, "plain", nil); e != nil {
			t.Fatalf("令牌调普通命令：%+v", e)
		}
		// 登录用户：缺密码、密码错都是 human_required 且命令没执行；对了才执行。
		for _, ver := range []*command.Verification{nil, {}, {Password: "wrong"}, {Password: password, User: "bob"}} {
			if e := call(identityFor(admin), "human", ver); e == nil || e.Code != v1.CodeHumanRequired {
				t.Fatalf("验证值 %+v 应当 human_required：%+v", ver, e)
			}
		}
		if ran != 1 {
			t.Fatalf("被拒的调用不该执行命令，执行了 %d 次", ran-1)
		}
		if e := call(identityFor(admin), "human", &command.Verification{Password: password}); e != nil {
			t.Fatalf("验过应当执行：%+v", e)
		}
		if e := call(identityFor(bob), "human", &command.Verification{Password: password}); e != nil {
			t.Fatalf("普通用户验自己也能过人类专属这一步（scope 由类别决定，action 要 operate，普通用户有）：%+v", e)
		}
		// 本机管理员：要 verify-user 指一个管理员；指普通用户被拒。
		root := v1.LocalAdmin("root")
		if e := call(root, "human", &command.Verification{Password: password}); e == nil || e.Code != v1.CodeHumanRequired {
			t.Fatalf("本机管理员缺 verify-user：%+v", e)
		}
		if e := call(root, "human", &command.Verification{Password: password, User: "bob"}); e == nil || e.Code != v1.CodeHumanRequired {
			t.Fatalf("本机管理员指普通用户：%+v", e)
		}
		if e := call(root, "human", &command.Verification{Password: password, User: "admin"}); e != nil {
			t.Fatalf("本机管理员指管理员并给对密码：%+v", e)
		}
		// 人类专属先于 scope：只有 read 的令牌是 human_required，不是 forbidden。
		readOnly := v1.Identity{Actor: "t", ActorKind: v1.ActorToken, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead}}
		if e := call(readOnly, "human", nil); e == nil || e.Code != v1.CodeHumanRequired {
			t.Fatalf("人类专属先于 scope：%+v", e)
		}
		if e := call(readOnly, "plain", nil); e == nil || e.Code != v1.CodeForbidden {
			t.Fatalf("普通命令缺 scope 是 forbidden：%+v", e)
		}
		if ran != 4 {
			t.Fatalf("应当执行 4 次（令牌 plain、admin、bob、root），得到 %d", ran)
		}
	})
}
