package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// PGServer 是一个 PostgreSQL 目标库的概况（database test 与迁移的前提检查用）。
type PGServer struct {
	Version string   `json:"server_version"`
	Major   int      `json:"-"`
	Tables  []string `json:"-"`
}

// PGInfo 读目标库的版本与当前 schema 里的表名；只读。
func PGInfo(ctx context.Context, dst *bun.DB) (PGServer, error) {
	var info PGServer
	var num string
	if err := dst.QueryRowContext(ctx, "SHOW server_version").Scan(&info.Version); err != nil {
		return info, v1.Wrap(v1.CodeDatabase, "读取目标库版本失败", err)
	}
	if err := dst.QueryRowContext(ctx, "SHOW server_version_num").Scan(&num); err != nil {
		return info, v1.Wrap(v1.CodeDatabase, "读取目标库版本失败", err)
	}
	n, _ := strconv.Atoi(num)
	info.Major = n / 10000
	rows, err := dst.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() ORDER BY 1")
	if err != nil {
		return info, v1.Wrap(v1.CodeDatabase, "列出目标库的表失败", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return info, v1.Wrap(v1.CodeDatabase, "列出目标库的表失败", err)
		}
		info.Tables = append(info.Tables, name)
	}
	return info, rows.Err()
}

// DropCreated 删掉迁移在目标库上建的全部表（注册表里的表按外键逆序，再加两张迁移记账表），给迁移失败时让目标回到空的。
func DropCreated(ctx context.Context, dst *bun.DB, reg *schema.Registry) error {
	tables := reg.Ordered()
	names := []string{MigrationsTable, MigrationLocksTable}
	for i := len(tables) - 1; i >= 0; i-- {
		names = append(names, tables[i].Name)
	}
	for _, name := range names {
		if _, err := dst.ExecContext(ctx, "DROP TABLE IF EXISTS ? CASCADE", bun.Ident(name)); err != nil {
			return v1.Wrap(v1.CodeDatabase, "清理目标库的表 "+name+" 失败", err)
		}
	}
	return nil
}

// CopyProgress 是拷贝的进度（master-db-migration「进度」里 copying 这一段）。
type CopyProgress struct {
	Table       string `json:"table"`
	TablesDone  int    `json:"tables_done"`
	TablesTotal int    `json:"tables_total"`
	Rows        int64  `json:"rows"`
}

// CopyReport 是拷贝的结果。
type CopyReport struct {
	Tables int   `json:"tables"`
	Rows   int64 `json:"rows"`
}

const (
	copyBatchRows   = 500
	copyBatchParams = 60000 // PostgreSQL 单条语句最多 65535 个参数
)

// CopyToPostgres 把 src（SQLite，通常是迁移拿着写锁的那个事务）里注册表的全部表拷进 dst（PostgreSQL 的一个事务，表已由迁移建好）：
// 按外键依赖顺序逐表、分批多行插入，按列类型转换取值，拷完把自增序列推到最大 id 之后（master-db-migration「拷贝与核对」）。
// 行数核对是单独的 VerifyCounts，调用方在两步之间报告阶段。
// 迁移记账表不在注册表里，不拷。每拷完一张表调一次 report（可为 nil）。提交与回滚由调用方做。
func CopyToPostgres(ctx context.Context, src bun.IDB, dst bun.Tx, reg *schema.Registry, report func(CopyProgress)) (CopyReport, error) {
	tables := reg.Ordered()
	var rep CopyReport
	for i, t := range tables {
		n, err := copyTable(ctx, src, dst, t)
		if err != nil {
			return rep, err
		}
		rep.Tables++
		rep.Rows += n
		if report != nil {
			report(CopyProgress{Table: t.Name, TablesDone: i + 1, TablesTotal: len(tables), Rows: rep.Rows})
		}
	}
	return rep, nil
}

// VerifyCounts 逐表核对 src 与 dst 的行数相同，不同就失败。
func VerifyCounts(ctx context.Context, src, dst bun.IDB, reg *schema.Registry) error {
	for _, t := range reg.Ordered() {
		var a, b int64
		if err := src.QueryRowContext(ctx, "SELECT count(*) FROM ?", bun.Ident(t.Name)).Scan(&a); err != nil {
			return v1.Wrap(v1.CodeDatabase, "核对 "+t.Name+" 的行数失败", err)
		}
		if err := dst.QueryRowContext(ctx, "SELECT count(*) FROM ?", bun.Ident(t.Name)).Scan(&b); err != nil {
			return v1.Wrap(v1.CodeDatabase, "核对 "+t.Name+" 的行数失败", err)
		}
		if a != b {
			return v1.Newf(v1.CodeDatabase, "表 %s 的行数对不上：SQLite %d 行，PostgreSQL %d 行", t.Name, a, b)
		}
	}
	return nil
}

