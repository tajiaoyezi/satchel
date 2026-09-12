package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestMigrateEmptyDatabaseThenRerun(t *testing.T) {
	dbtest.ForEachEmpty(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		before, err := db.Status(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if len(before.Applied) != 0 || strings.Join(before.Pending, ",") != "0001_init" || before.Schema.Checked {
			t.Fatalf("空库的状态应当是待应用 0001_init、未比对结构，得到 %+v", before)
		}
		tables, err := db.Introspect(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if len(tables) != 0 {
			t.Fatalf("Status 不该建任何表，库里却有 %d 张", len(tables))
		}
		var n int
		if err := bdb.NewRaw(existsQuery(bdb), db.MigrationsTable).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("Status 不该创建迁移表")
		}
		applied, err := db.Migrate(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(applied, ",") != "0001_init" {
			t.Fatalf("应当应用 0001_init，得到 %v", applied)
		}
		tables, err = db.Introspect(ctx, bdb)
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
		if !after.Schema.Checked || after.Schema.Consistent == nil || !*after.Schema.Consistent || len(after.Schema.Diff) != 0 {
			t.Fatalf("全部应用后应当比对结构且一致，得到 %+v", after.Schema)
		}
		if before.Schema.Consistent != nil {
			t.Fatalf("没比对时 consistent 应当是 null（nil），得到 %v", *before.Schema.Consistent)
		}
	})
}

func existsQuery(bdb *bun.DB) string {
	if db.DialectOf(bdb) == schema.SQLite {
		return "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?"
	}
	return "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?"
}

func TestMigrationNames(t *testing.T) {
	for _, d := range []schema.Dialect{schema.SQLite, schema.Postgres} {
		names, err := db.MigrationNames(d)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(names, ",") != "0001_init" {
			t.Fatalf("%s 的内置迁移应当只有 0001_init，得到 %v", d, names)
		}
	}
}

// 按旧结构建的库（这里用 ALTER TABLE 模拟）跑迁移时没有待应用的迁移，Migrate 自己就要报 schema_mismatch，
// Check 与 Status 也都要报出差异。
func TestMigrateReportsDriftOnOldDatabase(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		if _, err := bdb.ExecContext(ctx, "ALTER TABLE tasks ADD COLUMN colour TEXT"); err != nil {
			t.Fatal(err)
		}
		applied, err := db.Migrate(ctx, bdb)
		var e *v1.Error
		if !errors.As(err, &e) || e.Code != v1.CodeSchemaMismatch || !strings.Contains(e.Reason, "colour") || e.Next == "" {
			t.Fatalf("Migrate 应当报 schema_mismatch 并点名 colour：%v", err)
		}
		if len(applied) != 0 || e.State["applied"] == nil {
			t.Fatalf("不该有迁移被应用，且 state 里要带 applied：%v %v", applied, e.State)
		}
		check, err := db.Check(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if len(check.Diff) != 1 || !strings.Contains(check.Diff[0], "tasks") || !strings.Contains(check.Diff[0], "colour") || *check.Consistent {
			t.Fatalf("应当恰好报出 tasks 的列 colour，得到 %+v", check)
		}
		st, err := db.Status(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Schema.Checked || *st.Schema.Consistent || len(st.Schema.Diff) != 1 {
			t.Fatalf("Status 应当报出不一致，得到 %+v", st.Schema)
		}
	})
}

// 库里多出来的表只提示不算错误：共用一个 PostgreSQL 库时会有别的应用的表。
func TestExtraTablesAreReportedNotFailed(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		if _, err := bdb.ExecContext(ctx, "CREATE TABLE someone_elses (id INTEGER)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Migrate(ctx, bdb); err != nil {
			t.Fatalf("多出来的表不该让迁移失败：%v", err)
		}
		check, err := db.Check(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if !*check.Consistent || len(check.Diff) != 0 || strings.Join(check.ExtraTables, ",") != "someone_elses" {
			t.Fatalf("应当一致且把多出来的表单列，得到 %+v", check)
		}
	})
}

// 迁移记账缺失而库里已有表：Migrate 不动库、报 schema_mismatch 并说明；Status 标出记账缺失并照常比对。
func TestMissingBookkeepingIsReported(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		for _, tbl := range []string{db.MigrationLocksTable, db.MigrationsTable} {
			if _, err := bdb.ExecContext(ctx, "DROP TABLE "+tbl); err != nil {
				t.Fatal(err)
			}
		}
		_, err := db.Migrate(ctx, bdb)
		var e *v1.Error
		if !errors.As(err, &e) || e.Code != v1.CodeSchemaMismatch || !strings.Contains(e.Reason, "迁移记账") || e.Next == "" || e.State["tables"] == nil {
			t.Fatalf("应当报 schema_mismatch 并说明迁移记账缺失：%v", err)
		}
		st, err := db.Status(ctx, bdb)
		if err != nil {
			t.Fatal(err)
		}
		if !st.BookkeepingMissing || strings.Join(st.Pending, ",") != "0001_init" || !st.Schema.Checked || !*st.Schema.Consistent {
			t.Fatalf("Status 应当标出记账缺失、全部待应用、结构一致，得到 %+v", st)
		}
	})
}

func TestResidualLockCanBeCleared(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		if _, err := bdb.ExecContext(ctx, "INSERT INTO "+db.MigrationLocksTable+" (table_name) VALUES (?)", db.MigrationsTable); err != nil {
			t.Fatal(err)
		}
		_, err := db.Migrate(ctx, bdb)
		var e *v1.Error
		if !errors.As(err, &e) || e.Code != v1.CodeConflict || !strings.Contains(e.Next, "db unlock") {
			t.Fatalf("残留锁应当让迁移以 conflict 失败并提示 db unlock，得到 %v", err)
		}
		if err := db.Unlock(ctx, bdb); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Migrate(ctx, bdb); err != nil {
			t.Fatalf("清锁后迁移应当正常：%v", err)
		}
	})
}

// 外键真的生效：指向不存在告警的待办在两库都插不进去。
func TestForeignKeysAreEnforced(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		_, err := bdb.ExecContext(context.Background(),
			"INSERT INTO tasks (title, source, alert_id) VALUES (?, ?, ?)", "t", "system", 999)
		if err == nil {
			t.Fatal("指向不存在告警的待办应当被外键拦下")
		}
	})
}
