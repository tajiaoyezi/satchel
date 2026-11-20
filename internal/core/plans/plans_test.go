package plans

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestPlansLifecycleAndFiltering(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := New(bdb, store.New(bdb, schema.Default()))
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		p1, err := r.Create(ctx, CreateParams{Objects: json.RawMessage(`[{"op":"create"}]`), Diff: json.RawMessage(`{}`), AffectedCount: 1, ExpiresAt: now.Add(10 * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		p2, err := r.Create(ctx, CreateParams{Objects: json.RawMessage(`[{"op":"update"}]`), Diff: json.RawMessage(`{}`), AffectedCount: 2, ExpiresAt: now.Add(-1 * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if EffectiveStatus(p2, now) != StatusExpired {
			t.Fatalf("p2 应视为 expired，得到 %s", EffectiveStatus(p2, now))
		}
		// 过滤 pending 仅见 p1，过滤 expired 仅见 p2。
		if n, err := r.Count(ctx, StatusPending, now); err != nil || n != 1 {
			t.Fatalf("pending count=%d err=%v", n, err)
		}
		if n, err := r.Count(ctx, StatusExpired, now); err != nil || n != 1 {
			t.Fatalf("expired count=%d err=%v", n, err)
		}
		// 原子消费 p1。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return r.MarkAppliedTx(ctx, tx, p1.Metadata.ID, "apply-001")
		}); err != nil {
			t.Fatal(err)
		}
		// 二次消费报 conflict。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return r.MarkAppliedTx(ctx, tx, p1.Metadata.ID, "apply-002")
		}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("重复 MarkAppliedTx 应报 conflict，得到 %v", err)
		}
	})
}
