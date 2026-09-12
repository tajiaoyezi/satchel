package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Type 是列的抽象类型；每种在两库各写死一种表达，见 TypeName。
type Type int

const (
	TypeSerial Type = iota + 1 // 自增主键
	TypeInt                    // 64 位整数
	TypeFloat                  // 双精度小数
	TypeBool                   // 布尔
	TypeTime                   // 时间，UTC
	TypeJSON                   // JSON 文档
	TypeText                   // 文本
	TypeBlob                   // 二进制
)

var typeNames = map[Type]string{
	TypeSerial: "serial", TypeInt: "int", TypeFloat: "float", TypeBool: "bool",
	TypeTime: "time", TypeJSON: "json", TypeText: "text", TypeBlob: "blob",
}

func (t Type) String() string {
	if s, ok := typeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("Type(%d)", int(t))
}

// Class 是列在第 07 章五类操作表里的分档。零值是 spec：没标的默认 spec。
type Class int

const (
	ClassSpec       Class = iota // spec：经 plan/apply 写
	ClassMeta                    // 元数据：id、时间戳、版本、软删除
	ClassStatus                  // status：只由系统写
	ClassAction                  // 动作专属：只由动作类操作写
	ClassHuman                   // 人类专属：只由第 05 章七组操作写
	ClassMasterSelf              // 主控自身类
	ClassReadOnly                // 只读展示
)

var classNames = [...]string{"spec", "meta", "status", "action", "human", "master_self", "readonly"}

func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// KindClass 是 kind 的操作类别（第 07 章五类操作表）。零值表示这张表不是 kind。
type KindClass int

const (
	KindNone           KindClass = iota // 不是 kind：关联表、计数表
	KindConfig                          // 配置类
	KindAction                          // 动作类
	KindMasterSettings                  // 主控设置类
	KindSystem                          // 系统只写
	KindReadOnly                        // 只读展示
)

var kindClassNames = [...]string{"", "config", "action", "master_settings", "system", "readonly"}

func (k KindClass) String() string {
	if int(k) < len(kindClassNames) {
		return kindClassNames[k]
	}
	return fmt.Sprintf("KindClass(%d)", int(k))
}

// Origin 记录表从哪来。
type Origin int

const (
	OriginSatchel Origin = iota // Satchel 新增
	OriginMMWX                  // 来自 mmwx，照抄或改造
)

// Column 是一列。
type Column struct {
	Name     string
	Type     Type
	Nullable bool
	// Default 是 DDL 里的默认值字面量，例如 1、''、FALSE、CURRENT_TIMESTAMP；空表示没有默认值。
	Default string
	Class   Class
	// Enum 非空时生成 CHECK (col IN (...))，只用于文本列。
	Enum []string
	// Check 非空时生成 CHECK (<Check>)，任意谓词，如 dedup_key <> ''。
	Check string
	// Masked 表示输出时按第 05 章打码。
	Masked bool
	// Immutable 表示创建后不能改（自然主键，如 users.username）：UpdateSpec 与 apply 都拒绝改它。只能标在 spec 列上。
	Immutable bool
}

// Checks 返回这一列的全部 CHECK 谓词：Enum 展开成 IN 列表，Check 原样。
func (c Column) Checks() []string {
	var out []string
	if len(c.Enum) > 0 {
		quoted := make([]string, len(c.Enum))
		for i, v := range c.Enum {
			quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		out = append(out, c.Name+" IN ("+strings.Join(quoted, ", ")+")")
	}
	if c.Check != "" {
		out = append(out, c.Check)
	}
	return out
}

// Index 是一个索引；Where 非空就是部分索引。
// NaturalKey 标记这是 kind 的自然键唯一索引：撞上它报 name_taken，其它唯一约束报 conflict。
type Index struct {
	Name       string
	Columns    []string
	Unique     bool
	Where      string
	NaturalKey bool
}

// ForeignKey 是一条外键。OnDelete 取 CASCADE、SET NULL、RESTRICT 或空（数据库默认 NO ACTION）。
type ForeignKey struct {
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
}

// Table 是一张表。Kind 非空表示它背后是一个 kind，KindClass 必须同时给出。
type Table struct {
	Name string
	Kind string
	// GoName 是 bun 模型的结构体名；空时取 Kind。
	GoName    string
	KindClass KindClass
	Origin    Origin
	// AppendOnly 的表只能插入，写入原语拒绝一切更新与删除。
	AppendOnly bool
	Columns    []Column
	// PrimaryKey 为空时主键是名为 id 的自增列。
	PrimaryKey  []string
	Indexes     []Index
	ForeignKeys []ForeignKey
}

// Column 按名字找列。
func (t *Table) Column(name string) (*Column, bool) {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i], true
		}
	}
	return nil, false
}

// PKColumns 返回主键列。
func (t *Table) PKColumns() []string {
	if len(t.PrimaryKey) > 0 {
		return t.PrimaryKey
	}
	return []string{"id"}
}

