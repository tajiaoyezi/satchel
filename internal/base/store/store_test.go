package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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

// mustUser 建一个用户；mustToken 给它签一把令牌，返回令牌 id（tasks.claimed_by 的外键指向它）。
func mustUser(t *testing.T, s *store.Store, name string) *model.User {
	t.Helper()
	u := &model.User{Username: name, PasswordHash: "x", Role: "admin"}
	mustInsert(t, s, u)
	return u
}

func mustToken(t *testing.T, s *store.Store, owner, name string) int64 {
	t.Helper()
	tok := &model.ApiToken{Owner: owner, Name: name, TokenHash: "hash-" + name, Preset: "ops"}
	mustInsert(t, s, tok)
	return tok.ID
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

// 自增主键由库分配：调用方预填的 id 被忽略，两库一致。
func TestInsertIgnoresPrefilledID(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := newStore(bdb)
		first := rule("a")
		first.ID = 1
		mustInsert(t, s, first)
		second := rule("b")
		mustInsert(t, s, second)
		if second.ID == 0 || second.ID == first.ID {
			t.Fatalf("第二条应当拿到新的 id，得到 first=%d second=%d", first.ID, second.ID)
		}
		third := rule("c")
		third.ID = 999
		mustInsert(t, s, third)
		if third.ID != second.ID+1 {
			t.Fatalf("预填的 999 应当被忽略，得到 %d", third.ID)
		}
	})
}

// 布尔列的库默认值只能是 FALSE，与 Go 零值一致：没填的 enabled 读回 false。
func TestInsertBoolDefaultIsFalse(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := newStore(bdb)
		ch := &model.NotifyChannel{Name: "ops", Type: "webhook"}
		mustInsert(t, s, ch)
		var got model.NotifyChannel
		if err := s.Get(context.Background(), &got, ch.ID); err != nil {
			t.Fatal(err)
		}
		if got.Enabled {
			t.Fatal("没填的 enabled 应当读回 false")
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
		if err := s.UpdateStatus(ctx, a, []string{"last_seen_at", "occurrence_count"}); err != nil {
			t.Fatal(err)
		}
		var got model.Alert
		if err := s.Get(ctx, &got, a.ID); err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != 1 || got.OccurrenceCount != 5 {
			t.Fatalf("status 写不该抬版本：%+v", got)
		}
		wantCode(t, s.UpdateStatus(ctx, a, []string{"category"}), v1.CodeBadRequest)
		wantCode(t, s.UpdateStatus(ctx, a, nil), v1.CodeBadRequest)
		wantCode(t, s.UpdateAction(ctx, task("x"), []string{"title"}), v1.CodeBadRequest)
		missing := alert("nope")
		missing.ID = 12345
		wantCode(t, s.UpdateStatus(ctx, missing, []string{"occurrence_count"}), v1.CodeNotFound)
	})
}

// 主控自身类列有自己的写入口，且不抬版本；经 UpdateSpec 写被拒。
func TestUpdateMasterSelf(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		ch := &model.NotifyChannel{Name: "ops", Type: "webhook"}
		mustInsert(t, s, ch)
		ch.Target = "https://hooks.example/x"
		ch.Secret = "s3cret"
		if err := s.UpdateMasterSelf(ctx, ch, []string{"target", "secret"}); err != nil {
			t.Fatal(err)
		}
		var got model.NotifyChannel
		if err := s.Get(ctx, &got, ch.ID); err != nil {
			t.Fatal(err)
		}
		if got.Target != "https://hooks.example/x" || got.Secret != "s3cret" || got.ResourceVersion != 1 {
			t.Fatalf("主控自身类写入应当落库且不抬版本：%+v", got)
		}
		wantCode(t, s.UpdateSpec(ctx, ch, false, "target"), v1.CodeBadRequest)
		wantCode(t, s.UpdateStatus(ctx, ch, []string{"target"}), v1.CodeBadRequest)
	})
}

