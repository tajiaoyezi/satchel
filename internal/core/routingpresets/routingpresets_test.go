package routingpresets

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestRoutingPresetsCRUDAndUnique(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		r := New(bdb, st)
		ctx := context.Background()

		u := &model.User{Username: "admin", PasswordHash: "h", Role: "admin", IsActive: true}
		if err := st.Insert(ctx, u); err != nil {
			t.Fatal(err)
		}

		p, err := r.Save(ctx, "admin", "block-ex", json.RawMessage(`{"domain_suffix":["example.org"]}`))
		if err != nil || p.ID <= 0 {
			t.Fatalf("save failed: %v %+v", err, p)
		}
		// 相同规则（键序/空白不同）应命中唯一约束报 conflict。
		if _, err := r.Save(ctx, "admin", "dup", json.RawMessage(`{ "domain_suffix" : [ "example.org" ] }`)); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("duplicate rule should return conflict, got %v", err)
		}

		items, total, err := r.List(ctx, "admin", 10, 0)
		if err != nil || total != 1 || len(items) != 1 || items[0].Name != "block-ex" {
			t.Fatalf("list mismatch: total=%d items=%+v err=%v", total, items, err)
		}

		if err := r.Delete(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ByID(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound after delete, got %v", err)
		}
		if err := r.Delete(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete should return ErrNotFound, got %v", err)
		}
	})
}
