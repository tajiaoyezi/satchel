package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"time"

	"github.com/uptrace/bun"
	bunschema "github.com/uptrace/bun/schema"

	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Store 是写入原语。M1 起业务代码只能经这里写 kind 表，core 层不许自己拼 UPDATE。
// db 只用来取表元数据；查询走 q——平时就是 db 本身，WithTx 之后是那个事务。
type Store struct {
	db  *bun.DB
	q   bun.IDB
	reg *schema.Registry
}

// New 建一个写入原语实例；reg 通常是 schema.Default()。
func New(db *bun.DB, reg *schema.Registry) *Store {
	return &Store{db: db, q: db, reg: reg}
}

// WithTx 返回一个在事务 tx 里跑查询的副本：校验、翻译与表元数据都不变，只是每条语句都进这个事务。
// 一次业务写要把几张表的改动与快照放进同一个事务时用它（设置服务），事务的开与提交仍由调用方管。
func (s *Store) WithTx(tx bun.Tx) *Store {
	return &Store{db: s.db, q: tx, reg: s.reg}
}

// Cond 是更新的前置条件：列必须等于 Value（Value 为 nil 或 nil 指针表示必须为 NULL）。
// 不满足时更新不落地，报 conflict 并带上该列的当前值。用它做「只许从状态 A 转到 B」的原子更新。
type Cond struct {
	Column string
	Value  any
}

// isNull 报告前置条件的值是不是空：无类型 nil，或模型字段那种带类型的 nil 指针都算。
func (c Cond) isNull() bool {
	if c.Value == nil {
		return true
	}
	v := reflect.ValueOf(c.Value)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// target 是一次操作的目标：注册表里的表、bun 的表元数据、模型结构体的值。
type target struct {
	table  *schema.Table
	fields map[string]*bunschema.Field
	value  reflect.Value
}

func (s *Store) resolve(model any) (*target, error) {
	v := reflect.ValueOf(model)
	if v.Kind() != reflect.Ptr || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return nil, v1.Newf(v1.CodeInternal, "写入原语需要模型结构体指针，得到 %T", model)
	}
	bt := s.db.Table(v.Type().Elem())
	t, ok := s.reg.Table(bt.Name)
	if !ok {
		return nil, v1.Newf(v1.CodeInternal, "表 %s 不在注册表里", bt.Name)
	}
	fields := make(map[string]*bunschema.Field, len(bt.Fields))
	for _, f := range bt.Fields {
		fields[f.Name] = f
	}
	return &target{table: t, fields: fields, value: v.Elem()}, nil
}

func (t *target) label() string {
	if t.table.IsKind() {
		return t.table.Kind
	}
	return t.table.Name
}

func (t *target) field(column string) (reflect.Value, bool) {
	f, ok := t.fields[column]
	if !ok {
		return reflect.Value{}, false
	}
	return f.Value(t.value), true
}

func (t *target) id() (int64, error) {
	f, ok := t.field("id")
	if !ok {
		return 0, v1.Newf(v1.CodeBadRequest, "表 %s 没有整数主键 id", t.table.Name)
	}
	return f.Int(), nil
}

func (t *target) version() int64 {
	f, ok := t.field("resource_version")
	if !ok {
		return 0
	}
	return f.Int()
}

// setTime 把模型里的时间列设为 now，返回把它改回原值的函数：写没落地时模型不该带着库里没有的时间。
func (t *target) setTime(column string, now time.Time) (restore func()) {
	f, ok := t.field(column)
	if !ok {
		return func() {}
	}
	previous := reflect.ValueOf(f.Interface())
	if f.Kind() == reflect.Ptr {
		f.Set(reflect.ValueOf(&now))
	} else {
		f.Set(reflect.ValueOf(now))
	}
	return func() { f.Set(previous) }
}

