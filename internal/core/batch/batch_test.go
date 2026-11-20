package batch

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestBatchWindowCounting(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := New(bdb, store.New(bdb, schema.Default()))
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		tok1 := int64(101)
		tok2 := int64(102)
		idTok1 := v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tok1}
		idTok2 := v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tok2}
		idLocal := v1.LocalAdmin("root")

		if err := r.Record(ctx, idTok1, 3, "apply", now.Add(-20*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := r.Record(ctx, idTok1, 4, "apply", now.Add(-5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := r.Record(ctx, idTok2, 9, "apply", now.Add(-2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := r.Record(ctx, idLocal, 2, "apply", now.Add(-1*time.Minute)); err != nil {
			t.Fatal(err)
		}

		since := now.Add(-15 * time.Minute)
		if got, err := r.WindowUsed(ctx, idTok1, since); err != nil || got != 4 {
			t.Fatalf("tok1 window used = %d (want 4), err=%v", got, err)
		}
		if got, err := r.WindowUsed(ctx, idTok2, since); err != nil || got != 9 {
			t.Fatalf("tok2 window used = %d (want 9), err=%v", got, err)
		}
		if got, err := r.WindowUsed(ctx, idLocal, since); err != nil || got != 2 {
			t.Fatalf("local window used = %d (want 2), err=%v", got, err)
		}
	})
}
