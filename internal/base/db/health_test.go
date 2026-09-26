package db_test

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
)

// master-scheduler 的 db_checkpoint 与 db_health：SQLite 下检查点成功、quick_check 通过；PostgreSQL 下 QuickCheck 只 ping，没有检查点。
func TestCheckpointAndQuickCheck(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		if err := db.QuickCheck(ctx, bdb); err != nil {
			t.Fatalf("健康的库 QuickCheck 应当通过：%v", err)
		}
		busy, remaining, err := db.Checkpoint(ctx, bdb)
		if db.DialectOf(bdb) == schema.Postgres {
			if err == nil {
				t.Fatal("PostgreSQL 下检查点应当报不适用")
			}
			return
		}
		if err != nil || busy || remaining != 0 {
			t.Fatalf("空闲的库检查点应当一次做完：%v %v %d", err, busy, remaining)
		}
	})
}
