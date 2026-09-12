package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// ColumnInfo 是库里一列的实际情况。Type 是该方言的类型词（SQLite 声明的类型，PostgreSQL 归一成 DDL 写法）。
type ColumnInfo struct {
	Name    string
	Type    string
	NotNull bool
	Default string
}

// IndexInfo 是库里一个索引；Where 是部分索引的条件原文。
type IndexInfo struct {
	Name    string
	Columns []string
	Unique  bool
	Where   string
}

// ForeignKeyInfo 是库里一条外键；OnDelete 为空表示 NO ACTION。
type ForeignKeyInfo struct {
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
}

// TableInfo 是库里一张表的实际结构。Checks 是表上全部 CHECK 约束的谓词原文（不含 CHECK 关键字）。
type TableInfo struct {
	Name        string
	Columns     []ColumnInfo
	PrimaryKey  []string
	Indexes     []IndexInfo
	ForeignKeys []ForeignKeyInfo
	Checks      []string
}

var internalTables = map[string]bool{MigrationsTable: true, MigrationLocksTable: true}

// Introspect 反查库里的实际表结构，按表名排序；迁移表与 SQLite 的内部表不算。
func Introspect(ctx context.Context, db *bun.DB) ([]TableInfo, error) {
	var tables []TableInfo
	var err error
	switch DialectOf(db) {
	case schema.SQLite:
		tables, err = introspectSQLite(ctx, db)
	case schema.Postgres:
		tables, err = introspectPostgres(ctx, db)
	default:
		return nil, v1.New(v1.CodeInternal, "不认识的数据库方言")
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "反查表结构失败", err)
	}
	return tables, nil
}

// extractChecks 从 CREATE TABLE 语句里扫出每个 CHECK (...) 的谓词，跳过单引号字符串里的内容。
func extractChecks(createSQL string) []string {
	var out []string
	upper := strings.ToUpper(createSQL)
	inQuote := false
	for i := 0; i < len(createSQL); i++ {
		ch := createSQL[i]
		if ch == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote || !strings.HasPrefix(upper[i:], "CHECK") {
			continue
		}
		if i > 0 && isIdentChar(createSQL[i-1]) {
			continue
		}
		j := i + len("CHECK")
		for j < len(createSQL) && (createSQL[j] == ' ' || createSQL[j] == '\t' || createSQL[j] == '\n') {
			j++
		}
		if j >= len(createSQL) || createSQL[j] != '(' {
			continue
		}
		depth, k, quoted := 0, j, false
		for ; k < len(createSQL); k++ {
			switch createSQL[k] {
			case '\'':
				quoted = !quoted
			case '(':
				if !quoted {
					depth++
				}
			case ')':
				if !quoted {
					depth--
				}
			}
			if depth == 0 && !quoted {
				break
			}
		}
		if k >= len(createSQL) {
			break
		}
		out = append(out, strings.TrimSpace(createSQL[j+1:k]))
		i = k
	}
	return out
}

func isIdentChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func introspectSQLite(ctx context.Context, db *bun.DB) ([]TableInfo, error) {
	var names []string
	if err := db.NewRaw("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name").
		Scan(ctx, &names); err != nil {
		return nil, err
	}
	var tables []TableInfo
	for _, name := range names {
		if internalTables[name] {
			continue
		}
		t := TableInfo{Name: name}
		quoted := `"` + name + `"`
		type pkCol struct {
			pos  int
			name string
		}
		var pks []pkCol
		err := eachRow(ctx, db, "PRAGMA table_info("+quoted+")", func(rows *sql.Rows) error {
			var cid, notNull, pk int
			var colName, typ string
			var dflt sql.NullString
			if err := rows.Scan(&cid, &colName, &typ, &notNull, &dflt, &pk); err != nil {
				return err
			}
			ci := ColumnInfo{Name: colName, Type: strings.ToUpper(typ), NotNull: notNull == 1, Default: dflt.String}
			if pk > 0 {
				pks = append(pks, pkCol{pk, colName})
				if ci.Type == "INTEGER" {
					ci.NotNull = true // INTEGER PRIMARY KEY 是 rowid 别名，天然非空
				}
			}
			t.Columns = append(t.Columns, ci)
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
		for _, pk := range pks {
			t.PrimaryKey = append(t.PrimaryKey, pk.name)
		}
		type indexRow struct {
			name   string
			unique bool
		}
		var indexes []indexRow
		err = eachRow(ctx, db, "PRAGMA index_list("+quoted+")", func(rows *sql.Rows) error {
			var seq, unique, partial int
			var ixName, origin string
			if err := rows.Scan(&seq, &ixName, &unique, &origin, &partial); err != nil {
				return err
			}
			if origin == "c" { // 只看 CREATE INDEX 建的，主键与 UNIQUE 约束的自动索引不算
				indexes = append(indexes, indexRow{ixName, unique == 1})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, ix := range indexes {
			ii := IndexInfo{Name: ix.name, Unique: ix.unique}
			err := eachRow(ctx, db, `PRAGMA index_info("`+ix.name+`")`, func(rows *sql.Rows) error {
				var seqno, cid int
				var colName string
				if err := rows.Scan(&seqno, &cid, &colName); err != nil {
					return err
				}
				ii.Columns = append(ii.Columns, colName)
				return nil
			})
			if err != nil {
				return nil, err
			}
			var sqlText string
			if err := db.NewRaw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", ix.name).Scan(ctx, &sqlText); err != nil {
				return nil, err
			}
			if _, where, ok := strings.Cut(sqlText, " WHERE "); ok {
				ii.Where = where
			}
			t.Indexes = append(t.Indexes, ii)
		}
		sort.Slice(t.Indexes, func(i, j int) bool { return t.Indexes[i].Name < t.Indexes[j].Name })
		byID := map[int]*ForeignKeyInfo{}
		var order []int
		err = eachRow(ctx, db, "PRAGMA foreign_key_list("+quoted+")", func(rows *sql.Rows) error {
			var id, seq int
			var refTable, from, to, onUpdate, onDelete, match string
			if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				return err
			}
			fi, ok := byID[id]
			if !ok {
				if onDelete == "NO ACTION" {
					onDelete = ""
				}
				fi = &ForeignKeyInfo{RefTable: refTable, OnDelete: onDelete}
				byID[id] = fi
				order = append(order, id)
			}
			fi.Columns = append(fi.Columns, from)
			fi.RefColumns = append(fi.RefColumns, to)
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Ints(order)
		for _, id := range order {
			t.ForeignKeys = append(t.ForeignKeys, *byID[id])
		}
		var createSQL string
		if err := db.NewRaw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(ctx, &createSQL); err != nil {
			return nil, err
		}
		t.Checks = extractChecks(createSQL)
		tables = append(tables, t)
	}
	return tables, nil
}

// eachRow 用 database/sql 逐行扫描一条查询（PRAGMA 的结果列不固定，不走 bun 的结构体映射）。
func eachRow(ctx context.Context, db *bun.DB, query string, fn func(rows *sql.Rows) error) error {
	rows, err := db.DB.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("%s：%w", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return fmt.Errorf("%s：%w", query, err)
		}
	}
	return rows.Err()
}

// PostgreSQL 的 information_schema 类型名归一成 DDL 里的写法。
var pgTypeNames = map[string]string{"timestamp with time zone": "TIMESTAMPTZ"}

var pgOnDelete = map[string]string{"a": "", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}

func introspectPostgres(ctx context.Context, db *bun.DB) ([]TableInfo, error) {
	var names []string
	if err := db.NewRaw("SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name").
		Scan(ctx, &names); err != nil {
		return nil, err
	}
	var tables []TableInfo
	for _, name := range names {
		if internalTables[name] {
			continue
		}
		t := TableInfo{Name: name}
		var cols []struct {
			Name     string  `bun:"column_name"`
			Type     string  `bun:"data_type"`
			Nullable string  `bun:"is_nullable"`
			Default  *string `bun:"column_default"`
		}
		if err := db.NewRaw("SELECT column_name, data_type, is_nullable, column_default FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? ORDER BY ordinal_position", name).
			Scan(ctx, &cols); err != nil {
			return nil, err
		}
		for _, c := range cols {
			typ, ok := pgTypeNames[c.Type]
			if !ok {
				typ = strings.ToUpper(c.Type)
			}
			ci := ColumnInfo{Name: c.Name, Type: typ, NotNull: c.Nullable == "NO"}
			if c.Default != nil {
				ci.Default = *c.Default
			}
			t.Columns = append(t.Columns, ci)
		}
		if err := db.NewRaw(`SELECT a.attname FROM pg_index ix
			JOIN pg_class c ON c.oid = ix.indrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
			WHERE ix.indisprimary AND n.nspname = current_schema() AND c.relname = ?
			ORDER BY k.ord`, name).Scan(ctx, &t.PrimaryKey); err != nil {
			return nil, err
		}
		var indexes []struct {
			Name    string `bun:"name"`
			Unique  bool   `bun:"unique"`
			Where   string `bun:"where"`
			Columns string `bun:"columns"`
		}
		if err := db.NewRaw(`SELECT i.relname AS name, ix.indisunique AS "unique",
			COALESCE(pg_get_expr(ix.indpred, ix.indrelid), '') AS "where",
			(SELECT string_agg(a.attname, ',' ORDER BY k.ord)
				FROM unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord)
				JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum) AS columns
			FROM pg_index ix
			JOIN pg_class c ON c.oid = ix.indrelid
			JOIN pg_class i ON i.oid = ix.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE NOT ix.indisprimary AND n.nspname = current_schema() AND c.relname = ?
			ORDER BY i.relname`, name).Scan(ctx, &indexes); err != nil {
			return nil, err
		}
		for _, ix := range indexes {
			t.Indexes = append(t.Indexes, IndexInfo{Name: ix.Name, Unique: ix.Unique, Where: ix.Where, Columns: strings.Split(ix.Columns, ",")})
		}
		var fks []struct {
			Name     string `bun:"conname"`
			Column   string `bun:"col"`
			RefTable string `bun:"ref_table"`
			RefCol   string `bun:"ref_col"`
			OnDelete string `bun:"confdeltype"`
			Ord      int    `bun:"ord"`
		}
		if err := db.NewRaw(`SELECT con.conname, a.attname AS col, fc.relname AS ref_table, fa.attname AS ref_col, con.confdeltype, k.ord
			FROM pg_constraint con
			JOIN pg_class c ON c.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_class fc ON fc.oid = con.confrelid
			JOIN unnest(con.conkey, con.confkey) WITH ORDINALITY AS k(attnum, fattnum, ord) ON true
			JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
			JOIN pg_attribute fa ON fa.attrelid = con.confrelid AND fa.attnum = k.fattnum
			WHERE con.contype = 'f' AND n.nspname = current_schema() AND c.relname = ?
			ORDER BY con.conname, k.ord`, name).Scan(ctx, &fks); err != nil {
			return nil, err
		}
		byName := map[string]*ForeignKeyInfo{}
		var order []string
		for _, fk := range fks {
			fi, ok := byName[fk.Name]
			if !ok {
				fi = &ForeignKeyInfo{RefTable: fk.RefTable, OnDelete: pgOnDelete[fk.OnDelete]}
				byName[fk.Name] = fi
				order = append(order, fk.Name)
			}
			fi.Columns = append(fi.Columns, fk.Column)
			fi.RefColumns = append(fi.RefColumns, fk.RefCol)
		}
		for _, n := range order {
			t.ForeignKeys = append(t.ForeignKeys, *byName[n])
		}
		var checks []string
		if err := db.NewRaw(`SELECT pg_get_constraintdef(con.oid)
			FROM pg_constraint con
			JOIN pg_class c ON c.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE con.contype = 'c' AND n.nspname = current_schema() AND c.relname = ?
			ORDER BY con.conname`, name).Scan(ctx, &checks); err != nil {
			return nil, err
		}
		for _, def := range checks {
			t.Checks = append(t.Checks, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(def), "CHECK")))
		}
		tables = append(tables, t)
	}
	return tables, nil
}