func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// Insert 插入一行，自增主键与库默认值回填到 model。自增主键一律由库分配，调用方预填的 id 被忽略。
// created_at、updated_at 与其它带 CURRENT_TIMESTAMP 默认值的非空时间列，模型里是零值时填当前时间；
// 其它有默认值、模型里是零值的列交给库默认值（布尔列的默认值只能是 FALSE，与零值一致）。
func (s *Store) Insert(ctx context.Context, model any) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	ts := now()
	var exclude []string
	for _, c := range tg.table.Columns {
		if c.Type == schema.TypeSerial {
			exclude = append(exclude, c.Name)
			continue
		}
		f, ok := tg.field(c.Name)
		if !ok || !f.IsZero() {
			continue
		}
		if c.Type == schema.TypeTime && !c.Nullable && c.Default == "CURRENT_TIMESTAMP" {
			tg.setTime(c.Name, ts)
			continue
		}
		if c.Default != "" {
			exclude = append(exclude, c.Name)
		}
	}
	_, err = s.q.NewInsert().Model(model).ExcludeColumn(exclude...).Returning("*").Exec(ctx)
	return s.translate(tg.table, err)
}

// Get 按主键 id 读一行；没有时返回 not_found。已软删除的行照样返回，调用方看 deleted_at。
func (s *Store) Get(ctx context.Context, model any, id int64) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	if _, ok := tg.field("id"); !ok {
		return v1.Newf(v1.CodeBadRequest, "表 %s 没有整数主键 id", tg.table.Name)
	}
	err = s.q.NewSelect().Model(model).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound(tg, id)
	}
	return s.translate(tg.table, err)
}

func notFound(tg *target, id int64) error {
	return v1.Newf(v1.CodeNotFound, "%s 里没有 id 为 %d 的对象", tg.label(), id)
}

// checkColumns 确认要写的列都存在且属于允许的分档。
func (s *Store) checkColumns(tg *target, verb string, class schema.Class, columns []string) error {
	if tg.table.AppendOnly {
		return v1.Newf(v1.CodeAppendOnly, "%s 是 append-only 的表，只能追加，不能修改", tg.label())
	}
	if len(columns) == 0 {
		return v1.Newf(v1.CodeBadRequest, "%s 没有指定要写的列", verb)
	}
	for _, name := range columns {
		c, ok := tg.table.Column(name)
		if !ok {
			return v1.Newf(v1.CodeBadRequest, "表 %s 没有列 %s", tg.table.Name, name)
		}
		if c.Class != class {
			return v1.Newf(v1.CodeBadRequest, "列 %s 是 %s 档，不能经 %s 写", name, c.Class, verb)
		}
	}
	return nil
}

// UpdateSpec 写 spec 档的列：以 model 里的 resource_version 为期望值做校验，成功后版本加 1 并回填。
// force 只跳过版本比对，其它校验照做，版本仍加 1。
func (s *Store) UpdateSpec(ctx context.Context, model any, force bool, columns ...string) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	if err := s.checkColumns(tg, "UpdateSpec", schema.ClassSpec, columns); err != nil {
		return err
	}
	if !tg.table.HasVersion() {
		return v1.Newf(v1.CodeBadRequest, "%s 没有 resource_version，不能经 UpdateSpec 写", tg.label())
	}
	for _, name := range columns {
		if c, _ := tg.table.Column(name); c.Immutable {
			return v1.Newf(v1.CodeBadRequest, "%s 的 %s 创建后不能改", tg.label(), name).
				WithState("column", name)
		}
	}
	id, err := tg.id()
	if err != nil {
		return err
	}
	expected := tg.version()
	q := s.q.NewUpdate().Model(model).Column(columns...).Where("id = ?", id)
	if !force {
		q = q.Where("resource_version = ?", expected)
	}
	ok, err := s.execBump(ctx, tg, q)
	if err != nil || ok {
		return err
	}
	current, exists, err := s.currentVersion(ctx, tg, id)
	if err != nil {
		return err
	}
	if !exists {
		return notFound(tg, id)
	}
	return versionConflict(tg, expected, current)
}