// 前置条件让「只许从 open 到 claimed」成为原子操作：两个写者并发认领，恰好一个成功。
func TestConditionalUpdateClaim(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		base := task("k")
		mustInsert(t, s, base)
		mustUser(t, s, "claimer")
		const writers = 2
		tokens := make([]int64, writers)
		for i := range tokens {
			tokens[i] = mustToken(t, s, "claimer", "runtime-"+string(rune('a'+i)))
		}
		results := make([]error, writers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := *base
				m.Status = "claimed"
				who := tokens[i]
				m.ClaimedBy = &who
				<-start
				results[i] = s.UpdateAction(ctx, &m, []string{"status", "claimed_by"}, store.Cond{Column: "status", Value: "open"})
			}(i)
		}
		close(start)
		wg.Wait()
		succeeded, winner := 0, int64(0)
		for i, err := range results {
			if err == nil {
				succeeded++
				winner = tokens[i]
				continue
			}
			e := wantCode(t, err, v1.CodeConflict)
			if e.State["status"] == nil {
				t.Fatalf("conflict 应当带 status 的当前值，得到 %v", e.State)
			}
		}
		if succeeded != 1 {
			t.Fatalf("恰好一个写者应当成功，得到 %d", succeeded)
		}
		var got model.Task
		if err := s.Get(ctx, &got, base.ID); err != nil {
			t.Fatal(err)
		}
		if got.Status != "claimed" || got.ClaimedBy == nil || *got.ClaimedBy != winner {
			t.Fatalf("claimed_by 应当是成功者 %d：%+v", winner, got)
		}
	})
}

func TestConditionalUpdateLeavesRowUntouched(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		tk := task("k")
		mustInsert(t, s, tk)
		tk.Status = "done"
		if err := s.UpdateAction(ctx, tk, []string{"status"}); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(tk)
		attempt := *tk
		attempt.Status = "claimed"
		who := int64(7)
		attempt.ClaimedBy = &who
		err := s.UpdateAction(ctx, &attempt, []string{"status", "claimed_by"}, store.Cond{Column: "status", Value: "open"})
		e := wantCode(t, err, v1.CodeConflict)
		if got, _ := e.State["status"].(string); got != "done" {
			t.Fatalf("state 里 status 应当是 done，得到 %v", e.State)
		}
		var stored model.Task
		if err := s.Get(ctx, &stored, tk.ID); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(&stored)
		if string(before) != string(after) {
			t.Fatalf("前置条件不满足时行不该变：\n前 %s\n后 %s", before, after)
		}
		wantCode(t, s.UpdateAction(ctx, &attempt, []string{"status"}, store.Cond{Column: "nope", Value: 1}), v1.CodeBadRequest)
		nilCond := *tk
		nilCond.Status = "open"
		if err := s.UpdateAction(ctx, &nilCond, []string{"status"}, store.Cond{Column: "claimed_by", Value: nil}); err != nil {
			t.Fatalf("Value 为 nil 的前置条件应当匹配 NULL：%v", err)
		}
		// 模型字段那种带类型的 nil 指针也要当 NULL，不能渲染成永远为假的「= NULL」。
		typedNil := nilCond
		typedNil.Status = "claimed"
		if err := s.UpdateAction(ctx, &typedNil, []string{"status"}, store.Cond{Column: "claimed_by", Value: typedNil.ClaimedBy}); err != nil {
			t.Fatalf("带类型的 nil 指针前置条件应当匹配 NULL：%v", err)
		}
	})
}

// 写没落地时模型不该被改：updated_at 不能带着库里没有的时间。
func TestFailedWritesLeaveModelUntouched(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		tk := task("k")
		mustInsert(t, s, tk)
		attempt := *tk
		attempt.Status = "claimed"
		wantCode(t, s.UpdateAction(ctx, &attempt, []string{"status"}, store.Cond{Column: "status", Value: "done"}), v1.CodeConflict)
		if !attempt.UpdatedAt.Equal(tk.UpdatedAt) {
			t.Fatalf("前置条件失败后模型的 updated_at 不该变：%v → %v", tk.UpdatedAt, attempt.UpdatedAt)
		}
		r := rule("a")
		mustInsert(t, s, r)
		stale := *r
		r.Trigger = "x"
		if err := s.UpdateSpec(ctx, r, false, "trigger"); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(&stale)
		wantCode(t, s.SoftDelete(ctx, &stale, false), v1.CodeVersionConflict)
		if after, _ := json.Marshal(&stale); string(after) != string(before) {
			t.Fatalf("过期删除失败后模型不该变：\n前 %s\n后 %s", before, after)
		}
		stale.Trigger = "y"
		before, _ = json.Marshal(&stale)
		wantCode(t, s.UpdateSpec(ctx, &stale, false, "trigger"), v1.CodeVersionConflict)
		if after, _ := json.Marshal(&stale); string(after) != string(before) {
			t.Fatalf("过期 spec 写失败后模型不该变：\n前 %s\n后 %s", before, after)
		}
	})
}

