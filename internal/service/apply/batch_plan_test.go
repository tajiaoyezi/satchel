package apply

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corebatch "github.com/satchel/satchel/internal/core/batch"
	corebatchin "github.com/satchel/satchel/internal/core/batchinbounds"
	corebatchout "github.com/satchel/satchel/internal/core/batchoutbounds"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	coreplans "github.com/satchel/satchel/internal/core/plans"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	coresnapshots "github.com/satchel/satchel/internal/core/snapshots"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestBatchPlanApplyAtomicity(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		srvRepo := coreservers.New(bdb, st)
		inRepo := coreinbounds.New(bdb, st)
		outRepo := coreoutbounds.New(bdb, st)
		rrRepo := corerouting.New(bdb, st)
		planRepo := coreplans.New(bdb, st)
		snapRepo := coresnapshots.New(bdb, st)
		batchRepo := corebatch.New(bdb, st)
		batchInRepo := corebatchin.New(bdb, st)
		batchOutRepo := corebatchout.New(bdb, st)
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(context.Background()); err != nil {
			t.Fatal(err)
		}
		nodes := newFakeNodes()
		svc := New(Deps{
			DB:             bdb,
			Servers:        srvRepo,
			Inbounds:       inRepo,
			Outbounds:      outRepo,
			RoutingRules:   rrRepo,
			Plans:          planRepo,
			Snapshots:      snapRepo,
			Batch:          batchRepo,
			BatchInbounds:  batchInRepo,
			BatchOutbounds: batchOutRepo,
			Settings:       setRepo,
			Nodes:          nodes,
		})

		ctx := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		s1, err := srvRepo.Create(ctx, "hk-b1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := srvRepo.Create(ctx, "hk-b2", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}

		// 1. 批量入站 plan + apply -> 写入 batch_inbounds
		inOps, _ := json.Marshal([]map[string]any{
			{"op": "create", "kind": "Inbound", "spec": map[string]any{"server_id": s1.Metadata.ID, "tag": "b-vless", "protocol": "vless", "port": 18443, "enabled": true}},
			{"op": "create", "kind": "Inbound", "spec": map[string]any{"server_id": s2.Metadata.ID, "tag": "b-vless", "protocol": "vless", "port": 18443, "enabled": true}},
		})
		recCtx, getRec := command.WithPlanRecorder(ctx)
		rawView, err := svc.CreatePlanFromJSON(recCtx, string(inOps), "Inbound", "batch-in-test")
		if err != nil {
			t.Fatal(err)
		}
		pv := rawView.(*PlanView)
		if pv.BatchID != "batch-in-test" || getRec.Load() != pv.PlanID {
			t.Fatalf("plan batch_id/recorder mismatch: batch=%q rec=%d plan=%d", pv.BatchID, getRec.Load(), pv.PlanID)
		}
		res, err := svc.Bindings()["apply"](ctx, &command.Invocation{
			Path:  []string{"apply"},
			Flags: map[string]any{"plan": int(pv.PlanID)},
		})
		if err != nil {
			t.Fatalf("batch inbound apply failed: %v", err)
		}
		ar := res.(*ApplyResult)
		if ar.BatchID != "batch-in-test" {
			t.Fatalf("apply result missing batch_id: %+v", ar)
		}
		entries, total, err := batchInRepo.List(ctx, "batch-in-test", 0, 10, 0)
		if err != nil || total != 2 || len(entries) != 2 {
			t.Fatalf("batch_inbounds expected 2 entries, got total=%d len=%d err=%v", total, len(entries), err)
		}

		// 2. 批量出站 plan + apply -> 写入 batch_outbounds
		outOps, _ := json.Marshal([]map[string]any{
			{"op": "create", "kind": "Outbound", "spec": map[string]any{"server_id": s1.Metadata.ID, "tag": "b-socks", "protocol": "socks", "settings": map[string]any{"server": "127.0.0.1", "server_port": 1080}}},
			{"op": "create", "kind": "Outbound", "spec": map[string]any{"server_id": s2.Metadata.ID, "tag": "b-socks", "protocol": "socks", "settings": map[string]any{"server": "127.0.0.1", "server_port": 1080}}},
		})
		rawOutView, err := svc.CreatePlanFromJSON(ctx, string(outOps), "Outbound", "batch-out-test")
		if err != nil {
			t.Fatal(err)
		}
		outPV := rawOutView.(*PlanView)
		if _, err := svc.Bindings()["apply"](ctx, &command.Invocation{
			Path:  []string{"apply"},
			Flags: map[string]any{"plan": int(outPV.PlanID)},
		}); err != nil {
			t.Fatalf("batch outbound apply failed: %v", err)
		}
		outEntries, outTotal, err := batchOutRepo.List(ctx, "batch-out-test", 0, 10, 0)
		if err != nil || outTotal != 2 || len(outEntries) != 2 {
			t.Fatalf("batch_outbounds expected 2 entries, got total=%d len=%d err=%v", outTotal, len(outEntries), err)
		}
	})
}
