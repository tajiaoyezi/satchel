package routingrules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestRoutingRulesCRUD(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		r := New(bdb, st)
		ctx := context.Background()
		s1 := &model.Server{Name: "s1", Token: "t1", ConnectionMode: "auto"}
		if err := st.Insert(ctx, s1); err != nil {
			t.Fatal(err)
		}

		var created *RoutingRule
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			created, err = r.CreateTx(ctx, tx, v1.RoutingRuleSpec{
				ServerID: s1.ID, SortOrder: 10, Match: json.RawMessage(`{"domain_suffix":[".cn"]}`),
				OutboundTag: "direct", Enabled: true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.UpdateTx(ctx, tx, created.Metadata.ID, 1, false, v1.RoutingRuleSpec{
				ServerID: s1.ID, SortOrder: 20, Match: json.RawMessage(`{"domain_suffix":[".cn",".local"]}`),
				OutboundTag: "direct", Enabled: true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		list, err := r.ListByServer(ctx, s1.ID)
		if err != nil || len(list) != 1 || list[0].Spec.SortOrder != 20 || list[0].Metadata.ResourceVersion != 2 {
			t.Fatalf("list failed: len=%d err=%v", len(list), err)
		}
	})
}