func TestSoftDeleteAndRestoreKeepID(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		ch := &model.NotifyChannel{Name: "ops", Type: "webhook", Enabled: true}
		mustInsert(t, s, ch)
		id := ch.ID
		if err := s.SoftDelete(ctx, ch, false); err != nil {
			t.Fatal(err)
		}
		var got model.NotifyChannel
		if err := s.Get(ctx, &got, id); err != nil {
			t.Fatal(err)
		}
		if got.DeletedAt == nil || got.ResourceVersion != 2 {
			t.Fatalf("软删除后应当有 deleted_at 且版本为 2：%+v", got)
		}
		if err := s.Restore(ctx, &got, false); err != nil {
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

// 删除与恢复是带版本的写：过期的删除被拒，force 只跳过比对；重复删除与恢复未删除对象都被拒且不改行。
func TestSoftDeleteVersionAndStateChecks(t *testing.T) {
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
		err := s.SoftDelete(ctx, &stale, false)
		e := wantCode(t, err, v1.CodeVersionConflict)
		if e.State["resourceVersion"] != int64(2) || stale.DeletedAt != nil {
			t.Fatalf("过期删除应当报出版本 2 且不改模型：state=%v deletedAt=%v", e.State, stale.DeletedAt)
		}
		var stored model.AutomationRule
		if err := s.Get(ctx, &stored, r.ID); err != nil {
			t.Fatal(err)
		}
		if stored.DeletedAt != nil || stored.ResourceVersion != 2 || stored.Trigger != "x" {
			t.Fatalf("过期删除不该落库：%+v", stored)
		}
		wantCode(t, s.Restore(ctx, r, false), v1.CodeBadRequest)
		if err := s.Get(ctx, &stored, r.ID); err != nil || stored.ResourceVersion != 2 {
			t.Fatalf("恢复未删除的对象不该抬版本：%+v %v", stored, err)
		}
		if err := s.SoftDelete(ctx, &stale, true); err != nil {
			t.Fatalf("force 删除应当成功：%v", err)
		}
		if stale.ResourceVersion != 3 || stale.DeletedAt == nil {
			t.Fatalf("force 删除后版本 3 且 deleted_at 非空：%+v", stale)
		}
		firstDeletedAt := *stale.DeletedAt
		time.Sleep(2 * time.Millisecond)
		again := stale
		err = s.SoftDelete(ctx, &again, true)
		wantCode(t, err, v1.CodeBadRequest)
		if err := s.Get(ctx, &stored, r.ID); err != nil {
			t.Fatal(err)
		}
		if stored.DeletedAt == nil || !stored.DeletedAt.Equal(firstDeletedAt) || stored.ResourceVersion != 3 {
			t.Fatalf("重复删除不该改 deleted_at 或版本：%+v（首次 %v）", stored, firstDeletedAt)
		}
		missing := rule("m")
		missing.ID = 424242
		wantCode(t, s.SoftDelete(ctx, missing, false), v1.CodeNotFound)
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
		wantCode(t, s.UpdateStatus(ctx, log, []string{"result"}), v1.CodeAppendOnly)
		wantCode(t, s.SoftDelete(ctx, log, false), v1.CodeAppendOnly)
		var got model.AuditLog
		if err := s.Get(ctx, &got, log.ID); err != nil {
			t.Fatal(err)
		}
		if got.Result != "ok" {
			t.Fatalf("审计记录不该被改：%+v", got)
		}
	})
}

// 自然键冲突报 name_taken，含软删除后的同名插入。
func TestUniqueKeyIncludesSoftDeletedRows(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		first := rule("a")
		mustInsert(t, s, first)
		if err := s.SoftDelete(ctx, first, false); err != nil {
			t.Fatal(err)
		}
		err := s.Insert(ctx, rule("a"))
		e := wantCode(t, err, v1.CodeNameTaken)
		if errors.Unwrap(e) == nil {
			t.Fatal("原始的驱动错误应当在 Unwrap 链里")
		}
		if !strings.Contains(e.Reason, "name") {
			t.Fatalf("name_taken 应当点名 name 列：%q", e.Reason)
		}
		// job_id 是系统生成的幂等 id，不是名字：撞上报 conflict 并点名列，next 不能让人「换个名字」。
		job := &model.Job{JobID: "j1", Kind: "exec", Status: "queued"}
		mustInsert(t, s, job)
		e = wantCode(t, s.Insert(ctx, &model.Job{JobID: "j1", Kind: "exec", Status: "queued"}), v1.CodeConflict)
		if !strings.Contains(e.Reason, "job_id") || strings.Contains(e.Reason, "名字") {
			t.Fatalf("job_id 冲突的 reason 应当点名 job_id 且不提名字：%q", e.Reason)
		}
	})
}

