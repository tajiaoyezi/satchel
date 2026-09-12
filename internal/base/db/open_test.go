package db_test

import (
	"context"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
)

func TestOpenSQLitePragmas(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	ctx := context.Background()
	var fk int
	if err := bdb.NewRaw("PRAGMA foreign_keys").Scan(ctx, &fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("PRAGMA foreign_keys 应当是 1，得到 %d", fk)
	}
	var mode string
	if err := bdb.NewRaw("PRAGMA journal_mode").Scan(ctx, &mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode 应当是 wal，得到 %s", mode)
	}
	if db.DialectOf(bdb) != schema.SQLite {
		t.Fatal("方言应当是 sqlite")
	}
}

func TestOpenPostgresUTC(t *testing.T) {
	bdb := dbtest.OpenPostgres(t)
	var tz string
	if err := bdb.NewRaw("SHOW timezone").Scan(context.Background(), &tz); err != nil {
		t.Fatal(err)
	}
	if tz != "UTC" {
		t.Fatalf("会话时区应当是 UTC，得到 %s", tz)
	}
	if db.DialectOf(bdb) != schema.Postgres {
		t.Fatal("方言应当是 postgres")
	}
}
