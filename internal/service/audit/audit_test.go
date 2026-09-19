package audit

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/audit"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func service(t *testing.T, bdb *bun.DB) *Service {
	t.Helper()
	return New(core.New(bdb, store.New(bdb, schema.Default())))
}

func adminCtx() context.Context {
	return v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
}

// master-audit-log「audit list」与 master-rest-api「列表分页」：120 条翻三页不重不漏。
func TestListPagination(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		ctx := adminCtx()
		for i := 0; i < 120; i++ {
			if err := s.Record(ctx, Entry{Actor: "root", ActorKind: v1.ActorLocalAdmin, Command: "whoami", ArgsDigest: "{}", Result: "ok"}); err != nil {
				t.Fatal(err)
			}
		}
		seen := map[int64]bool{}
		cursor := ""
		pages := 0
		for {
			res, err := s.List(ctx, core.Filter{}, command.Page{Limit: 50, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			pages++
			if res.Total != 120 {
				t.Fatalf("total 应当 120，得到 %d", res.Total)
			}
			for _, item := range res.Items {
				rec := item.(core.Record)
				if seen[rec.ID] {
					t.Fatalf("id %d 重复", rec.ID)
				}
				seen[rec.ID] = true
			}
			if res.NextCursor == "" {
				if len(res.Items) != 20 {
					t.Fatalf("最后一页应当 20 条，得到 %d", len(res.Items))
				}
				break
			}
			if len(res.Items) != 50 {
				t.Fatalf("整页应当 50 条，得到 %d", len(res.Items))
			}
			cursor = res.NextCursor
		}
		if pages != 3 || len(seen) != 120 {
			t.Fatalf("应当三页 120 条，得到 %d 页 %d 条", pages, len(seen))
		}
		res, _ := s.List(ctx, core.Filter{}, command.Page{})
		if len(res.Items) != 50 {
			t.Fatalf("默认页大小 50，得到 %d", len(res.Items))
		}
		for _, bad := range []command.Page{{Limit: 0, Cursor: "x"}, {Limit: 501}, {Limit: -1}, {Limit: 10, Cursor: "garbage"}} {
			if _, err := s.List(ctx, core.Filter{}, bad); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
				t.Errorf("%+v 应当 bad_request：%v", bad, err)
			}
		}
	})
}

func TestListHandler(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		ctx := adminCtx()
		_ = s.Record(ctx, Entry{Actor: "root", ActorKind: v1.ActorLocalAdmin, Command: "whoami", Result: "ok"})
		_ = s.Record(ctx, Entry{Actor: "bob", ActorKind: v1.ActorToken, Command: "audit list", Result: "forbidden"})
		h := s.ListHandler()
		got, err := h(ctx, &command.Invocation{Path: []string{"audit", "list"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		res := got.(*command.PageResult)
		if res.Total != 2 || res.Items[0].(core.Record).Command != "audit list" {
			t.Fatalf("应当倒序两条：%+v", res)
		}
		got, err = h(ctx, &command.Invocation{Path: []string{"audit", "list"}, Flags: map[string]any{"command": "who", "actor": "root"}, Page: &command.Page{Limit: 5}})
		if err != nil || got.(*command.PageResult).Total != 1 {
			t.Fatalf("过滤应当剩 1 条：%v %v", got, err)
		}
		got, err = h(ctx, &command.Invocation{Path: []string{"audit", "list"}, Flags: map[string]any{"since": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}})
		if err != nil || got.(*command.PageResult).Total != 0 || len(got.(*command.PageResult).Items) != 0 {
			t.Fatalf("未来的 since 应当空：%v %v", got, err)
		}
		if _, err := h(ctx, &command.Invocation{Path: []string{"audit", "list"}, Flags: map[string]any{"since": "yesterday"}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("坏的 since 应当 bad_request：%v", err)
		}
		user := v1.WithIdentity(context.Background(), v1.Identity{Actor: "u", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead}})
		if _, err := h(user, &command.Invocation{Path: []string{"audit", "list"}, Flags: map[string]any{}}); err == nil || v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("普通用户应当 forbidden：%v", err)
		}
	})
}
