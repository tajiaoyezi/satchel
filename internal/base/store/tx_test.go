package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-settings 的写路径要在一个事务里比对版本、写两张表、存快照：Bump 只抬版本，WithTx 让写入原语进事务。

func TestBump(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		r := rule("bump")
		mustInsert(t, s, r)
		if err := s.Bump(ctx, r, false); err != nil || r.ResourceVersion != 2 || r.UpdatedAt.IsZero() {
			t.Fatalf("Bump 应当把版本抬到 2 并回填：%v %+v", err, r)
		}
		// Bump 后模型里的版本就是期望值，接着 UpdateSpec 直接能用。
		r.Trigger = "server_online"
		if err := s.UpdateSpec(ctx, r, false, "trigger"); err != nil || r.ResourceVersion != 3 {
			t.Fatalf("Bump 之后的 UpdateSpec 应当成功并到 3：%v %d", err, r.ResourceVersion)
		}
		stale := &model.AutomationRule{ID: r.ID, ResourceVersion: 1}
		e := wantCode(t, s.Bump(ctx, stale, false), v1.CodeVersionConflict)
		if e.State["resourceVersion"] != int64(3) {
			t.Fatalf("version_conflict 应当带存储中的版本 3：%v", e.State)
		}
		if err := s.Bump(ctx, stale, true); err != nil || stale.ResourceVersion != 4 {
			t.Fatalf("force 只跳过比对，版本仍加 1：%v %d", err, stale.ResourceVersion)
		}
		var got model.AutomationRule
		if err := s.Get(ctx, &got, r.ID); err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != 4 || got.Trigger != "server_online" || got.Name != "bump" {
			t.Fatalf("Bump 不该动别的列：%+v", got)
		}
		wantCode(t, s.Bump(ctx, &model.AutomationRule{ID: 424242, ResourceVersion: 1}, false), v1.CodeNotFound)
		wantCode(t, s.Bump(ctx, &model.AuditLog{ID: 1}, false), v1.CodeAppendOnly)
		wantCode(t, s.Bump(ctx, &model.SystemSettingEntry{Key: "x"}, false), v1.CodeBadRequest)
	})
}

func TestWithTxFollowsTransaction(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		r := rule("tx")
		mustInsert(t, s, r)
		abort := errors.New("abort")
		err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			ts := s.WithTx(tx)
			r.Trigger = "server_online"
			if err := ts.UpdateSpec(ctx, r, false, "trigger"); err != nil {
				return err
			}
			if err := ts.Bump(ctx, r, false); err != nil {
				return err
			}
			if err := ts.Insert(ctx, task("tx-task")); err != nil {
				return err
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatalf("事务应当因 abort 回滚：%v", err)
		}
		var got model.AutomationRule
		if err := s.Get(ctx, &got, r.ID); err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != 1 || got.Trigger != "server_offline" {
			t.Fatalf("回滚后一切照旧：%+v", got)
		}
		if n, _ := bdb.NewSelect().Model((*model.Task)(nil)).Where("dedup_key = ?", "tx-task").Count(ctx); n != 0 {
			t.Fatalf("回滚后不该有插入的行，得到 %d", n)
		}
		// 提交的事务落地：先重读拿最新版本（上一个事务在内存里抬过的版本不作数）。
		if err := s.Get(ctx, r, r.ID); err != nil {
			t.Fatal(err)
		}
		err = bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			ts := s.WithTx(tx)
			r.Trigger = "cert_failed"
			return ts.UpdateSpec(ctx, r, false, "trigger")
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Get(ctx, &got, r.ID); err != nil || got.ResourceVersion != 2 || got.Trigger != "cert_failed" {
			t.Fatalf("提交后应当落地：%v %+v", err, got)
		}
	})
}
