package snapshots

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
)

func TestSnapshotsAppendAndQuery(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := New(bdb, store.New(bdb, schema.Default()))
		ctx := context.Background()

		var s1, s2 *Snapshot
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			s1, err = r.AppendTx(ctx, tx, AppendInput{
				ObjectKind: "Server", ObjectID: 10, ObjectVersion: 1,
				Content: json.RawMessage(`{"b":2,"a":1}`), ApplyID: "apply-xyz",
			})
			if err != nil {
				return err
			}
			s2, err = r.AppendTx(ctx, tx, AppendInput{
				ObjectKind: "Inbound", ObjectID: 20, ObjectVersion: 3,
				Content: json.RawMessage(`{"tag":"v1"}`), ApplyID: "apply-xyz",
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if s1.ContentHash == "" || len(s1.ContentHash) != 64 {
			t.Fatalf("invalid hash: %q", s1.ContentHash)
		}
		byApply, err := r.ListByApplyID(ctx, "apply-xyz")
		if err != nil || len(byApply) != 2 || byApply[0].ID != s1.ID || byApply[1].ID != s2.ID {
			t.Fatalf("ListByApplyID failed: len=%d err=%v", len(byApply), err)
		}
		list, err := r.List(ctx, Filter{Kind: "Server"}, 0, 10)
		if err != nil || len(list) != 1 || list[0].ID != s1.ID {
			t.Fatalf("List filtered failed: %+v %v", list, err)
		}
	})
}