// Bump 只比对 resource_version 并加 1（force 跳过比对），顺手写 updated_at 并回填，不写任何别的列。
// 给「没有 spec 列可写、但按第 07 章要抬整单版本」的写用：系统设置单例只改键值表的 key、或只改人类专属列的那一次。
// append-only 的表与没有版本列的表拒绝；行不存在 not_found；版本不匹配 version_conflict。
func (s *Store) Bump(ctx context.Context, model any, force bool) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	if tg.table.AppendOnly {
		return v1.Newf(v1.CodeAppendOnly, "%s 是 append-only 的表，只能追加，不能修改", tg.label())
	}
	if !tg.table.HasVersion() {
		return v1.Newf(v1.CodeBadRequest, "%s 没有 resource_version，不能经 Bump 抬版本", tg.label())
	}
	id, err := tg.id()
	if err != nil {
		return err
	}
	expected := tg.version()
	q := s.q.NewUpdate().Model(model).Where("id = ?", id)
	if !force {
		q = q.Where("resource_version = ?", expected)
	}
	ok, err := s.execBump(ctx, tg, q)
	if err != nil || ok {
		return err
	}
	current, exists, err := s.currentVersion(ctx, tg, id)
	if err != nil {
		return err
	}
	if !exists {
		return notFound(tg, id)
	}
	return versionConflict(tg, expected, current)
}

// execBump 给更新语句加上 updated_at 与 resource_version + 1 并执行；返回是否写到了行。
// 没写到行时把模型的 updated_at 改回去，模型不带库里没有的时间。
func (s *Store) execBump(ctx context.Context, tg *target, q *bun.UpdateQuery) (bool, error) {
	restore := tg.setTime("updated_at", now())
	q = q.Column("updated_at", "resource_version").
		Value("resource_version", "resource_version + 1").
		Returning("resource_version, updated_at")
	res, err := q.Exec(ctx)
	if err != nil {
		restore()
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, s.translate(tg.table, err)
	}
	// bun 走 RETURNING 时，RowsAffected 是扫回的行数；零行说明 WHERE 没匹配到。
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	restore()
	return false, nil
}

func (s *Store) currentVersion(ctx context.Context, tg *target, id int64) (version int64, exists bool, err error) {
	err = s.q.NewSelect().Table(tg.table.Name).Column("resource_version").Where("id = ?", id).Scan(ctx, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, s.translate(tg.table, err)
	}
	return version, true, nil
}

func versionConflict(tg *target, expected, current int64) error {
	return v1.Newf(v1.CodeVersionConflict, "%s 已被别人改过：期望 resourceVersion %d，存储中是 %d", tg.label(), expected, current).
		WithState("resourceVersion", current).
		WithNext("重新读取对象，在最新的 resourceVersion 上再改")
}

// UpdateStatus 写 status 档的列，不动 resource_version；conds 是可选的前置条件。
func (s *Store) UpdateStatus(ctx context.Context, model any, columns []string, conds ...Cond) error {
	return s.updateClass(ctx, model, "UpdateStatus", schema.ClassStatus, columns, conds)
}

// UpdateAction 写动作专属的列，不动 resource_version；conds 是可选的前置条件。
func (s *Store) UpdateAction(ctx context.Context, model any, columns []string, conds ...Cond) error {
	return s.updateClass(ctx, model, "UpdateAction", schema.ClassAction, columns, conds)
}

// UpdateHuman 写人类专属的列，不动 resource_version；conds 是可选的前置条件。
func (s *Store) UpdateHuman(ctx context.Context, model any, columns []string, conds ...Cond) error {
	return s.updateClass(ctx, model, "UpdateHuman", schema.ClassHuman, columns, conds)
}

// UpdateMasterSelf 写主控自身类的列，不动 resource_version；conds 是可选的前置条件。
func (s *Store) UpdateMasterSelf(ctx context.Context, model any, columns []string, conds ...Cond) error {
	return s.updateClass(ctx, model, "UpdateMasterSelf", schema.ClassMasterSelf, columns, conds)
}