// IsKind 报告这张表背后是不是一个 kind。
func (t *Table) IsKind() bool { return t.Kind != "" }

// HasVersion 报告这张表有没有 resource_version 列。
func (t *Table) HasVersion() bool {
	_, ok := t.Column("resource_version")
	return ok
}

// 公共列按名字自动归 meta 档。
var metaColumns = map[string]bool{"id": true, "created_at": true, "updated_at": true, "resource_version": true, "deleted_at": true}

// Registry 是表注册表：表结构的唯一来源。
type Registry struct {
	tables map[string]*Table
	names  []string // 登记顺序
	dups   []string
}

// New 建一个空注册表。
func New() *Registry {
	return &Registry{tables: map[string]*Table{}}
}

// Add 登记一张表。配置类与动作类的 kind 表自动补 resource_version 与 deleted_at，
// 主控设置类只补 resource_version；公共列自动归 meta 档。
func (r *Registry) Add(t Table) {
	t.Columns = append([]Column(nil), t.Columns...)
	if t.GoName == "" {
		t.GoName = t.Kind
	}
	switch t.KindClass {
	case KindConfig, KindAction:
		t.addColumn(Column{Name: "resource_version", Type: TypeInt, Default: "1"})
		t.addColumn(Column{Name: "deleted_at", Type: TypeTime, Nullable: true})
	case KindMasterSettings:
		t.addColumn(Column{Name: "resource_version", Type: TypeInt, Default: "1"})
	}
	for i := range t.Columns {
		if metaColumns[t.Columns[i].Name] {
			t.Columns[i].Class = ClassMeta
		}
	}
	if _, exists := r.tables[t.Name]; exists {
		r.dups = append(r.dups, t.Name)
		return
	}
	r.tables[t.Name] = &t
	r.names = append(r.names, t.Name)
}

func (t *Table) addColumn(c Column) {
	if _, exists := t.Column(c.Name); exists {
		return
	}
	t.Columns = append(t.Columns, c)
}

// Table 按名字找表。
func (r *Registry) Table(name string) (*Table, bool) {
	t, ok := r.tables[name]
	return t, ok
}

// Tables 返回全部表，按外键拓扑排序：被引用的表在前，同级按名字。
// 有环时环里的表按名字排在最后，环本身由 Validate 报出。
func (r *Registry) Tables() []*Table {
	names := append([]string(nil), r.names...)
	sort.Strings(names)
	indeg := map[string]int{}
	next := map[string][]string{}
	for _, name := range names {
		indeg[name] += 0
		for _, fk := range r.tables[name].ForeignKeys {
			if fk.RefTable == name {
				continue
			}
			if _, ok := r.tables[fk.RefTable]; !ok {
				continue
			}
			indeg[name]++
			next[fk.RefTable] = append(next[fk.RefTable], name)
		}
	}
	var ready, out []string
	for _, name := range names {
		if indeg[name] == 0 {
			ready = append(ready, name)
		}
	}
	for len(ready) > 0 {
		sort.Strings(ready)
		name := ready[0]
		ready = ready[1:]
		out = append(out, name)
		for _, n := range next[name] {
			indeg[n]--
			if indeg[n] == 0 {
				ready = append(ready, n)
			}
		}
	}
	done := map[string]bool{}
	for _, name := range out {
		done[name] = true
	}
	for _, name := range names {
		if !done[name] {
			out = append(out, name)
		}
	}
	tables := make([]*Table, len(out))
	for i, name := range out {
		tables[i] = r.tables[name]
	}
	return tables
}

var validOnDelete = map[string]bool{"": true, "CASCADE": true, "SET NULL": true, "RESTRICT": true}

