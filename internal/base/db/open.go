package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "modernc.org/sqlite" // 注册 database/sql 的 sqlite 驱动

	"github.com/satchel/satchel/internal/base/schema"
)

// SQLiteDSN 返回打开某个 SQLite 文件的连接串：WAL、busy_timeout 5 秒、外键约束开启。
func SQLiteDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
}

// Open 按配置打开数据库并 ping 一次。
func Open(ctx context.Context, cfg Config) (*bun.DB, error) {
	return OpenDSN(ctx, cfg.Driver, cfg.DSN())
}

// OpenDSN 用连接串打开数据库。PostgreSQL 会话强制 UTC，读回的时间也是 UTC。
func OpenDSN(ctx context.Context, driver Driver, dsn string) (*bun.DB, error) {
	var bdb *bun.DB
	switch driver {
	case DriverSQLite:
		sqldb, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("打开 SQLite 失败：%w", err)
		}
		bdb = bun.NewDB(sqldb, sqlitedialect.New())
	case DriverPostgres:
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, fmt.Errorf("解析 PostgreSQL 连接串失败：%w", err)
		}
		cfg.RuntimeParams["TimeZone"] = "UTC"
		sqldb := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(scanTimeInUTC))
		bdb = bun.NewDB(sqldb, pgdialect.New())
	default:
		return nil, fmt.Errorf("不支持的数据库驱动 %q", driver)
	}
	if err := bdb.PingContext(ctx); err != nil {
		bdb.Close()
		return nil, fmt.Errorf("连接数据库失败：%w", err)
	}
	return bdb, nil
}

// scanTimeInUTC 让 pgx 把 timestamptz 读成 UTC 的 time.Time，而不是本机时区。
func scanTimeInUTC(_ context.Context, conn *pgx.Conn) error {
	conn.TypeMap().RegisterType(&pgtype.Type{
		Name:  "timestamptz",
		OID:   pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	})
	return nil
}

// DialectOf 返回已打开的库的方言；不认识的返回 0。
func DialectOf(db *bun.DB) schema.Dialect {
	switch db.Dialect().Name() {
	case dialect.SQLite:
		return schema.SQLite
	case dialect.PG:
		return schema.Postgres
	}
	return 0
}
