package taskruns

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
)

func TestStartFinishAndInterrupted(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := New(bdb)
		now := time.Now()
		id, err := r.Start(ctx, "audit_cleanup", now)
		if err != nil || id == 0 {
			t.Fatalf("Start：%d %v", id, err)
		}
		if err := r.Finish(ctx, id, 1500*time.Millisecond, StatusOK, "删掉 12 条"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Start(ctx, "db_health", now); err != nil {
			t.Fatal(err)
		}
		n, err := r.MarkInterrupted(ctx, "主控停止时它还在运行")
		if err != nil || n != 1 {
			t.Fatalf("应当收尾 1 行，得到 %d %v", n, err)
		}
		runs, err := r.List(ctx, Filter{}, 10, 0)
		if err != nil || len(runs) != 2 {
			t.Fatalf("List：%+v %v", runs, err)
		}
		if runs[0].Task != "db_health" || runs[0].Status != StatusError || runs[0].Detail != "主控停止时它还在运行" {
			t.Fatalf("中断的那行不对：%+v", runs[0])
		}
		if runs[1].Status != StatusOK || runs[1].DurationMs != 1500 || runs[1].Detail != "删掉 12 条" || !runs[1].StartedAt.Equal(utc(now)) {
			t.Fatalf("结束的那行不对：%+v", runs[1])
		}
	})
}

func TestListFilterPagingLatest(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := New(bdb)
		now := time.Now()
		for i := 0; i < 25; i++ {
			task, status := "audit_cleanup", StatusOK
			if i%5 == 0 {
				task, status = "db_health", StatusError
			}
			if err := r.Insert(ctx, Run{Task: task, StartedAt: now.Add(time.Duration(i) * time.Minute), Status: status, Detail: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
		}
		f := Filter{Task: "audit_cleanup", Status: StatusOK}
		if n, err := r.Count(ctx, f); err != nil || n != 20 {
			t.Fatalf("Count 应当 20，得到 %d %v", n, err)
		}
		seen := map[int64]bool{}
		var before int64
		for {
			page, err := r.List(ctx, f, 7, before)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, run := range page {
				if seen[run.ID] || run.Task != "audit_cleanup" {
					t.Fatalf("重复或过滤失效：%+v", run)
				}
				seen[run.ID] = true
			}
			before = page[len(page)-1].ID
		}
		if len(seen) != 20 {
			t.Fatalf("翻页应当不重不漏拿到 20 条，得到 %d", len(seen))
		}
		latest, err := r.Latest(ctx)
		if err != nil || len(latest) != 2 || latest["audit_cleanup"].Detail != "24" || latest["db_health"].Detail != "20" {
			t.Fatalf("Latest 不对：%+v %v", latest, err)
		}
	})
}

func TestDeleteBefore(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := New(bdb)
		now := time.Now()
		for i := 0; i < 3; i++ {
			r.Insert(ctx, Run{Task: "x", StartedAt: now.Add(-8 * 24 * time.Hour), Status: StatusOK})
		}
		r.Insert(ctx, Run{Task: "x", StartedAt: now, Status: StatusOK})
		n, err := r.DeleteBefore(ctx, now.Add(-7*24*time.Hour))
		if err != nil || n != 3 {
			t.Fatalf("应当删 3 条，得到 %d %v", n, err)
		}
		if left, _ := r.Count(ctx, Filter{}); left != 1 {
			t.Fatalf("应当剩 1 条，得到 %d", left)
		}
	})
}
