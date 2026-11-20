package batch

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corebatch "github.com/satchel/satchel/internal/core/batch"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestBatchMiddlewareRollingWindow(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		reg := schema.Default()
		batchRepo := corebatch.New(bdb, st)
		setRepo := coresettings.New(bdb, st, reg)
		if err := setRepo.EnsureSingleton(context.Background()); err != nil {
			t.Fatal(err)
		}
		// 把窗口阈值调成 2，方便测试。
		if _, err := setRepo.Write(context.Background(), coresettings.WriteRequest{
			Values: map[string]any{"batch_window_threshold": int64(2), "batch_single_threshold": int64(5), "batch_window_minutes": int64(15)}, ExpectedVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		tbl, err := command.New(&command.Command{Path: []string{"demo", "kick"}, Summary: "s", Class: command.ClassAction})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		calls := 0
		r := Wrap(batchRepo, setRepo, tbl, map[string]CountFunc{"demo kick": ConstOne}, func() time.Time { return now },
			command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) {
				calls++
				return map[string]any{"ok": true}, nil
			}))

		tokID := int64(77)
		noBatch := v1.WithIdentity(context.Background(), v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokID, Scopes: []v1.Scope{v1.ScopeOperate}})
		withBatch := v1.WithIdentity(context.Background(), v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokID, Scopes: []v1.Scope{v1.ScopeOperate}, Danger: []v1.Danger{v1.DangerBatch}})

		// 前 2 次放行并持久化记账。
		for i := 0; i < 2; i++ {
			if _, err := r.Run(noBatch, &command.Invocation{Path: []string{"demo", "kick"}}); err != nil {
				t.Fatalf("第 %d 次不应被拒：%v", i+1, err)
			}
		}
		// 第 3 次超窗口阈值，无 batch 权限报 forbidden。
		if _, err := r.Run(noBatch, &command.Invocation{Path: []string{"demo", "kick"}}); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("第 3 次无 batch 应报 forbidden，得到 %v", err)
		}
		// 开启 batch 权限后放行。
		if _, err := r.Run(withBatch, &command.Invocation{Path: []string{"demo", "kick"}}); err != nil {
			t.Fatalf("带 batch 权限应当放行：%v", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})
}
