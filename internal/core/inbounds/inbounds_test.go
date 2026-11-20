package inbounds

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

func seedServer(t *testing.T, bdb *bun.DB, st *store.Store, name string) int64 {
	t.Helper()
	m := &model.Server{Name: name, Token: "tok-" + name, ConnectionMode: "auto"}
	if err := st.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func TestInboundsCRUDAndGuards(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		r := New(bdb, st)
		ctx := context.Background()
		srv1 := seedServer(t, bdb, st, "s1")
		srv2 := seedServer(t, bdb, st, "s2")

		var created *Inbound
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			created, err = r.CreateTx(ctx, tx, v1.InboundSpec{
				ServerID: srv1, Tag: "vless-in", Protocol: "vless", Port: 443, Listen: "::",
				TLS:      v1.SecretJSON(`{"enabled":true,"private_key":"secret-key"}`),
				Settings: v1.SecretJSON(`{"users":[{"name":"u1","uuid":"1111"}]}`),
				Enabled:  true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if created.Metadata.ResourceVersion != 1 || created.Metadata.Name != "1/vless-in" {
			t.Fatalf("created metadata=%+v", created.Metadata)
		}

		// 更新传 "***" 保留原打码值。
		var updated *Inbound
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			updated, err = r.UpdateTx(ctx, tx, created.Metadata.ID, 1, false, v1.InboundSpec{
				ServerID: srv1, Tag: "vless-in", Protocol: "vless", Port: 8443, Listen: "::",
				TLS:      v1.SecretJSON(`"***"`),
				Settings: v1.SecretJSON(`"***"`),
				Enabled:  true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if updated.Spec.Port != 8443 || !json.Valid(updated.Spec.TLS.Reveal()) || string(updated.Spec.TLS.Reveal()) == `"***"` {
			t.Fatalf("应保留原 TLS 与 Settings：tls=%s settings=%s", updated.Spec.TLS.Reveal(), updated.Spec.Settings.Reveal())
		}

		// 禁止跨 server_id 修改。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.UpdateTx(ctx, tx, created.Metadata.ID, 2, false, v1.InboundSpec{
				ServerID: srv2, Tag: "vless-in", Protocol: "vless", Port: 8443,
			})
			return err
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("跨服务器修改应报 bad_request，得到 %v", err)
		}

		// 软删除保留自然键占坑，随后可恢复。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.SoftDeleteTx(ctx, tx, created.Metadata.ID, 2, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.CreateTx(ctx, tx, v1.InboundSpec{ServerID: srv1, Tag: "vless-in", Protocol: "vless", Port: 444})
			return err
		}); v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("软删除期内重名 tag 应报 name_taken，得到 %v", err)
		}
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.RestoreTx(ctx, tx, created.Metadata.ID, 3, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		list, err := r.ListByServer(ctx, srv1)
		if err != nil || len(list) != 1 || list[0].Metadata.ResourceVersion != 4 {
			t.Fatalf("恢复后列出失败：len=%d err=%v", len(list), err)
		}
	})
}
