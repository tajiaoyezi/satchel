// Package dbtest 是双库测试装置：让同一个测试在 SQLite 与 PostgreSQL 各跑一遍。
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
)

const (
	// EnvDSN 是 PostgreSQL 测试库的连接串；没设就跳过 PostgreSQL 那一遍。
	EnvDSN = "SATCHEL_TEST_PG_DSN"
	// EnvRequirePG 设为 1 时缺 DSN 视为失败，CI 用它保证双库都跑。
	EnvRequirePG = "SATCHEL_TEST_REQUIRE_PG"
)

// ForEach 让 fn 在 SQLite 与 PostgreSQL 各跑一遍，库已迁移到最新。
func ForEach(t *testing.T, fn func(t *testing.T, db *bun.DB)) {
	t.Helper()
	forEach(t, true, fn)
}

// ForEachEmpty 同 ForEach，但库是空的，没有跑迁移。
func ForEachEmpty(t *testing.T, fn func(t *testing.T, db *bun.DB)) {
	t.Helper()
	forEach(t, false, fn)
}

func forEach(t *testing.T, migrated bool, fn func(t *testing.T, db *bun.DB)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		run(t, OpenSQLite(t), migrated, fn)
	})
	t.Run("postgres", func(t *testing.T) {
		run(t, OpenPostgres(t), migrated, fn)
	})
}

func run(t *testing.T, bdb *bun.DB, migrated bool, fn func(t *testing.T, db *bun.DB)) {
	t.Helper()
	if migrated {
		if _, err := db.Migrate(context.Background(), bdb); err != nil {
			t.Fatalf("迁移失败：%v", err)
		}
	}
	fn(t, bdb)
}

// OpenSQLite 在临时目录里打开一个空的 SQLite 库，测试结束时关闭。
func OpenSQLite(t *testing.T) *bun.DB {
	t.Helper()
	bdb, err := db.Open(context.Background(), db.Config{Driver: db.DriverSQLite, Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })
	return bdb
}

// OpenPostgres 按 SATCHEL_TEST_PG_DSN 连接 PostgreSQL，为本测试建一个随机 schema 并把 search_path 指过去，
// 测试结束时 DROP SCHEMA。没设 DSN 时跳过；设了 SATCHEL_TEST_REQUIRE_PG=1 又没 DSN 时失败。
func OpenPostgres(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	skip, fail := pgDecision(dsn, os.Getenv(EnvRequirePG))
	if fail != "" {
		t.Fatal(fail)
	}
	if skip != "" {
		t.Skip(skip)
	}
	ctx := context.Background()
	admin, err := db.OpenDSN(ctx, db.DriverPostgres, dsn)
	if err != nil {
		t.Fatalf("连接 PostgreSQL 失败：%v", err)
	}
	name := "t_" + randomHex(6)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+name); err != nil {
		admin.Close()
		t.Fatalf("建测试 schema 失败：%v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+name+" CASCADE"); err != nil {
			t.Errorf("删测试 schema %s 失败：%v", name, err)
		}
		admin.Close()
	})
	bdb, err := db.OpenDSN(ctx, db.DriverPostgres, withSearchPath(dsn, name))
	if err != nil {
		t.Fatalf("连接测试 schema 失败：%v", err)
	}
	t.Cleanup(func() { bdb.Close() })
	return bdb
}

// pgDecision 决定 PostgreSQL 那一遍是跑、跳过还是失败。
func pgDecision(dsn, require string) (skip, fail string) {
	if dsn != "" {
		return "", ""
	}
	if require == "1" {
		return "", "设置了 " + EnvRequirePG + "=1 但没有 " + EnvDSN + "：缺少 PostgreSQL，双库测试不能只跑 SQLite"
	}
	return "没有设置 " + EnvDSN + "，跳过 PostgreSQL 这一遍；本地起库的方法见 README", ""
}

// withSearchPath 给连接串加上 search_path，URL 与 key=value 两种写法都认。
func withSearchPath(dsn, schema string) string {
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	}
	return dsn + " search_path=" + schema
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
