package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
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
	var sync int
	if err := bdb.NewRaw("PRAGMA synchronous").Scan(ctx, &sync); err != nil {
		t.Fatal(err)
	}
	if sync != 1 {
		t.Fatalf("synchronous 应当是 NORMAL（1），得到 %d", sync)
	}
	if db.DialectOf(bdb) != schema.SQLite {
		t.Fatal("方言应当是 sqlite")
	}
	if got := bdb.DB.Stats().MaxOpenConnections; got != 8 {
		t.Fatalf("SQLite 连接池上限应当是 8（design 决策 2），得到 %d", got)
	}
}

// 路径含 #、?、%、空格时库文件仍然落在那个目录里，而不是被 URI 解析截断的路径上。
func TestSQLitePathWithSpecialCharacters(t *testing.T) {
	for _, name := range []string{"hash#tag", "q?mark", "per%cent", "with space", "all #?% here"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := db.EnsureDataDir(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, db.SQLiteFile)
			bdb, err := db.Open(context.Background(), db.Config{Driver: db.DriverSQLite, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer bdb.Close()
			if _, err := db.Migrate(context.Background(), bdb); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("库文件应当在 %s：%v", path, err)
			}
			entries, _ := os.ReadDir(filepath.Dir(dir))
			for _, e := range entries {
				if e.Name() != name {
					t.Errorf("上级目录里多出了 %q，说明路径被截断了", e.Name())
				}
			}
		})
	}
}

// 多个并发事务各自读一行再改它：BEGIN IMMEDIATE 让写锁一开始就拿到，busy_timeout 排队，不会 SQLITE_BUSY。
func TestConcurrentWriteTransactions(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		job := &model.Job{JobID: "j", Kind: "exec", Status: "queued"}
		if _, err := bdb.NewInsert().Model(job).ExcludeColumn("id", "args", "created_at", "updated_at", "resource_version").Exec(ctx); err != nil {
			t.Fatal(err)
		}
		const workers = 16
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
					var current int64
					if err := tx.NewSelect().Table("jobs").Column("resource_version").Where("job_id = ?", "j").Scan(ctx, &current); err != nil {
						return err
					}
					_, err := tx.NewUpdate().Table("jobs").Set("resource_version = ?", current+1).Where("job_id = ?", "j").Exec(ctx)
					return err
				})
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("并发事务失败：%v", err)
			}
		}
		// SQLite 的写事务以 BEGIN IMMEDIATE 开始，天然串行，每个事务都在前一个的基础上加 1；
		// PostgreSQL 是 READ COMMITTED，读后改不加锁会丢失更新，那是它的正常行为，这里只要求全部成功。
		if db.DialectOf(bdb) == schema.SQLite {
			var final int64
			if err := bdb.NewSelect().Table("jobs").Column("resource_version").Where("job_id = ?", "j").Scan(ctx, &final); err != nil {
				t.Fatal(err)
			}
			if final != 1+workers {
				t.Fatalf("BEGIN IMMEDIATE 应当让事务串行，最终应当是 %d，得到 %d", 1+workers, final)
			}
		}
	})
}

func TestOpenFailureIsWrapped(t *testing.T) {
	_, err := db.OpenDSN(context.Background(), db.DriverPostgres, "postgres://u:p@127.0.0.1:1/nope?sslmode=disable&connect_timeout=2")
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != v1.CodeDatabase {
		t.Fatalf("连不上时应当是 database 错误，得到 %v", err)
	}
	if strings.Contains(e.Reason, "refused") || strings.Contains(e.Reason, "dial") {
		t.Fatalf("reason 不该含驱动原文：%q", e.Reason)
	}
	if errors.Unwrap(e) == nil {
		t.Fatal("驱动原文应当在 Unwrap 链里")
	}
	if e.Next == "" {
		t.Fatal("连不上库应当给出下一步提示")
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
