package db

import (
	"context"
	"embed"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"

	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 两套迁移 SQL 按方言分目录，由 go generate ./internal/base/schema/ 从注册表生成。
//
//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationFiles embed.FS

const (
	// MigrationsTable 记录已应用的迁移。
	MigrationsTable = "satchel_migrations"
	// MigrationLocksTable 是迁移锁表。
	MigrationLocksTable = "satchel_migration_locks"
)

func discover(d schema.Dialect) (*migrate.Migrations, error) {
	if d == 0 {
		return nil, v1.New(v1.CodeInternal, "不认识的数据库方言")
	}
	sub, err := fs.Sub(migrationFiles, "migrations/"+d.String())
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "读取迁移文件目录失败", err)
	}
	ms := migrate.NewMigrations()
	if err := ms.Discover(sub); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "读取 "+d.String()+" 的迁移文件失败", err)
	}
	return ms, nil
}

// MigrationNames 返回某方言内置的全部迁移名，按序号排序；不需要打开数据库。
func MigrationNames(d schema.Dialect) ([]string, error) {
	ms, err := discover(d)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, m := range ms.Sorted() {
		names = append(names, m.String())
	}
	sort.Strings(names)
	return names, nil
}

func newMigrator(db *bun.DB) (*migrate.Migrator, error) {
	ms, err := discover(DialectOf(db))
	if err != nil {
		return nil, err
	}
	return migrate.NewMigrator(db, ms,
		migrate.WithTableName(MigrationsTable),
		migrate.WithLocksTableName(MigrationLocksTable),
		migrate.WithMarkAppliedOnSuccess(true),
	), nil
}

// Migrate 执行全部未应用的迁移并比对库结构，返回本次应用的迁移名；没有待应用的返回空。
// CLI 的 db migrate 与 serve 启动都用它，结构比对不另外接。
// 迁移记账缺失而库里已有表（旧数据目录、手工删过迁移表）时不动库，直接报 schema_mismatch；
// 迁移跑完后库结构与注册表不一致时，已应用的迁移名放在错误的 state.applied 里。
func Migrate(ctx context.Context, db *bun.DB) ([]string, error) {
	bookkept, err := tableExists(ctx, db, MigrationsTable)
	if err != nil {
		return nil, err
	}
	if !bookkept {
		tables, err := Introspect(ctx, db)
		if err != nil {
			return nil, err
		}
		if len(tables) > 0 {
			return nil, bookkeepingMissingError(tables, Diff(schema.Default(), DialectOf(db), tables))
		}
	}
	m, err := newMigrator(db)
	if err != nil {
		return nil, err
	}
	if err := m.Init(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "初始化迁移表失败", err)
	}
	if err := m.Lock(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeConflict, "获取迁移锁失败：另一次迁移正在进行，或上一次迁移被中断后锁没有释放", err).
			WithState("locksTable", MigrationLocksTable).
			WithNext("确认没有别的迁移在跑之后，用 satchel db unlock 清掉残留的锁")
	}
	defer m.Unlock(ctx) //nolint:errcheck // 解锁失败没有可做的事，下一次 Lock 会报
	group, err := m.Migrate(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "执行迁移失败", err)
	}
	names := []string{}
	for _, mig := range group.Migrations {
		names = append(names, mig.String())
	}
	check, err := Check(ctx, db)
	if err != nil {
		return names, err
	}
	if !*check.Consistent {
		return names, MismatchError(check.Diff).WithState("applied", names)
	}
	return names, nil
}

func bookkeepingMissingError(tables []TableInfo, diff []string) *v1.Error {
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	e := v1.Newf(v1.CodeSchemaMismatch, "库里已有 %d 张表，却没有迁移记账（%s 表不存在），不能在它上面执行迁移", len(tables), MigrationsTable).
		WithState("tables", names).
		WithState("diff", diff).
		WithNext("这是按旧结构建的库或被手工改过：首个正式发布之前删掉库重新迁移（README「开发期删库重建」）")
	if len(diff) > 0 {
		e.Reason += "；它与注册表另有 " + strconv.Itoa(len(diff)) + " 处不一致"
	}
	return e
}

// Unlock 清除残留的迁移锁；锁表不存在时先建表。
func Unlock(ctx context.Context, db *bun.DB) error {
	m, err := newMigrator(db)
	if err != nil {
		return err
	}
	if err := m.Init(ctx); err != nil {
		return v1.Wrap(v1.CodeDatabase, "初始化迁移表失败", err)
	}
	if err := m.Unlock(ctx); err != nil {
		return v1.Wrap(v1.CodeDatabase, "清除迁移锁失败", err)
	}
	return nil
}