func copyTable(ctx context.Context, src bun.IDB, dst bun.Tx, t *schema.Table) (int64, error) {
	cols := make([]string, len(t.Columns))
	idents := make([]any, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = c.Name
		idents[i] = bun.Ident(c.Name)
	}
	order := t.PrimaryKey
	if len(order) == 0 {
		order = []string{"id"}
	}
	orderIdents := make([]any, len(order))
	for i, c := range order {
		orderIdents[i] = bun.Ident(c)
	}
	rows, err := src.QueryContext(ctx, "SELECT "+placeholders(len(cols))+" FROM ? ORDER BY "+placeholders(len(order)),
		append(append(idents, bun.Ident(t.Name)), orderIdents...)...)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "读取 "+t.Name+" 失败", err)
	}
	defer rows.Close()
	perBatch := copyBatchRows
	if perBatch*len(cols) > copyBatchParams {
		perBatch = copyBatchParams / len(cols)
	}
	var batch [][]any
	var total int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		args := []any{bun.Ident(t.Name)}
		args = append(args, idents...)
		groups := make([]string, len(batch))
		for i, row := range batch {
			groups[i] = "(" + placeholders(len(row)) + ")"
			args = append(args, row...)
		}
		q := "INSERT INTO ? (" + placeholders(len(cols)) + ") VALUES " + strings.Join(groups, ", ")
		if _, err := dst.ExecContext(ctx, q, args...); err != nil {
			return v1.Wrap(v1.CodeDatabase, "写入 "+t.Name+" 失败", err)
		}
		total += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return 0, v1.Wrap(v1.CodeDatabase, "读取 "+t.Name+" 失败", err)
		}
		row := make([]any, len(cols))
		for i, c := range t.Columns {
			v, err := convertValue(c.Type, raw[i])
			if err != nil {
				return 0, v1.Newf(v1.CodeDatabase, "表 %s 的列 %s 的值 %v 转不成 PostgreSQL 的 %s：%v", t.Name, c.Name, raw[i], c.Type, err)
			}
			row[i] = v
		}
		batch = append(batch, row)
		if len(batch) >= perBatch {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "读取 "+t.Name+" 失败", err)
	}
	if err := flush(); err != nil {
		return 0, err
	}
	// 自增主键：把序列推到最大 id 之后，之后新建的行接着编号。
	for _, c := range t.Columns {
		if c.Type != schema.TypeSerial {
			continue
		}
		if _, err := dst.ExecContext(ctx, "SELECT setval(pg_get_serial_sequence(?, ?), max(?)) FROM ? HAVING max(?) IS NOT NULL",
			t.Name, c.Name, bun.Ident(c.Name), bun.Ident(t.Name), bun.Ident(c.Name)); err != nil {
			return 0, v1.Wrap(v1.CodeDatabase, "推进 "+t.Name+" 的序列失败", err)
		}
	}
	return total, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// sqliteTimeLayouts 是 SQLite 里时间文本可能的几种写法（bun 写入的、CURRENT_TIMESTAMP 默认值的、RFC 3339 的）。
var sqliteTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
}

// convertValue 按注册表的列类型把 SQLite 驱动给的值转成写进 PostgreSQL 的值；NULL 原样。
func convertValue(t schema.Type, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch t {
	case schema.TypeBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			return x != 0, nil
		case string:
			return strconv.ParseBool(x)
		}
	case schema.TypeTime:
		switch x := v.(type) {
		case time.Time:
			return x.UTC(), nil
		case string:
			for _, layout := range sqliteTimeLayouts {
				if ts, err := time.Parse(layout, x); err == nil {
					return ts.UTC(), nil
				}
			}
			return nil, fmt.Errorf("认不出的时间写法")
		case []byte:
			return convertValue(t, string(x))
		}
	case schema.TypeJSON, schema.TypeText:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		}
	case schema.TypeBlob:
		switch x := v.(type) {
		case []byte:
			return x, nil
		case string:
			return []byte(x), nil
		}
	case schema.TypeInt, schema.TypeSerial:
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		}
	case schema.TypeFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		}
	}
	return nil, fmt.Errorf("驱动给的是 %T", v)
}
