package db

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
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
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// SQLite 的连接参数：WAL、busy_timeout 5 秒、外键约束、写事务 BEGIN IMMEDIATE、synchronous NORMAL、WAL 文件上限 64MB。
const sqliteParams = "_txlock=immediate" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_pragma=journal_size_limit(67108864)" +
	"&_pragma=foreign_keys(ON)"

// sqliteMaxConns 是 SQLite 连接池上限：WAL 下读可以并发，写由 BEGIN IMMEDIATE 排队。
const sqliteMaxConns = 8

// SQLiteDSN 返回打开某个 SQLite 文件的连接串。路径按 URI 规则逐段转义，
// 含 #、?、%、空格的路径也能打开正确的文件（驱动把 file: 开头的连接串交给 SQLite 的 URI 解析）。
func SQLiteDSN(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return "file:" + strings.Join(segments, "/") + "?" + sqliteParams
}

// Open 按配置打开数据库并 ping 一次。
func Open(ctx context.Context, cfg Config) (*bun.DB, error) {
	return OpenDSN(ctx, cfg.Driver, cfg.DSN())
}

// OpenDSN 用连接串打开数据库。PostgreSQL 会话强制 UTC，读回的时间也是 UTC。
// 失败返回四字段错误，驱动原文只在 Unwrap 链里。
func OpenDSN(ctx context.Context, driver Driver, dsn string) (*bun.DB, error) {
	var bdb *bun.DB
	switch driver {
	case DriverSQLite:
		sqldb, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, v1.Wrap(v1.CodeDatabase, "打开 SQLite 失败", err)
		}
		sqldb.SetMaxOpenConns(sqliteMaxConns)
		sqldb.SetMaxIdleConns(sqliteMaxConns)
		sqldb.SetConnMaxIdleTime(5 * time.Minute)
		bdb = bun.NewDB(sqldb, sqlitedialect.New())
	case DriverPostgres:
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, v1.Wrap(v1.CodeConfig, "PostgreSQL 连接参数不合法", err)
		}
		cfg.RuntimeParams["TimeZone"] = "UTC"
		sqldb := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(scanTimeInUTC))
		bdb = bun.NewDB(sqldb, pgdialect.New())
	default:
		return nil, v1.Newf(v1.CodeConfig, "不支持的数据库驱动 %q，只认 sqlite 与 postgres", driver)
	}
	if err := bdb.PingContext(ctx); err != nil {
		bdb.Close()
		return nil, v1.Wrap(v1.CodeDatabase, "连接数据库失败（驱动 "+string(driver)+"）", err).
			WithNext("检查数据目录里的 database.json、环境变量 SATCHEL_DATABASE_* 与数据库是否在运行")
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
