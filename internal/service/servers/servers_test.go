package servers

import (
	"context"
	"fmt"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestServiceRequireAdminAndOverview(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		settings := coresettings.New(bdb, st, schema.Default())
		if err := settings.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		repo := core.New(bdb, st)
		svc := New(repo, settings, nil, nil)

		// 非管理员身份（包含普通用户会话与普通用户签发的可操作令牌）调 10 条服务器命令一律返回 forbidden。
		nonAdmins := []struct {
			label string
			id    v1.Identity
		}{
			{"user_session", v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}}},
			{"user_token", v1.Identity{Actor: "bob", ActorKind: v1.ActorToken, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}}},
			{"anonymous", v1.Anonymous()},
		}
		bindings := svc.Bindings()
		for _, caller := range nonAdmins {
			cctx := v1.WithIdentity(ctx, caller.id)
			for cmd, h := range bindings {
				_, err := h(cctx, &command.Invocation{Args: []string{"hk-1"}})
				if e := v1.AsError(err); e == nil || e.Code != v1.CodeForbidden {
					t.Fatalf("%s 调 %s 应当返回 forbidden，得到 %v", caller.label, cmd, err)
				}
			}
		}

		// 空库时管理员看 overview.servers 是空数组与 0。
		adminCtx := v1.WithIdentity(ctx, v1.LocalAdmin("root"))
		items, total, err := svc.Overview(adminCtx, 10)
		if err != nil || total != 0 || len(items) != 0 {
			t.Fatalf("空库 Overview 不对：items=%v total=%d err=%v", items, total, err)
		}

		// 建 12 台服务器：管理员看到按 id 升序前 10 台且 total 为 12，普通用户仍是空数组与 0。
		for i := 1; i <= 12; i++ {
			if _, err := repo.Create(ctx, fmt.Sprintf("srv-%02d", i), ModeWebsocket); err != nil {
				t.Fatal(err)
			}
		}
		items, total, err = svc.Overview(adminCtx, 10)
		if err != nil || total != 12 || len(items) != 10 {
			t.Fatalf("管理员 Overview 应当有 total=12、10 条摘要：items=%d total=%d err=%v", len(items), total, err)
		}
		if first, ok := items[0].(Summary); !ok || first.Name != "srv-01" {
			t.Fatalf("第 1 条应当是 srv-01 Summary：%#v", items[0])
		}
		if last, ok := items[9].(Summary); !ok || last.Name != "srv-10" {
			t.Fatalf("第 10 条应当是 srv-10 Summary：%#v", items[9])
		}

		for _, caller := range nonAdmins {
			cctx := v1.WithIdentity(ctx, caller.id)
			items, total, err := svc.Overview(cctx, 10)
			if err != nil || total != 0 || len(items) != 0 {
				t.Fatalf("%s 的 Overview.servers 应当为空数组与 0：items=%v total=%d err=%v", caller.label, items, total, err)
			}
		}
	})
}
