package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
)

func insertEvents(t *testing.T, bdb *bun.DB, at time.Time, n int) {
	t.Helper()
	rows := make([]model.SecurityEvent, n)
	for i := range rows {
		rows[i] = model.SecurityEvent{At: at.UTC(), IP: "198.51.100.7", Kind: "probe"}
	}
	if _, err := bdb.NewInsert().Model(&rows).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func countEvents(t *testing.T, bdb *bun.DB) int {
	t.Helper()
	n, err := bdb.NewSelect().Model((*model.SecurityEvent)(nil)).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// 跨多批删除、碰到保留期以内的行就停、空表。
func TestPruneBefore(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		now := time.Now()
		cutoff := now.Add(-90 * 24 * time.Hour)
		if n, err := store.PruneBefore(ctx, bdb, "security_events", "at", cutoff, 10); err != nil || n != 0 {
			t.Fatalf("空表应当删 0 行，得到 %d %v", n, err)
		}
		insertEvents(t, bdb, now.Add(-100*24*time.Hour), 25) // 三批才删完
		insertEvents(t, bdb, now, 3)
		insertEvents(t, bdb, now.Add(-100*24*time.Hour), 2) // 新行之后的旧行：按 id 扫到新行就停，不删
		n, err := store.PruneBefore(ctx, bdb, "security_events", "at", cutoff, 10)
		if err != nil || n != 25 {
			t.Fatalf("应当删掉 25 行，得到 %d %v", n, err)
		}
		if left := countEvents(t, bdb); left != 5 {
			t.Fatalf("应当剩 5 行，得到 %d", left)
		}
		if n, err := store.PruneBefore(ctx, bdb, "security_events", "at", cutoff, 10); err != nil || n != 0 {
			t.Fatalf("第二次应当删 0 行，得到 %d %v", n, err)
		}
		// 时间恰好等于截止时间的行不算「早于」，留着（审查第 4 条）。
		if _, err := bdb.NewDelete().Model((*model.SecurityEvent)(nil)).Where("1 = 1").Exec(ctx); err != nil {
			t.Fatal(err)
		}
		exact := cutoff.UTC().Truncate(time.Microsecond)
		insertEvents(t, bdb, exact, 1)
		insertEvents(t, bdb, now, 1)
		if n, err := store.PruneBefore(ctx, bdb, "security_events", "at", exact, 10); err != nil || n != 0 || countEvents(t, bdb) != 2 {
			t.Fatalf("等于截止时间的行不应当删，得到 %d %v", n, err)
		}
		// 恰好一整批都是旧行、之后没有别的行：删完后再看一批是空的，停下。
		if _, err := bdb.NewDelete().Model((*model.SecurityEvent)(nil)).Where("1 = 1").Exec(ctx); err != nil {
			t.Fatal(err)
		}
		insertEvents(t, bdb, now.Add(-100*24*time.Hour), 10)
		if n, err := store.PruneBefore(ctx, bdb, "security_events", "at", cutoff, 10); err != nil || n != 10 || countEvents(t, bdb) != 0 {
			t.Fatalf("整批旧行应当全删，得到 %d %v", n, err)
		}
	})
}