// Validate 检查注册表；有问题时把全部问题合成一个错误返回。
func (r *Registry) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	for _, name := range r.dups {
		add("表 %s 登记了两次", name)
	}
	indexNames := map[string]string{}
	for _, name := range r.names {
		t := r.tables[name]
		if t.Name == "" {
			add("有一张表没有名字")
			continue
		}
		if (t.Kind == "") != (t.KindClass == KindNone) {
			add("表 %s：Kind 与 KindClass 要么都给，要么都不给", t.Name)
		}
		if t.GoName == "" {
			add("表 %s 缺少 GoName", t.Name)
		}
		if t.AppendOnly && t.KindClass != KindNone && t.KindClass != KindSystem {
			add("表 %s：append-only 的表只能是系统只写的 kind 或不是 kind", t.Name)
		}
		if len(t.Columns) == 0 {
			add("表 %s 没有列", t.Name)
		}
		seen := map[string]bool{}
		for _, c := range t.Columns {
			if seen[c.Name] {
				add("表 %s 的列 %s 重复", t.Name, c.Name)
			}
			seen[c.Name] = true
			if c.Type < TypeSerial || c.Type > TypeBlob {
				add("表 %s 的列 %s 类型不合法", t.Name, c.Name)
			}
			if len(c.Enum) > 0 && c.Type != TypeText {
				add("表 %s 的列 %s：Enum 只能用在文本列上", t.Name, c.Name)
			}
			if c.Type == TypeSerial && c.Nullable {
				add("表 %s 的列 %s：自增主键不能可空", t.Name, c.Name)
			}
			if c.Type == TypeBool && c.Default != "" && !strings.EqualFold(c.Default, "FALSE") {
				add("表 %s 的列 %s：布尔列的默认值只能是 FALSE（Go 的零值分不清没填与 false）", t.Name, c.Name)
			}
			if c.Immutable && c.Class != ClassSpec {
				add("表 %s 的列 %s：Immutable 只能标在 spec 列上", t.Name, c.Name)
			}
			if c.Masked && c.Type != TypeText && c.Type != TypeJSON {
				add("表 %s 的列 %s：打码只能标在文本或 JSON 列上", t.Name, c.Name)
			}
		}
		pk := t.PKColumns()
		for _, col := range pk {
			c, ok := t.Column(col)
			if !ok {
				add("表 %s 没有主键：找不到列 %s", t.Name, col)
				continue
			}
			if c.Nullable {
				add("表 %s 的主键列 %s 不能可空", t.Name, col)
			}
		}
		if len(t.PrimaryKey) == 0 {
			if c, ok := t.Column("id"); ok && c.Type != TypeSerial {
				add("表 %s 的 id 列必须是自增主键（TypeSerial）", t.Name)
			}
		}
		for _, c := range t.Columns {
			if c.Type == TypeSerial && !(len(pk) == 1 && pk[0] == c.Name) {
				add("表 %s 的列 %s：自增列只能作为单列主键", t.Name, c.Name)
			}
		}
		for _, ix := range t.Indexes {
			if ix.Name == "" {
				add("表 %s 有索引没有名字", t.Name)
				continue
			}
			if len(ix.Name) > 63 {
				add("索引 %s 的名字超过 63 个字符", ix.Name)
			}
			if other, dup := indexNames[ix.Name]; dup {
				add("索引名 %s 在表 %s 与 %s 里重复", ix.Name, other, t.Name)
			}
			indexNames[ix.Name] = t.Name
			if len(ix.Columns) == 0 {
				add("索引 %s 没有列", ix.Name)
			}
			if ix.NaturalKey && !ix.Unique {
				add("索引 %s 标了自然键却不是唯一索引", ix.Name)
			}
			for _, col := range ix.Columns {
				if _, ok := t.Column(col); !ok {
					add("索引 %s 引用了表 %s 没有的列 %s", ix.Name, t.Name, col)
				}
			}
		}
		for _, fk := range t.ForeignKeys {
			if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefColumns) {
				add("表 %s 的外键 %v 列数与引用列数不一致", t.Name, fk.Columns)
			}
			for _, col := range fk.Columns {
				if _, ok := t.Column(col); !ok {
					add("表 %s 的外键引用了自己没有的列 %s", t.Name, col)
				}
			}
			ref, ok := r.tables[fk.RefTable]
			if !ok {
				add("表 %s 的外键引用了不存在的表 %s", t.Name, fk.RefTable)
			} else {
				for _, col := range fk.RefColumns {
					if _, ok := ref.Column(col); !ok {
						add("表 %s 的外键引用了表 %s 没有的列 %s", t.Name, fk.RefTable, col)
					}
				}
			}
			if !validOnDelete[fk.OnDelete] {
				add("表 %s 的外键 ON DELETE %q 不合法", t.Name, fk.OnDelete)
			}
		}
	}
	if cycle := r.cycle(); len(cycle) > 0 {
		add("外键成环：%s", strings.Join(cycle, ", "))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("表注册表有 %d 处问题：\n  %s", len(problems), strings.Join(problems, "\n  "))
}

// cycle 返回拓扑排序排不进去的表名（外键成环的那些）。
func (r *Registry) cycle() []string {
	indeg := map[string]int{}
	next := map[string][]string{}
	for _, name := range r.names {
		for _, fk := range r.tables[name].ForeignKeys {
			if fk.RefTable == name {
				continue
			}
			if _, ok := r.tables[fk.RefTable]; !ok {
				continue
			}
			indeg[name]++
			next[fk.RefTable] = append(next[fk.RefTable], name)
		}
	}
	var ready []string
	for _, name := range r.names {
		if indeg[name] == 0 {
			ready = append(ready, name)
		}
	}
	visited := 0
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		visited++
		for _, n := range next[name] {
			indeg[n]--
			if indeg[n] == 0 {
				ready = append(ready, n)
			}
		}
	}
	if visited == len(r.names) {
		return nil
	}
	var stuck []string
	for _, name := range r.names {
		if indeg[name] > 0 {
			stuck = append(stuck, name)
		}
	}
	sort.Strings(stuck)
	return stuck
}
