package db

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/satchel/satchel/internal/base/schema"
)

// Diff 把注册表里的每张表与库里的实际结构逐项比对（列、类型、可空、默认值、CHECK、主键、唯一、
// 索引与部分索引条件、外键），返回差异说明；空表示一致。库里多出来的表不在这里，见 ExtraTables。
func Diff(reg *schema.Registry, d schema.Dialect, actual []TableInfo) []string {
	var out []string
	byName := map[string]TableInfo{}
	for _, t := range actual {
		byName[t.Name] = t
	}
	for _, t := range reg.Tables() {
		a, ok := byName[t.Name]
		if !ok {
			out = append(out, fmt.Sprintf("表 %s：注册表里有，库里没有", t.Name))
			continue
		}
		out = append(out, diffTable(t, d, a)...)
	}
	return out
}

// ExtraTables 返回库里有、注册表里没有的表名，按名字排序。
func ExtraTables(reg *schema.Registry, actual []TableInfo) []string {
	var out []string
	for _, t := range actual {
		if _, ok := reg.Table(t.Name); !ok {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	return out
}

func diffTable(t *schema.Table, d schema.Dialect, a TableInfo) []string {
	var out []string
	report := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("表 %s 的", t.Name)+fmt.Sprintf(format, args...))
	}
	actualCols := map[string]ColumnInfo{}
	for _, c := range a.Columns {
		actualCols[c.Name] = c
	}
	for _, c := range t.Columns {
		ac, ok := actualCols[c.Name]
		if !ok {
			report("列 %s：注册表里有，库里没有", c.Name)
			continue
		}
		if want := schema.TypeName(c.Type, d); !strings.EqualFold(ac.Type, want) {
			report("列 %s 类型：注册表 %s，库里 %s", c.Name, want, ac.Type)
		}
		if ac.NotNull != !c.Nullable {
			report("列 %s 可空：注册表 %v，库里 %v", c.Name, c.Nullable, !ac.NotNull)
		}
		if c.Type != schema.TypeSerial && normalizeDefault(ac.Default) != normalizeDefault(c.Default) {
			report("列 %s 默认值：注册表 %q，库里 %q", c.Name, c.Default, ac.Default)
		}
	}
	for _, ac := range a.Columns {
		if _, ok := t.Column(ac.Name); !ok {
			report("列 %s：库里有，注册表里没有", ac.Name)
		}
	}
	if want, got := strings.Join(t.PKColumns(), ","), strings.Join(a.PrimaryKey, ","); want != got {
		report("主键：注册表 (%s)，库里 (%s)", want, got)
	}
	actualIdx := map[string]IndexInfo{}
	for _, ix := range a.Indexes {
		actualIdx[ix.Name] = ix
	}
	for _, ix := range t.Indexes {
		ai, ok := actualIdx[ix.Name]
		if !ok {
			report("索引 %s：注册表里有，库里没有", ix.Name)
			continue
		}
		if strings.Join(ix.Columns, ",") != strings.Join(ai.Columns, ",") {
			report("索引 %s 的列：注册表 (%s)，库里 (%s)", ix.Name, strings.Join(ix.Columns, ","), strings.Join(ai.Columns, ","))
		}
		if ix.Unique != ai.Unique {
			report("索引 %s 唯一：注册表 %v，库里 %v", ix.Name, ix.Unique, ai.Unique)
		}
		if normalizePredicate(ix.Where) != normalizePredicate(ai.Where) {
			report("索引 %s 的部分索引条件：注册表 %q，库里 %q", ix.Name, ix.Where, ai.Where)
		}
	}
	for _, ai := range a.Indexes {
		found := false
		for _, ix := range t.Indexes {
			if ix.Name == ai.Name {
				found = true
			}
		}
		if !found {
			report("索引 %s：库里有，注册表里没有", ai.Name)
		}
	}
	want := map[string]bool{}
	for _, fk := range t.ForeignKeys {
		want[fkKey(fk.Columns, fk.RefTable, fk.RefColumns, fk.OnUpdate, fk.OnDelete)] = true
	}
	got := map[string]bool{}
	for _, fk := range a.ForeignKeys {
		got[fkKey(fk.Columns, fk.RefTable, fk.RefColumns, fk.OnUpdate, fk.OnDelete)] = true
	}
	for k := range want {
		if !got[k] {
			report("外键 %s：注册表里有，库里没有", k)
		}
	}
	for k := range got {
		if !want[k] {
			report("外键 %s：库里有，注册表里没有", k)
		}
	}
	// CHECK 约束：注册表侧按列展开（Enum 与 Check），库侧是表级清单；归一后按集合比对。
	type expectedCheck struct{ column, expr string }
	wantChecks := map[string][]expectedCheck{}
	for _, c := range t.Columns {
		for _, expr := range c.Checks() {
			key := normalizePredicate(expr)
			wantChecks[key] = append(wantChecks[key], expectedCheck{c.Name, expr})
		}
	}
	gotChecks := map[string][]string{}
	for _, expr := range a.Checks {
		key := normalizePredicate(expr)
		gotChecks[key] = append(gotChecks[key], expr)
	}
	for key, list := range wantChecks {
		if len(gotChecks[key]) < len(list) {
			report("列 %s 的 CHECK (%s)：注册表里有，库里没有", list[0].column, list[0].expr)
		}
	}
	for key, list := range gotChecks {
		if len(wantChecks[key]) < len(list) {
			report("CHECK (%s)：库里有，注册表里没有", list[0])
		}
	}
	sort.Strings(out)
	return out
}

func fkKey(cols []string, refTable string, refCols []string, onUpdate, onDelete string) string {
	k := fmt.Sprintf("(%s) → %s (%s)", strings.Join(cols, ","), refTable, strings.Join(refCols, ","))
	if onUpdate != "" {
		k += " ON UPDATE " + onUpdate
	}
	if onDelete != "" {
		k += " ON DELETE " + onDelete
	}
	return k
}

var (
	// PostgreSQL 会给默认值与表达式加类型转换，比对前去掉。
	castRE = regexp.MustCompile(`::(text|bigint|integer|jsonb|boolean|double precision|bytea|timestamp with time zone|regclass)`)
	// PostgreSQL 把 x IN (...) 改写成 x = ANY (ARRAY[...])，比对前改回来。
	anyArrayRE = regexp.MustCompile(`=\s*ANY\s*\(\s*ARRAY\[([^\]]*)\]\s*\)`)
	spaceParen = strings.NewReplacer(" ", "", "\t", "", "\n", "", "(", "", ")", "")
	// PostgreSQL 把负数默认值存成带引号的字面量（'-1'::integer），去掉类型转换后再去引号。
	quotedNumberRE = regexp.MustCompile(`^'(-?\d+(?:\.\d+)?)'$`)
)

func normalizeDefault(s string) string {
	s = strings.TrimSpace(castRE.ReplaceAllString(s, ""))
	s = quotedNumberRE.ReplaceAllString(s, "$1")
	return strings.ToLower(s)
}

// normalizePredicate 把部分索引条件归一成可比对的形式：去类型转换、ANY(ARRAY) 改回 IN、小写、去空白与括号。
// 只覆盖注册表里用到的写法；出现新写法时一致性测试会报差异，到时再扩。
func normalizePredicate(s string) string {
	s = castRE.ReplaceAllString(s, "")
	s = anyArrayRE.ReplaceAllString(s, "IN ($1)")
	return spaceParen.Replace(strings.ToLower(s))
}
