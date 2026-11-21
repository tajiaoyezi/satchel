package batchoutbounds

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
)

func TestBatchOutboundsInsertAndList(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		r := New(bdb, st)
		ctx := context.Background()

		s1 := &model.Server{Name: "s1", Token: "t1", ConnectionMode: "auto"}
		s2 := &model.Server{Name: "s2", Token: "t2", ConnectionMode: "auto"}
		if err := st.Insert(ctx, s1); err != nil {
			t.Fatal(err)
		}
		if err := st.Insert(ctx, s2); err != nil {
			t.Fatal(err)
		}

		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return r.InsertTx(ctx, tx, []Entry{
				{BatchID: "batch-ob1", Tag: "land-jp", ServerID: s1.ID, Protocol: "socks"},
				{BatchID: "batch-ob1", Tag: "land-jp", ServerID: s2.ID, Protocol: "socks"},
			})
		}); err != nil {
			t.Fatal(err)
		}

		items, total, err := r.List(ctx, "batch-ob1", 0, 10, 0)
		if err != nil || total != 2 || len(items) != 2 {
			t.Fatalf("list batch-ob1 mismatch: total=%d len=%d err=%v", total, len(items), err)
		}
		items2, total2, err := r.List(ctx, "", s1.ID, 10, 0)
		if err != nil || total2 != 1 || len(items2) != 1 || items2[0].Tag != "land-jp" {
			t.Fatalf("list server s1 mismatch: total=%d items=%+v err=%v", total2, items2, err)
		}
	})
}