func (s *Store) updateClass(ctx context.Context, model any, verb string, class schema.Class, columns []string, conds []Cond) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	if err := s.checkColumns(tg, verb, class, columns); err != nil {
		return err
	}
	for _, c := range conds {
		if _, ok := tg.table.Column(c.Column); !ok {
			return v1.Newf(v1.CodeBadRequest, "表 %s 没有列 %s，不能作为前置条件", tg.table.Name, c.Column)
		}
	}
	id, err := tg.id()
	if err != nil {
		return err
	}
	// 复制一份再追加：columns 是调用方的切片，直接 append 会改写它后面的元素。
	columns = append([]string(nil), columns...)
	restore := func() {}
	if _, ok := tg.field("updated_at"); ok {
		restore = tg.setTime("updated_at", now())
		columns = append(columns, "updated_at")
	}
	q := s.q.NewUpdate().Model(model).Column(columns...).Where("id = ?", id)
	for _, c := range conds {
		if c.isNull() {
			q = q.Where("? IS NULL", bun.Ident(c.Column))
		} else {
			q = q.Where("? = ?", bun.Ident(c.Column), c.Value)
		}
	}
	res, err := q.Exec(ctx)
	if err != nil {
		restore()
		return s.translate(tg.table, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	restore()
	if len(conds) == 0 {
		return notFound(tg, id)
	}
	condColumns := make([]string, len(conds))
	for i, c := range conds {
		condColumns[i] = c.Column
	}
	current := map[string]any{}
	err = s.q.NewSelect().Table(tg.table.Name).Column(condColumns...).Where("id = ?", id).Scan(ctx, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound(tg, id)
	}
	if err != nil {
		return s.translate(tg.table, err)
	}
	e := v1.Newf(v1.CodeConflict, "%s 的前置条件不满足，没有改动", tg.label()).
		WithNext("重新读取对象，按它现在的状态决定下一步")
	for _, col := range condColumns {
		e.WithState(col, current[col])
	}
	return e
}

// SoftDelete 写 deleted_at 并抬版本：以 model 里的 resource_version 为期望值做校验（force 跳过比对）；
// 已删除的对象再删报 bad_request。
func (s *Store) SoftDelete(ctx context.Context, model any, force bool) error {
	return s.setDeleted(ctx, model, force, true)
}

// Restore 清空 deleted_at 并抬版本，id 不变：同样带版本校验；未删除的对象报 bad_request。
func (s *Store) Restore(ctx context.Context, model any, force bool) error {
	return s.setDeleted(ctx, model, force, false)
}

func (s *Store) setDeleted(ctx context.Context, model any, force, deleted bool) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	if tg.table.AppendOnly {
		return v1.Newf(v1.CodeAppendOnly, "%s 是 append-only 的表，不能删除", tg.label())
	}
	deletedAt, ok := tg.field("deleted_at")
	if !ok || !tg.table.HasVersion() {
		return v1.Newf(v1.CodeBadRequest, "%s 不支持软删除", tg.label())
	}
	id, err := tg.id()
	if err != nil {
		return err
	}
	expected := tg.version()
	previous := deletedAt.Interface()
	if deleted {
		tg.setTime("deleted_at", now())
	} else {
		deletedAt.Set(reflect.Zero(deletedAt.Type()))
	}
	q := s.q.NewUpdate().Model(model).Column("deleted_at").Where("id = ?", id)
	if deleted {
		q = q.Where("deleted_at IS NULL")
	} else {
		q = q.Where("deleted_at IS NOT NULL")
	}
	if !force {
		q = q.Where("resource_version = ?", expected)
	}
	ok, err = s.execBump(ctx, tg, q)
	if err == nil && ok {
		return nil
	}
	deletedAt.Set(reflect.ValueOf(previous))
	if err != nil {
		return err
	}
	var row struct {
		ResourceVersion int64      `bun:"resource_version"`
		DeletedAt       *time.Time `bun:"deleted_at"`
	}
	err = s.q.NewSelect().Table(tg.table.Name).Column("resource_version", "deleted_at").Where("id = ?", id).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound(tg, id)
	}
	if err != nil {
		return s.translate(tg.table, err)
	}
	if deleted && row.DeletedAt != nil {
		return v1.Newf(v1.CodeBadRequest, "%s 已经是软删除状态，不能重复删除", tg.label()).
			WithState("deletedAt", *row.DeletedAt)
	}
	if !deleted && row.DeletedAt == nil {
		return v1.Newf(v1.CodeBadRequest, "%s 没有被删除，不需要恢复", tg.label())
	}
	return versionConflict(tg, expected, row.ResourceVersion)
}
