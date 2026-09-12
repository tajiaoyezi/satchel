package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func newStore(bdb *bun.DB) *store.Store {
	return store.New(bdb, schema.Default())
}

func rule(name string) *model.AutomationRule {
	return &model.AutomationRule{Name: name, Trigger: "server_offline", Action: "notify", ProposedBy: "ai"}
}

func task(dedup string) *model.Task {
	return &model.Task{Title: "t", Source: "system", DedupKey: dedup}
}

func alert(dedup string) *model.Alert {
	return &model.Alert{Category: "server_offline", Level: "warning", ObjectKind: "Server", ObjectID: "1", DedupKey: dedup}
}

func mustInsert(t *testing.T, s *store.Store, m any) {
	t.Helper()
	if err := s.Insert(context.Background(), m); err != nil {
		t.Fatalf("插入 %T 失败：%v", m, err)
	}
}

func wantCode(t *testing.T, err error, code v1.Code) *v1.Error {
	t.Helper()
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("想要错误码 %s，得到 %v", code, err)
	}
	return e
}

func TestInsertFillsMetaAndDefaults(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := newStore(bdb)
		r := rule("a")
		mustInsert(t, s, r)
		if r.ID == 0 || r.ResourceVersion != 1 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
			t.Fatalf("插入后应当回填 id、版本 1 与时间戳，得到 %+v", r)
		}
		if r.Status != "pending" || string(r.Condition) != "{}" || r.Enabled {
			t.Fatalf("库默认值应当回填，得到 status=%q condition=%s enabled=%v", r.Status, r.Condition, r.Enabled)
		}
		var got model.AutomationRule
		if err := s.Get(context.Background(), &got, r.ID); err != nil {
			t.Fatal(err)
		}
		if got.Name != "a" || got.ResourceVersion != 1 {
			t.Fatalf("读回不对：%+v", got)
		}
	})
}

func TestGetNotFound(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		err := newStore(bdb).Get(context.Background(), &model.Task{}, 999)
		wantCode(t, err, v1.CodeNotFound)
		if v1.ExitCodeOf(err) != v1.ExitNotFound {
			t.Fatalf("退出码应当是 5，得到 %d", v1.ExitCodeOf(err))
		}
	})
}

func TestStaleSpecWriteRejected(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		r := rule("a")
		mustInsert(t, s, r)
		var a, b model.AutomationRule
		for _, m := range []*model.AutomationRule{&a, &b} {
			if err := s.Get(ctx, m, r.ID); err != nil {
				t.Fatal(err)
			}
		}
		a.Trigger = "server_online"
		if err := s.UpdateSpec(ctx, &a, false, "trigger"); err != nil {
			t.Fatal(err)
		}
		if a.ResourceVersion != 2 {
			t.Fatalf("成功的 spec 写应当把版本抬到 2，得到 %d", a.ResourceVersion)
		}
		b.Trigger = "cert_failed"
		err := s.UpdateSpec(ctx, &b, false, "trigger")
		e := wantCode(t, err, v1.CodeVersionConflict)
		if e.State["resourceVersion"] != int64(2) || e.Next == "" {
			t.Fatalf("version_conflict 应当带存储中的版本 2 与下一步提示，得到 state=%v next=%q", e.State, e.Next)
		}
		if v1.ExitCodeOf(err) != v1.ExitVersionConflict {
			t.Fatalf("退出码应当是 6，得到 %d", v1.ExitCodeOf(err))
		}
		var stored model.AutomationRule
		if err := s.Get(ctx, &stored, r.ID); err != nil {
			t.Fatal(err)
		}
		if stored.ResourceVersion != 2 || stored.Trigger != "server_online" {
			t.Fatalf("过期的写不该落库：%+v", stored)
		}
	})
}

func TestForceSkipsOnlyVersionCheck(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		r := rule("a")
		mustInsert(t, s, r)
		stale := *r
		r.Trigger = "x"
		if err := s.UpdateSpec(ctx, r, false, "trigger"); err != nil {
			t.Fatal(err)
		}
		stale.Trigger = "y"
		if err := s.UpdateSpec(ctx, &stale, true, "trigger"); err != nil {
			t.Fatalf("force 应当写成功，得到 %v", err)
		}
		if stale.ResourceVersion != 3 {
			t.Fatalf("force 之后版本应当是 3，得到 %d", stale.ResourceVersion)
		}
		if err := s.UpdateSpec(ctx, &stale, true, "status"); err == nil {
			t.Fatal("force 不该放过分档校验：status 是人类专属列")
		}
	})
}

func TestStatusWriteKeepsVersion(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		a := alert("k")
		mustInsert(t, s, a)
		a.LastSeenAt = time.Now().UTC().Add(time.Minute)
		a.OccurrenceCount = 5
		if err := s.UpdateStatus(ctx, a, "last_seen_at", "occurrence_count"); err != nil {
			t.Fatal(err)
		}
		var got model.Alert
		if err := s.Get(ctx, &got, a.ID); err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != 1 || got.OccurrenceCount != 5 {
			t.Fatalf("status 写不该抬版本：%+v", got)
		}
		wantCode(t, s.UpdateStatus(ctx, a, "category"), v1.CodeBadRequest)
		wantCode(t, s.UpdateStatus(ctx, a), v1.CodeBadRequest)
		wantCode(t, s.UpdateAction(ctx, task(""), "title"), v1.CodeBadRequest)
		missing := alert("nope")
		missing.ID = 12345
		wantCode(t, s.UpdateStatus(ctx, missing, "occurrence_count"), v1.CodeNotFound)
	})
}

