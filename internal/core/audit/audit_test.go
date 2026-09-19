package audit

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func repo(t *testing.T, bdb *bun.DB) *Repo {
	t.Helper()
	return New(bdb, store.New(bdb, schema.Default()))
}

func seed(t *testing.T, r *Repo, n int, actor, cmd string, at time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := r.Insert(context.Background(), Record{At: at.Add(time.Duration(i) * time.Second), Actor: actor, ActorKind: v1.ActorLocalAdmin, Command: cmd, ArgsDigest: `{"args":[],"flags":{}}`, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInsertListCount(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := repo(t, bdb)
		ctx := context.Background()
		base := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
		seed(t, r, 3, "root", "whoami", base)
		seed(t, r, 2, "alice", "audit list", base.Add(time.Hour))
		seed(t, r, 1, "root", "audit_x", base.Add(2*time.Hour)) // 下划线：LIKE 转义的反向用例

		rows, err := r.List(ctx, Filter{}, 10, 0)
		if err != nil || len(rows) != 6 {
			t.Fatalf("应当 6 条：%d %v", len(rows), err)
		}
		for i := 1; i < len(rows); i++ {
			if rows[i].ID >= rows[i-1].ID {
				t.Fatal("应当按 id 倒序")
			}
		}
		if rows[0].Command != "audit_x" || rows[0].ActorKind != v1.ActorLocalAdmin || rows[0].At.IsZero() {
			t.Fatalf("字段没回来：%+v", rows[0])
		}
		if n, _ := r.Count(ctx, Filter{Actor: "root"}); n != 4 {
			t.Fatalf("actor 精确过滤应当 4 条，得到 %d", n)
		}
		if n, _ := r.Count(ctx, Filter{CommandPrefix: "audit"}); n != 3 {
			t.Fatalf("command 前缀 audit 应当 3 条，得到 %d", n)
		}
		if n, _ := r.Count(ctx, Filter{CommandPrefix: "audit_"}); n != 1 {
			t.Fatalf("前缀里的下划线要按字面匹配，应当 1 条，得到 %d", n)
		}
		if n, _ := r.Count(ctx, Filter{CommandPrefix: "audit%"}); n != 0 {
			t.Fatalf("前缀里的百分号要按字面匹配，应当 0 条，得到 %d", n)
		}
		since := base.Add(time.Hour)
		if n, _ := r.Count(ctx, Filter{Since: &since}); n != 3 {
			t.Fatalf("since 过滤应当 3 条，得到 %d", n)
		}
		// keyset：id 小于某条的。
		page2, err := r.List(ctx, Filter{}, 10, rows[2].ID)
		if err != nil || len(page2) != 3 || page2[0].ID != rows[3].ID {
			t.Fatalf("beforeID 翻页不对：%v %v", page2, err)
		}
	})
}
