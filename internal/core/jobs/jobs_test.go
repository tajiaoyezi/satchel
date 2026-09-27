package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func newRepo(bdb *bun.DB) *Repo { return New(bdb, store.New(bdb, schema.Default())) }

func TestLifecycle(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := newRepo(bdb)
		j, err := r.Insert(ctx, "job-0000000000000001", "backup create", json.RawMessage(`{"actor":"root"}`))
		if err != nil || j.Status != StatusQueued || j.ID == 0 {
			t.Fatalf("Insert：%+v %v", j, err)
		}
		if _, err := r.Insert(ctx, "job-0000000000000001", "backup create", json.RawMessage(`{}`)); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("重复的 job_id 应当 conflict，得到 %v", err)
		}
		now := time.Now()
		if err := r.SetRunning(ctx, j.ID, now); err != nil {
			t.Fatal(err)
		}
		// 两个并发的 Finish 只有一个生效。
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = r.Finish(ctx, j.ID, StatusRunning, StatusDone, 0, fmt.Sprintf(`{"n":%d}`, i), false, now)
			}(i)
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("两个 Finish 应当恰好一个成功：%v %v", errs[0], errs[1])
		}
		got, err := r.Get(ctx, j.JobID)
		if err != nil || got.Status != StatusDone || *got.ExitCode != 0 || got.StartedAt == nil || got.FinishedAt == nil || !got.Finished() {
			t.Fatalf("Get：%+v %v", got, err)
		}
		if _, err := r.Get(ctx, "job-nosuch"); err != ErrNotFound {
			t.Fatalf("没有的 job 应当 ErrNotFound，得到 %v", err)
		}
	})
}

func TestInterruptedListAndCleanup(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := newRepo(bdb)
		now := time.Now()
		var ids []int64
		for i := 0; i < 5; i++ {
			j, err := r.Insert(ctx, fmt.Sprintf("job-%016d", i), "backup create", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, j.ID)
		}
		r.SetRunning(ctx, ids[0], now)
		r.Finish(ctx, ids[0], StatusRunning, StatusDone, 0, "{}", false, now)
		r.SetRunning(ctx, ids[1], now)
		n, err := r.MarkInterrupted(ctx, `{"code":"internal"}`, now)
		if err != nil || n != 4 {
			t.Fatalf("应当收尾 4 个（1 个 running、3 个 queued），得到 %d %v", n, err)
		}
		if c, _ := r.Count(ctx, StatusFailed); c != 4 {
			t.Fatalf("应当有 4 个 failed，得到 %d", c)
		}
		seen := map[string]bool{}
		var before int64
		for {
			page, err := r.List(ctx, StatusFailed, 3, before)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, j := range page {
				seen[j.JobID] = true
			}
			before = page[len(page)-1].ID
		}
		if len(seen) != 4 {
			t.Fatalf("翻页应当拿到 4 个，得到 %d", len(seen))
		}
		// 再插一个没结束的，把所有行的 created_at 拨到 8 天前：清理只删已结束的。
		if _, err := r.Insert(ctx, "job-running", "backup create", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := bdb.NewUpdate().Table("jobs").Set("created_at = ?", now.Add(-8*24*time.Hour).UTC()).Where("1 = 1").Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := r.DeleteFinishedBefore(ctx, now.Add(-7*24*time.Hour)); err != nil || n != 5 {
			t.Fatalf("应当删掉 5 个已结束的，得到 %d %v", n, err)
		}
		if _, err := r.Get(ctx, "job-running"); err != nil {
			t.Fatalf("没结束的应当还在：%v", err)
		}
	})
}
