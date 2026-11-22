package returnroutes

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func insertTestServer(t *testing.T, bdb *bun.DB, st *store.Store, name string) int64 {
	t.Helper()
	m := &model.Server{
		Name:           name,
		ConnectionMode: "auto",
		Token:          "tok-" + name,
		AgentToken:     "ag-" + name,
		PullToken:      "pl-" + name,
	}
	if err := st.Insert(context.Background(), m); err != nil {
		t.Fatalf("insert test server %s failed: %v", name, err)
	}
	return m.ID
}

func TestReturnRoutesCRUDAndProbeStatus(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		repo := New(bdb, st)

		s1 := insertTestServer(t, bdb, st, "rr-s1")
		s2 := insertTestServer(t, bdb, st, "rr-s2")

		// 1. 合法创建：小写 ct 规范化为 CT，probe 为空归一为 {}。
		var created *ReturnRoute
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			created, err = repo.CreateTx(ctx, tx, v1.ReturnRouteSpec{
				ServerID: s1,
				Carrier:  "ct",
				Enabled:  true,
			})
			return err
		}); err != nil {
			t.Fatalf("CreateTx 失败：%v", err)
		}
		if created.Spec.Carrier != "CT" || !created.Spec.Enabled || created.Metadata.ResourceVersion != 1 || created.Status.RouteType != "Unknown" || created.Status.TestedAt != nil {
			t.Fatalf("创建结果异常：%+v", created)
		}

		// 2. 同机重复 (server_id, carrier) 报 name_taken；跨服务器同 carrier 合法。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.CreateTx(ctx, tx, v1.ReturnRouteSpec{ServerID: s1, Carrier: "CT"})
			return err
		}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("同机重复 carrier 应报 name_taken，得到：%v", err)
		}
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.CreateTx(ctx, tx, v1.ReturnRouteSpec{
				ServerID: s2,
				Carrier:  "CT",
				Enabled:  false,
				Probe:    json.RawMessage(`{"target_ip":"202.97.1.1","port":80,"max_hops":25,"timeout_ms":2000}`),
			})
			return err
		}); err != nil {
			t.Fatalf("跨服务器同 carrier 创建应成功：%v", err)
		}

		// 3. UpdateProbeStatus 仅写 6 个 status 列，绝不抬升 resourceVersion。
		testedAt := time.Now().UTC().Truncate(time.Second)
		if err := repo.UpdateProbeStatus(ctx, created.Metadata.ID, ProbeStatusPatch{
			RouteType: "CN2 GIA",
			Region:    "Shanghai",
			EntryIP:   "59.43.130.1",
			EntryASN:  "AS4809",
			Reason:    "hit 59.43 backbone",
			TestedAt:  testedAt,
		}); err != nil {
			t.Fatalf("UpdateProbeStatus 失败：%v", err)
		}
		got, err := repo.ByID(ctx, created.Metadata.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata.ResourceVersion != 1 || got.Status.RouteType != "CN2 GIA" || got.Status.EntryIP != "59.43.130.1" || got.Status.EntryAsn != "AS4809" || got.Status.TestedAt == nil {
			t.Fatalf("UpdateProbeStatus 后字段异常：%+v", got)
		}

		// 4. 禁止跨服务器修改 server_id；合法 UpdateTx 递增版本。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.UpdateTx(ctx, tx, created.Metadata.ID, 1, false, v1.ReturnRouteSpec{ServerID: s2, Carrier: "CT"})
			return err
		}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("跨服务器改 server_id 应报 bad_request，得到：%v", err)
		}
		var updated *ReturnRoute
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			updated, err = repo.UpdateTx(ctx, tx, created.Metadata.ID, 1, false, v1.ReturnRouteSpec{
				ServerID: s1,
				Carrier:  "CT",
				Enabled:  false,
				Probe:    json.RawMessage(`{"max_hops":20}`),
			})
			return err
		}); err != nil {
			t.Fatalf("UpdateTx 失败：%v", err)
		}
		if updated.Metadata.ResourceVersion != 2 || updated.Spec.Enabled {
			t.Fatalf("UpdateTx 结果异常：%+v", updated)
		}

		// 5. 软删除保留自然键占坑，恢复后原 ID 复用且版本递增。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.SoftDeleteTx(ctx, tx, created.Metadata.ID, 2, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.ByServerCarrierIncludeDeleted(ctx, s1, "ct"); err != nil {
			t.Fatalf("ByServerCarrierIncludeDeleted 应能找到软删记录：%v", err)
		}
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.CreateTx(ctx, tx, v1.ReturnRouteSpec{ServerID: s1, Carrier: "CT"})
			return err
		}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("软删除占坑期间新建同名应报 name_taken，得到：%v", err)
		}
		var restored *ReturnRoute
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			restored, err = repo.RestoreTx(ctx, tx, created.Metadata.ID, 3, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if restored.Metadata.ID != created.Metadata.ID || restored.Metadata.ResourceVersion != 4 || restored.Metadata.DeletedAt != nil {
			t.Fatalf("RestoreTx 结果异常：%+v", restored)
		}

		// 6. List 过滤（server_id, carrier, enabled）。
		enFalse := false
		items, total, err := repo.List(ctx, ListFilter{ServerID: s1, Carrier: "ct", Enabled: &enFalse}, 10, 0)
		if err != nil || total != 1 || len(items) != 1 || items[0].Metadata.ID != created.Metadata.ID {
			t.Fatalf("List 过滤异常：total=%d len=%d err=%v", total, len(items), err)
		}

		// 7. 非法 carrier 与非法 probe 拦截。
		for _, bad := range []v1.ReturnRouteSpec{
			{ServerID: s1, Carrier: "bad carrier"},
			{ServerID: s1, Carrier: "CT", Probe: json.RawMessage(`[]`)},
			{ServerID: s1, Carrier: "CT", Probe: json.RawMessage(`{"unknown":1}`)},
			{ServerID: s1, Carrier: "CT", Probe: json.RawMessage(`{"port":70000}`)},
			{ServerID: s1, Carrier: "CT", Probe: json.RawMessage(`{"max_hops":99}`)},
			{ServerID: s1, Carrier: "CT", Probe: json.RawMessage(`{"timeout_ms":100}`)},
		} {
			if _, err := NormalizeSpec(bad); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeBadRequest {
				t.Fatalf("非法 spec %+v 应报 bad_request，得到：%v", bad, err)
			}
		}
	})
}