// SchemaCheck 是库结构与注册表的比对结果。
// Checked 为 false 表示没有比对（还有迁移没应用），这时 Consistent 是 null 而不是 false。
// Diff 是注册表里的表在库里对不上的地方；ExtraTables 是库里有、注册表里没有的表，
// 只提示不算错误（共用一个 PostgreSQL 库时会有别的应用的表）。
type SchemaCheck struct {
	Checked     bool     `json:"checked"`
	Consistent  *bool    `json:"consistent"`
	Diff        []string `json:"diff"`
	ExtraTables []string `json:"extraTables"`
}

// MigrationStatus 是已应用与待应用的迁移名，以及结构比对结果。
// BookkeepingMissing 为 true 表示库里有表却没有迁移记账，这时 Pending 是全部迁移，Schema 照常比对。
type MigrationStatus struct {
	Applied            []string    `json:"applied"`
	Pending            []string    `json:"pending"`
	BookkeepingMissing bool        `json:"bookkeepingMissing"`
	Schema             SchemaCheck `json:"schema"`
}

func uncheckedSchema() SchemaCheck {
	return SchemaCheck{Diff: []string{}, ExtraTables: []string{}}
}

// Status 列出迁移状态，没有任何写盘副作用：迁移表不存在时按内置清单报全部待应用，不建表。
// 全部迁移都已应用、或迁移记账缺失而库里已有表时，比对库结构与注册表。
func Status(ctx context.Context, db *bun.DB) (MigrationStatus, error) {
	st := MigrationStatus{Applied: []string{}, Pending: []string{}, Schema: uncheckedSchema()}
	exists, err := tableExists(ctx, db, MigrationsTable)
	if err != nil {
		return st, err
	}
	if !exists {
		if st.Pending, err = MigrationNames(DialectOf(db)); err != nil {
			return st, err
		}
		tables, err := Introspect(ctx, db)
		if err != nil {
			return st, err
		}
		if len(tables) > 0 {
			st.BookkeepingMissing = true
			st.Schema = checkTables(DialectOf(db), tables)
		}
		return st, nil
	}
	m, err := newMigrator(db)
	if err != nil {
		return st, err
	}
	ms, err := m.MigrationsWithStatus(ctx)
	if err != nil {
		return st, v1.Wrap(v1.CodeDatabase, "读取迁移状态失败", err)
	}
	for _, mig := range ms.Applied() {
		st.Applied = append(st.Applied, mig.String())
	}
	for _, mig := range ms.Unapplied() {
		st.Pending = append(st.Pending, mig.String())
	}
	if len(st.Pending) == 0 {
		if st.Schema, err = Check(ctx, db); err != nil {
			return st, err
		}
	}
	return st, nil
}

// Check 把库里的实际结构与注册表比对。Consistent 只看 Diff：多出来的表不算不一致。
func Check(ctx context.Context, db *bun.DB) (SchemaCheck, error) {
	actual, err := Introspect(ctx, db)
	if err != nil {
		return uncheckedSchema(), err
	}
	return checkTables(DialectOf(db), actual), nil
}

func checkTables(d schema.Dialect, actual []TableInfo) SchemaCheck {
	reg := schema.Default()
	diff := Diff(reg, d, actual)
	if diff == nil {
		diff = []string{}
	}
	extra := ExtraTables(reg, actual)
	if extra == nil {
		extra = []string{}
	}
	consistent := len(diff) == 0
	return SchemaCheck{Checked: true, Consistent: &consistent, Diff: diff, ExtraTables: extra}
}

// MismatchError 把结构差异包成四字段错误：code schema_mismatch，差异清单在 state.diff 里。
func MismatchError(diff []string) *v1.Error {
	return v1.Newf(v1.CodeSchemaMismatch, "库结构与注册表有 %d 处不一致：%s", len(diff), strings.Join(diff, "；")).
		WithState("diff", diff).
		WithNext("首个正式发布之前：删掉库重新迁移（README「开发期删库重建」）；之后：写新的迁移文件")
}

func tableExists(ctx context.Context, db *bun.DB, name string) (bool, error) {
	var query string
	switch DialectOf(db) {
	case schema.SQLite:
		query = "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?"
	case schema.Postgres:
		query = "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?"
	default:
		return false, v1.New(v1.CodeInternal, "不认识的数据库方言")
	}
	var n int
	if err := db.NewRaw(query, name).Scan(ctx, &n); err != nil {
		return false, v1.Wrap(v1.CodeDatabase, "查询表是否存在失败", err)
	}
	return n > 0, nil
}