func TestSoftDeleteAndRestoreKeepID(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		ch := &model.NotifyChannel{Name: "ops", Type: "webhook", Enabled: true}
		mustInsert(t, s, ch)
		id := ch.ID
		if err := s.SoftDelete(ctx, ch); err != nil {
			t.Fatal(err)
		}
		var got model.NotifyChannel
		if err := s.Get(ctx, &got, id); err != nil {
			t.Fatal(err)
		}
		if got.DeletedAt == nil || got.ResourceVersion != 2 {
			t.Fatalf("软删除后应当有 deleted_at 且版本为 2：%+v", got)
		}
		if err := s.Restore(ctx, &got); err != nil {
			t.Fatal(err)
		}
		var back model.NotifyChannel
		if err := s.Get(ctx, &back, id); err != nil {
			t.Fatal(err)
		}
		if back.ID != id || back.DeletedAt != nil || back.ResourceVersion != 3 {
			t.Fatalf("恢复后 id 不变、deleted_at 为空、版本为 3：%+v", back)
		}
	})
}

func TestAppendOnlyTablesRejectUpdates(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		log := &model.AuditLog{Actor: "alice", ActorKind: "human", Command: "user delete", ArgsDigest: "d", Result: "ok"}
		mustInsert(t, s, log)
		if log.ID == 0 || log.At.IsZero() {
			t.Fatalf("append-only 表也要回填 id 与 at：%+v", log)
		}
		log.Result = "changed"
		wantCode(t, s.UpdateSpec(ctx, log, false, "result"), v1.CodeAppendOnly)
		wantCode(t, s.UpdateStatus(ctx, log, "result"), v1.CodeAppendOnly)
		wantCode(t, s.SoftDelete(ctx, log), v1.CodeAppendOnly)
		var got model.AuditLog
		if err := s.Get(ctx, &got, log.ID); err != nil {
			t.Fatal(err)
		}
		if got.Result != "ok" {
			t.Fatalf("审计记录不该被改：%+v", got)
		}
	})
}

func TestUniqueKeyIncludesSoftDeletedRows(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		first := rule("a")
		mustInsert(t, s, first)
		if err := s.SoftDelete(ctx, first); err != nil {
			t.Fatal(err)
		}
		err := s.Insert(ctx, rule("a"))
		e := wantCode(t, err, v1.CodeNameTaken)
		if errors.Unwrap(e) == nil {
			t.Fatal("原始的驱动错误应当在 Unwrap 链里")
		}
	})
}

func TestCheckViolationIsBadRequest(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := rule("a")
		r.Action = "delete_user"
		wantCode(t, newStore(bdb).Insert(context.Background(), r), v1.CodeBadRequest)
	})
}

func TestTaskDedupKey(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		first := task("k")
		mustInsert(t, s, first)
		wantCode(t, s.Insert(ctx, task("k")), v1.CodeNameTaken)
		first.Status = "done"
		if err := s.UpdateAction(ctx, first, "status"); err != nil {
			t.Fatal(err)
		}
		if err := s.Insert(ctx, task("k")); err != nil {
			t.Fatalf("done 之后同键应当能再插：%v", err)
		}
		for i := 0; i < 2; i++ {
			if err := s.Insert(ctx, task("")); err != nil {
				t.Fatalf("空去重键不受限，第 %d 条失败：%v", i+1, err)
			}
		}
	})
}

func TestAlertDedupKey(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		first := alert("k")
		mustInsert(t, s, first)
		wantCode(t, s.Insert(ctx, alert("k")), v1.CodeNameTaken)
		first.Status = "resolved"
		if err := s.UpdateStatus(ctx, first, "status"); err != nil {
			t.Fatal(err)
		}
		if err := s.Insert(ctx, alert("k")); err != nil {
			t.Fatalf("resolved 之后同键应当能再插：%v", err)
		}
	})
}

func TestJSONColumnsRoundTrip(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		p := &model.Plan{Objects: json.RawMessage(`[{"kind":"Inbound","id":1,"resourceVersion":3}]`), Diff: json.RawMessage(`{"port":[443,8443]}`),
			AffectedCount: 1, ExpiresAt: time.Now().UTC().Add(time.Hour)}
		mustInsert(t, s, p)
		var got model.Plan
		if err := s.Get(ctx, &got, p.ID); err != nil {
			t.Fatal(err)
		}
		var objects []map[string]any
		if err := json.Unmarshal(got.Objects, &objects); err != nil || len(objects) != 1 || objects[0]["kind"] != "Inbound" {
			t.Fatalf("JSON 列读回不对：%s（%v）", got.Objects, err)
		}
		if got.ShareScope != nil {
			t.Fatalf("可空 JSON 列写 NULL 应当读回 nil，得到 %s", got.ShareScope)
		}
	})
}
