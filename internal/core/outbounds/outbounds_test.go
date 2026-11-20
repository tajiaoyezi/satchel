package outbounds

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestOutboundsCRUDAndGuards(t *testing.T) {
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

		var created *Outbound
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			created, err = r.CreateTx(ctx, tx, v1.OutboundSpec{
				ServerID: s1.ID, Tag: "proxy-out", Protocol: "shadowsocks",
				Settings: v1.SecretJSON(`{"server":"1.2.3.4","password":"pw"}`),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var updated *Outbound
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			updated, err = r.UpdateTx(ctx, tx, created.Metadata.ID, 1, false, v1.OutboundSpec{
				ServerID: s1.ID, Tag: "proxy-out", Protocol: "shadowsocks",
				Settings: v1.SecretJSON(`"***"`), BalancerGroup: "hk",
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if updated.Spec.BalancerGroup != "hk" || string(updated.Spec.Settings.Reveal()) == `"***"` {
			t.Fatalf("应保留原 Settings：%s", updated.Spec.Settings.Reveal())
		}
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.UpdateTx(ctx, tx, created.Metadata.ID, 2, false, v1.OutboundSpec{ServerID: s2.ID, Tag: "proxy-out", Protocol: "direct"})
			return err
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("跨服务器修改应报 bad_request，得到 %v", err)
		}
	})
}