// 分档更新不能改写调用方传进来的列清单。
func TestUpdateDoesNotMutateCallerSlice(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		tk := task("k")
		mustInsert(t, s, tk)
		all := []string{"status", "claimed_by"}
		tk.Status = "claimed"
		if err := s.UpdateAction(ctx, tk, all[:1]); err != nil {
			t.Fatal(err)
		}
		if all[1] != "claimed_by" {
			t.Fatalf("调用方的切片被改写了：%v", all)
		}
	})
}

// CHECK 与外键违反是请求不合法：bad_request。
func TestConstraintViolationsAreBadRequest(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		r := rule("a")
		r.Action = "delete_user"
		if e := wantCode(t, s.Insert(ctx, r), v1.CodeBadRequest); !strings.Contains(e.Reason, "action") || e.State["allowed"] == nil {
			t.Fatalf("白名单外的动作应当点名 action 并给出允许的值：%+v", e)
		}
		if e := wantCode(t, s.Insert(ctx, alert("")), v1.CodeBadRequest); !strings.Contains(e.Reason, "dedup_key") {
			t.Fatalf("空去重键应当点名 dedup_key：%+v", e)
		}
		dangling := task("")
		alertID := int64(999)
		dangling.AlertID = &alertID
		if e := wantCode(t, s.Insert(ctx, dangling), v1.CodeBadRequest); !strings.Contains(e.Reason, "引用的对象不存在") {
			t.Fatalf("外键违反的 reason 应当说引用的对象不存在：%+v", e)
		}
		missing := &model.Task{Title: "t"} // source 是 NOT NULL 且没有默认值
		if e := wantCode(t, s.Insert(ctx, missing), v1.CodeBadRequest); !strings.Contains(e.Reason, "source") {
			t.Fatalf("NOT NULL 违反应当点名 source：%+v", e)
		}
	})
}

// 去重键不是名字：撞上部分唯一索引报 conflict 并点名 dedup_key。
func TestTaskDedupKey(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		first := task("k")
		mustInsert(t, s, first)
		e := wantCode(t, s.Insert(ctx, task("k")), v1.CodeConflict)
		if !containsString(e.Reason, "dedup_key") {
			t.Fatalf("reason 应当点名 dedup_key，得到 %q", e.Reason)
		}
		first.Status = "done"
		if err := s.UpdateAction(ctx, first, []string{"status"}); err != nil {
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
		e := wantCode(t, s.Insert(ctx, alert("k")), v1.CodeConflict)
		if !containsString(e.Reason, "dedup_key") {
			t.Fatalf("reason 应当点名 dedup_key，得到 %q", e.Reason)
		}
		first.Status = "resolved"
		if err := s.UpdateStatus(ctx, first, []string{"status"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Insert(ctx, alert("k")); err != nil {
			t.Fatalf("resolved 之后同键应当能再插：%v", err)
		}
	})
}

// 复合主键撞上也是 conflict，点名两列。
func TestCompositePrimaryKeyConflict(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		a := alert("k")
		mustInsert(t, s, a)
		ch := &model.NotifyChannel{Name: "ops", Type: "webhook"}
		mustInsert(t, s, ch)
		d := &model.NotifyDelivery{DeliveryID: "d1", ChannelID: ch.ID, Type: "alerts", Payload: json.RawMessage(`{}`)}
		mustInsert(t, s, d)
		link := &model.AlertDelivery{AlertID: a.ID, DeliveryID: d.ID}
		mustInsert(t, s, link)
		e := wantCode(t, s.Insert(ctx, &model.AlertDelivery{AlertID: a.ID, DeliveryID: d.ID}), v1.CodeConflict)
		if !containsString(e.Reason, "alert_id") || !containsString(e.Reason, "delivery_id") {
			t.Fatalf("reason 应当点名两列，得到 %q", e.Reason)
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

func containsString(s, sub string) bool {
	return strings.Contains(s, sub)
}
