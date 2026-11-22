package servers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestServerConfigTxLifecycle(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := New(bdb, store.New(bdb, schema.Default()))
		ctx := context.Background()

		// CreateFromSpecTx 创建并生成三把令牌。
		spec := DefaultSpec()
		spec.Name = "hk-cfg"
		spec.CoreLogLevel = "info"
		spec.CoreDNS = json.RawMessage(`{"servers":[{"tag":"dns-local","address":"local"}]}`)
		spec.CoreStatsEnabled = true
		spec.SortOrder = 7

		var created *Server
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			created, err = r.CreateFromSpecTx(ctx, tx, spec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if created.Metadata.ResourceVersion != 1 || created.Spec.CoreLogLevel != "info" || !created.Spec.CoreStatsEnabled || created.Spec.SortOrder != 7 {
			t.Fatalf("CreateFromSpecTx 结果不对：%+v", created)
		}
		if created.Status.Token.Reveal() == "" || created.Status.AgentToken.Reveal() == "" || created.Status.PullToken.Reveal() == "" {
			t.Fatalf("应自动生成三把凭据")
		}

		// UpdateSpecTx 更新并抬版本。
		spec.CoreLogLevel = "debug"
		spec.Region = "HK"
		var updated *Server
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			updated, err = r.UpdateSpecTx(ctx, tx, created.Metadata.ID, 1, false, spec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if updated.Metadata.ResourceVersion != 2 || updated.Spec.CoreLogLevel != "debug" || updated.Spec.Region != "HK" {
			t.Fatalf("UpdateSpecTx 结果不对：%+v", updated)
		}

		// SoftDeleteTx 软删除保留自然键占坑，新建同名返回 name_taken。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.SoftDeleteTx(ctx, tx, created.Metadata.ID, 2, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Create(ctx, "hk-cfg", ModeAuto); v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("软删除保留期内同名创建应报 name_taken，得到 %v", err)
		}
		// 软删除状态下拒绝 UpdateSpecTx。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := r.UpdateSpecTx(ctx, tx, created.Metadata.ID, 3, false, spec)
			return err
		}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("软删除对象不可修改 spec，得到 %v", err)
		}

		// RestoreTx 按原 ID 恢复并递增版本。
		var restored *Server
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			restored, err = r.RestoreTx(ctx, tx, created.Metadata.ID, 3, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if restored.Metadata.ID != created.Metadata.ID || restored.Metadata.ResourceVersion != 4 || restored.Metadata.DeletedAt != nil {
			t.Fatalf("RestoreTx 结果不对：%+v", restored.Metadata)
		}

		// MarkAppliedConfig 写 applied_hash / applied_generation 并消费 first_apply_eligible（不抬版本）。
		if err := r.SetFirstApplyEligible(ctx, restored.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkAppliedConfig(ctx, restored.Metadata.ID, "abc123", 9); err != nil {
			t.Fatal(err)
		}
		got, err := r.ByID(ctx, restored.Metadata.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata.ResourceVersion != 4 || got.Status.AppliedHash != "abc123" || got.Status.AppliedGeneration != 9 || got.Status.FirstApplyEligible {
			t.Fatalf("MarkAppliedConfig 状态不对：%+v", got.Status)
		}
	})
}

func TestNormalizeSpecPortRangeAndSiteType(t *testing.T) {
	base := DefaultSpec()
	base.Name = "hk-norm"

	// 合法组合
	good := []v1.ServerSpec{
		base,
		func() v1.ServerSpec {
			s := base
			s.PortRangeMin = 20000
			s.PortRangeMax = 30000
			s.SiteType = "Decoy"
			s.SiteValue = "landing"
			return s
		}(),
		func() v1.ServerSpec {
			s := base
			s.PortRangeMin = 15000
			s.PortRangeMax = 15000
			s.SiteType = "static"
			return s
		}(),
		func() v1.ServerSpec { s := base; s.SiteType = "proxy"; s.SiteValue = "http://127.0.0.1:8080"; return s }(),
	}
	for i, tc := range good {
		norm, err := NormalizeSpec(tc)
		if err != nil {
			t.Errorf("合法用例 %d 失败：%v", i, err)
		}
		if tc.SiteType == "Decoy" && norm.SiteType != "decoy" {
			t.Errorf("site_type 应规范化为小写 decoy，得到 %q", norm.SiteType)
		}
	}

	// 非法组合
	bad := []v1.ServerSpec{
		func() v1.ServerSpec { s := base; s.PortRangeMin = -1; return s }(),
		func() v1.ServerSpec { s := base; s.PortRangeMax = 70000; return s }(),
		func() v1.ServerSpec { s := base; s.PortRangeMin = 10000; s.PortRangeMax = 0; return s }(),
		func() v1.ServerSpec { s := base; s.PortRangeMin = 0; s.PortRangeMax = 20000; return s }(),
		func() v1.ServerSpec { s := base; s.PortRangeMin = 30000; s.PortRangeMax = 20000; return s }(),
		func() v1.ServerSpec { s := base; s.SiteType = "php"; return s }(),
	}
	for i, tc := range bad {
		if _, err := NormalizeSpec(tc); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("非法用例 %d 应报 bad_request，实际：%v", i, err)
		}
	}
}
