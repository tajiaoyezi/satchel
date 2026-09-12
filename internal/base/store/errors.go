package store

import (
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"

	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// SQLite 的扩展错误码：SQLITE_CONSTRAINT (19) 加上子码左移 8 位。
const (
	sqliteConstraintCheck      = 275  // SQLITE_CONSTRAINT_CHECK
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintNotNull    = 1299 // SQLITE_CONSTRAINT_NOTNULL
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// PostgreSQL 的 SQLSTATE。
const (
	pgNotNullViolation    = "23502"
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
	pgCheckViolation      = "23514"
)

// SQLite 约束失败的文本（modernc 会再套一层「constraint failed: 」前缀），最后一个冒号后面是细节，
// 末尾括号里是驱动附上的结果码：
//
//	constraint failed: UNIQUE constraint failed: tasks.dedup_key, tasks.status (2067)
//	constraint failed: CHECK constraint failed: dedup_key <> '' (275)
//	constraint failed: NOT NULL constraint failed: alerts.category (1299)
var sqliteDetailRE = regexp.MustCompile(`^(?:.*constraint failed: )(.+?)(?: \(\d+\))?$`)

// PostgreSQL 给列级约束起的名字：<表>_<列>_check、<表>_<列>_check1、<表>_<列>_fkey。
var pgColumnConstraintRE = regexp.MustCompile(`^(.+)_(check\d*|fkey)$`)

// translate 把驱动错误翻译成四字段错误。唯一约束按注册表分类：自然键 → name_taken，
// 其它唯一索引与主键 → conflict 并点名列；外键、CHECK、NOT NULL → bad_request；其它 → database。
// 原始错误只在 Unwrap 链里。
func (s *Store) translate(t *schema.Table, err error) error {
	if err == nil {
		return nil
	}
	var already *v1.Error
	if errors.As(err, &already) {
		return err
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		detail := sqliteDetail(sqliteErr.Error())
		switch sqliteErr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return s.uniqueViolation(t, columnsFromDetail(detail), err)
		case sqliteConstraintCheck:
			return checkFailed(t, columnByCheck(t, detail), err)
		case sqliteConstraintForeignKey:
			return foreignKeyFailed(t, "", err)
		case sqliteConstraintNotNull:
			return notNull(t, strings.Join(columnsFromDetail(detail), ","), err)
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return s.uniqueViolation(t, pgColumns(t, pgErr.ConstraintName), err)
		case pgCheckViolation:
			return checkFailed(t, pgConstraintColumn(t, pgErr.ConstraintName), err)
		case pgForeignKeyViolation:
			return foreignKeyFailed(t, pgConstraintColumn(t, pgErr.ConstraintName), err)
		case pgNotNullViolation:
			return notNull(t, pgErr.ColumnName, err)
		}
	}
	return v1.Wrap(v1.CodeDatabase, "数据库操作失败", err)
}

// sqliteDetail 取 SQLite 错误文本冒号后面的细节（列清单或 CHECK 谓词），解析不出来返回空。
func sqliteDetail(msg string) string {
	m := sqliteDetailRE.FindStringSubmatch(msg)
	if m == nil || strings.Contains(m[1], "constraint failed") {
		return "" // 外键那种没有细节的文本，冒号后面只是又一层「constraint failed」
	}
	return m[1]
}

// columnsFromDetail 把「表.列, 表.列」解析成列名清单。
func columnsFromDetail(detail string) []string {
	if detail == "" {
		return nil
	}
	var cols []string
	for _, part := range strings.Split(detail, ",") {
		part = strings.TrimSpace(part)
		if i := strings.LastIndex(part, "."); i >= 0 {
			part = part[i+1:]
		}
		if part != "" {
			cols = append(cols, part)
		}
	}
	return cols
}

// columnByCheck 按 SQLite 报出的 CHECK 谓词原文在注册表里找到它属于哪一列；生成的 DDL 与注册表文本一致，直接比对。
func columnByCheck(t *schema.Table, predicate string) string {
	if t == nil || predicate == "" {
		return ""
	}
	want := strings.Join(strings.Fields(predicate), " ")
	for _, c := range t.Columns {
		for _, check := range c.Checks() {
			if strings.Join(strings.Fields(check), " ") == want {
				return c.Name
			}
		}
	}
	return ""
}

// pgConstraintColumn 从 PostgreSQL 自动起的约束名 <表>_<列>_check / _fkey 里取列名；对不上返回空。
func pgConstraintColumn(t *schema.Table, constraint string) string {
	if t == nil {
		return ""
	}
	m := pgColumnConstraintRE.FindStringSubmatch(constraint)
	if m == nil || !strings.HasPrefix(m[1], t.Name+"_") {
		return ""
	}
	col := strings.TrimPrefix(m[1], t.Name+"_")
	if _, ok := t.Column(col); !ok {
		return ""
	}
	return col
}

// pgColumns 按 PostgreSQL 报的约束名找到列：唯一索引按注册表里的名字，<表>_pkey 是主键。
func pgColumns(t *schema.Table, constraint string) []string {
	if t == nil {
		return nil
	}
	for _, ix := range t.Indexes {
		if ix.Name == constraint {
			return ix.Columns
		}
	}
	if constraint == t.Name+"_pkey" {
		return t.PKColumns()
	}
	return nil
}

// uniqueViolation 按撞上的列集合在注册表里找唯一索引：自然键报 name_taken，其它报 conflict。
func (s *Store) uniqueViolation(t *schema.Table, cols []string, cause error) error {
	label := "对象"
	if t != nil {
		label = t.Name
		if t.IsKind() {
			label = t.Kind
		}
		if ix := findUniqueIndex(t, cols); ix != nil && ix.NaturalKey {
			return v1.Wrap(v1.CodeNameTaken, label+" 的 "+strings.Join(ix.Columns, ", ")+" 已被占用：同名对象已存在，或还在软删除的保留期内", cause).
				WithState("columns", ix.Columns).
				WithNext("换一个名字，或恢复那条已删除的对象")
		}
	}
	if len(cols) == 0 {
		return v1.Wrap(v1.CodeConflict, label+" 与已有的一行撞上了唯一约束", cause)
	}
	sorted := append([]string(nil), cols...)
	sort.Strings(sorted)
	return v1.Wrap(v1.CodeConflict, label+" 的 "+strings.Join(sorted, ", ")+" 已有相同的值", cause).
		WithState("columns", sorted).
		WithNext("先查出已有的那条对象，再决定是复用它还是换值")
}

func findUniqueIndex(t *schema.Table, cols []string) *schema.Index {
	if len(cols) == 0 {
		return nil
	}
	want := append([]string(nil), cols...)
	sort.Strings(want)
	for i := range t.Indexes {
		ix := &t.Indexes[i]
		if !ix.Unique || len(ix.Columns) != len(want) {
			continue
		}
		have := append([]string(nil), ix.Columns...)
		sort.Strings(have)
		if strings.Join(have, ",") == strings.Join(want, ",") {
			return ix
		}
	}
	return nil
}

func tableLabel(t *schema.Table) string {
	if t == nil {
		return "对象"
	}
	if t.IsKind() {
		return t.Kind
	}
	return t.Name
}

// checkFailed 是 CHECK 失败：能对上列就点名列，并把允许的值（Enum）放进 state。
func checkFailed(t *schema.Table, column string, cause error) error {
	if column == "" {
		return v1.Wrap(v1.CodeBadRequest, tableLabel(t)+" 写入的值不在允许的范围内", cause)
	}
	e := v1.Wrap(v1.CodeBadRequest, tableLabel(t)+" 的字段 "+column+" 的值不在允许的范围内", cause).
		WithState("column", column)
	if c, ok := t.Column(column); ok {
		if len(c.Enum) > 0 {
			e.WithState("allowed", c.Enum)
		} else if c.Check != "" {
			e.WithState("check", c.Check)
		}
	}
	return e
}

func foreignKeyFailed(t *schema.Table, column string, cause error) error {
	if column == "" {
		return v1.Wrap(v1.CodeBadRequest, tableLabel(t)+" 引用的对象不存在，或它还被别的对象引用着", cause)
	}
	return v1.Wrap(v1.CodeBadRequest, tableLabel(t)+" 的字段 "+column+" 引用的对象不存在", cause).
		WithState("column", column)
}

func notNull(t *schema.Table, column string, cause error) error {
	if column == "" {
		return v1.Wrap(v1.CodeBadRequest, tableLabel(t)+" 缺少必填字段", cause)
	}
	return v1.Wrap(v1.CodeBadRequest, tableLabel(t)+" 缺少必填字段 "+column, cause).
		WithState("column", column)
}
