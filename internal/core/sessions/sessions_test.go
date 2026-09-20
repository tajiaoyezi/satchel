package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
)

func seedUser(t *testing.T, bdb *bun.DB, name string) {
	t.Helper()
	u := &model.User{Username: name, Role: "admin", IsActive: true, PasswordHash: "h", RecoveryCodes: []byte(`[]`),
		NodeSpeedLimitOverrides: []byte(`{}`), NodeDeviceLimitOverrides: []byte(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now(), ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSessions(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := New(bdb)
		ctx := context.Background()
		seedUser(t, bdb, "alice")
		now := time.Now().UTC().Truncate(time.Second)
		for i, h := range []string{"h1", "h2", "h3"} {
			exp := now.Add(time.Hour)
			if i == 2 {
				exp = now.Add(-time.Hour) // 过期
			}
			if err := r.Insert(ctx, Session{TokenHash: h, Username: "alice", ExpiresAt: exp}); err != nil {
				t.Fatal(err)
			}
		}
		s, err := r.GetByHash(ctx, "h1")
		if err != nil || s.Username != "alice" || !s.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("读会话：%+v %v", s, err)
		}
		if _, err := r.GetByHash(ctx, "nope"); err != ErrNotFound {
			t.Fatalf("不存在应当 ErrNotFound：%v", err)
		}
		if n, _ := r.CountByUser(ctx, "alice", now); n != 2 {
			t.Fatalf("未过期的应当 2 条，得到 %d", n)
		}
		if n, err := r.DeleteExpired(ctx, now); err != nil || n != 1 {
			t.Fatalf("应当清掉 1 条过期：%d %v", n, err)
		}
		if n, err := r.DeleteByUser(ctx, "alice", "h1"); err != nil || n != 1 {
			t.Fatalf("保留 h1 应当删 1 条：%d %v", n, err)
		}
		if _, err := r.GetByHash(ctx, "h1"); err != nil {
			t.Fatal("h1 应当还在")
		}
		if err := r.Delete(ctx, "h1"); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(ctx, "h1"); err != nil {
			t.Fatal("重复删不算错")
		}
		if n, _ := r.CountByUser(ctx, "alice", now); n != 0 {
			t.Fatalf("应当 0 条，得到 %d", n)
		}
	})
}
