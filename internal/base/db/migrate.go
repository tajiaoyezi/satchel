package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
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

func newMigrator(db *bun.DB) (*migrate.Migrator, error) {
	d := DialectOf(db)
	if d == 0 {
		return nil, fmt.Errorf("不认识的数据库方言 %s", db.Dialect().Name())
	}
	sub, err := fs.Sub(migrationFiles, "migrations/"+d.String())
	if err != nil {
		return nil, err
	}
	ms := migrate.NewMigrations()
	if err := ms.Discover(sub); err != nil {
		return nil, fmt.Errorf("读取 %s 的迁移文件失败：%w", d, err)
	}
	return migrate.NewMigrator(db, ms,
		migrate.WithTableName(MigrationsTable),
		migrate.WithLocksTableName(MigrationLocksTable),
		migrate.WithMarkAppliedOnSuccess(true),
	), nil
}

// Migrate 执行全部未应用的迁移，返回本次应用的迁移名；没有待应用的返回空。
func Migrate(ctx context.Context, db *bun.DB) ([]string, error) {
	m, err := newMigrator(db)
	if err != nil {
		return nil, err
	}
	if err := m.Init(ctx); err != nil {
		return nil, fmt.Errorf("初始化迁移表失败：%w", err)
	}
	if err := m.Lock(ctx); err != nil {
		return nil, fmt.Errorf("获取迁移锁失败：%w", err)
	}
	defer m.Unlock(ctx) //nolint:errcheck // 解锁失败没有可做的事，下一次 Lock 会报
	group, err := m.Migrate(ctx)
	if err != nil {
		return nil, fmt.Errorf("执行迁移失败：%w", err)
	}
	names := make([]string, 0, len(group.Migrations))
	for _, mig := range group.Migrations {
		names = append(names, mig.String())
	}
	return names, nil
}

// MigrationStatus 是已应用与待应用的迁移名。
type MigrationStatus struct {
	Applied []string `json:"applied"`
	Pending []string `json:"pending"`
}

// Status 列出迁移状态。
func Status(ctx context.Context, db *bun.DB) (MigrationStatus, error) {
	var st MigrationStatus
	m, err := newMigrator(db)
	if err != nil {
		return st, err
	}
	if err := m.Init(ctx); err != nil {
		return st, fmt.Errorf("初始化迁移表失败：%w", err)
	}
	ms, err := m.MigrationsWithStatus(ctx)
	if err != nil {
		return st, fmt.Errorf("读取迁移状态失败：%w", err)
	}
	st.Applied = []string{}
	st.Pending = []string{}
	for _, mig := range ms.Applied() {
		st.Applied = append(st.Applied, mig.String())
	}
	for _, mig := range ms.Unapplied() {
		st.Pending = append(st.Pending, mig.String())
	}
	return st, nil
}
