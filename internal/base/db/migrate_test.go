package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
)

func TestMigrateEmptyDatabaseThenRerun(t *testing.T) {
	dbtest.ForEachEmpty(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		before, err := db.Status(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if len(before.Applied) != 0 || strings.Join(before.Pending, ",") != "0001_init" {
			t.Fatalf("空库的状态应当是待应用 0001_init，得到 %+v", before)
		}
		applied, err := db.Migrate(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(applied, ",") != "0001_init" {
			t.Fatalf("应当应用 0001_init，得到 %v", applied)
		}
		tables, err := db.Introspect(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if want := len(schema.Default().Tables()); len(tables) != want {
			t.Fatalf("迁移后应当有 %d 张表，得到 %d", want, len(tables))
		}
		again, err := db.Migrate(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 0 {
			t.Fatalf("重跑不该应用任何迁移，得到 %v", again)
		}
		after, err := db.Status(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(after.Applied, ",") != "0001_init" || len(after.Pending) != 0 {
			t.Fatalf("迁移后的状态不对：%+v", after)
		}
	})
}
