package auth

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
)

// master-web-session「会话令牌与存储」：登录时不再顺带删过期会话，由 session_cleanup 调 PruneSessions 清理；没过期的留着。
func TestPruneSessions(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := service(t, bdb)
		seedUser(t, bdb, "alice", "admin", true)
		for i := 0; i < 2; i++ {
			if _, err := s.Login(ctx, "alice", password, false, ""); err != nil {
				t.Fatal(err)
			}
		}
		count := func() int {
			n, err := bdb.NewSelect().Model((*model.Session)(nil)).Count(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		// 把其中一条改成已过期。
		var one model.Session
		if err := bdb.NewSelect().Model(&one).Limit(1).Scan(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := bdb.NewUpdate().Model((*model.Session)(nil)).Set("expires_at = ?", time.Now().Add(-time.Hour).UTC()).
			Where("token_hash = ?", one.TokenHash).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Login(ctx, "alice", password, false, ""); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != 3 {
			t.Fatalf("登录不应当删过期会话：应当 3 条，得到 %d", n)
		}
		if n, err := s.PruneSessions(ctx); err != nil || n != 1 {
			t.Fatalf("应当删掉 1 条过期会话，得到 %d %v", n, err)
		}
		if n := count(); n != 2 {
			t.Fatalf("没过期的两条应当还在，得到 %d", n)
		}
	})
}
