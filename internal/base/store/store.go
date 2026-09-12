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
type Store struct {
	db  *bun.DB
	reg *schema.Registry
}

// New 建一个写入原语实例；reg 通常是 schema.Default()。
func New(db *bun.DB, reg *schema.Registry) *Store {
	return &Store{db: db, reg: reg}
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

func (t *target) setTime(column string, now time.Time) {
	f, ok := t.field(column)
	if !ok {
		return
	}
	if f.Kind() == reflect.Ptr {
		f.Set(reflect.ValueOf(&now))
		return
	}
	f.Set(reflect.ValueOf(now))
}

func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// Insert 插入一行，自增主键与库默认值回填到 model。
// created_at、updated_at 与其它带 CURRENT_TIMESTAMP 默认值的非空时间列，模型里是零值时填当前时间；
// 其它有默认值、模型里是零值的列（布尔除外，布尔按模型的值写）交给库默认值。
func (s *Store) Insert(ctx context.Context, model any) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	ts := now()
	var exclude []string
	for _, c := range tg.table.Columns {
		f, ok := tg.field(c.Name)
		if !ok || !f.IsZero() {
			continue
		}
		if c.Type == schema.TypeTime && !c.Nullable && c.Default == "CURRENT_TIMESTAMP" {
			tg.setTime(c.Name, ts)
			continue
		}
		if c.Default != "" && c.Type != schema.TypeBool {
			exclude = append(exclude, c.Name)
		}
	}
	_, err = s.db.NewInsert().Model(model).ExcludeColumn(exclude...).Returning("*").Exec(ctx)
	return translate(err)
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
	err = s.db.NewSelect().Model(model).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound(tg, id)
	}
	return translate(err)
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
	id, err := tg.id()
	if err != nil {
		return err
	}
	expected, _ := tg.field("resource_version")
	q := s.db.NewUpdate().Model(model).Column(columns...).Where("id = ?", id)
	if !force {
		q = q.Where("resource_version = ?", expected.Int())
	}
	return s.bumpVersion(ctx, tg, q, id, expected.Int(), force)
}

// bumpVersion 给更新语句加上 updated_at 与 resource_version + 1，执行并处理零行的情况。
func (s *Store) bumpVersion(ctx context.Context, tg *target, q *bun.UpdateQuery, id, expected int64, force bool) error {
	tg.setTime("updated_at", now())
	q = q.Column("updated_at", "resource_version").
		Value("resource_version", "resource_version + 1").
		Returning("resource_version, updated_at")
	res, err := q.Exec(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return translate(err)
	}
	if err == nil {
		// bun 走 RETURNING 时，RowsAffected 是扫回的行数；零行说明 WHERE 没匹配到。
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
	}
	var current int64
	err = s.db.NewSelect().Table(tg.table.Name).Column("resource_version").Where("id = ?", id).Scan(ctx, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound(tg, id)
	}
	if err != nil {
		return translate(err)
	}
	if force {
		return v1.Wrap(v1.CodeDatabase, "更新没有写到任何行", errors.New("零行"))
	}
	return v1.Newf(v1.CodeVersionConflict, "%s 已被别人改过：期望 resourceVersion %d，存储中是 %d", tg.label(), expected, current).
		WithState("resourceVersion", current).
		WithNext("重新读取对象，在最新的 resourceVersion 上再改")
}

// UpdateStatus 写 status 档的列，不动 resource_version。
func (s *Store) UpdateStatus(ctx context.Context, model any, columns ...string) error {
	return s.updateClass(ctx, model, "UpdateStatus", schema.ClassStatus, columns)
}

// UpdateAction 写动作专属的列，不动 resource_version。
func (s *Store) UpdateAction(ctx context.Context, model any, columns ...string) error {
	return s.updateClass(ctx, model, "UpdateAction", schema.ClassAction, columns)
}

// UpdateHuman 写人类专属的列，不动 resource_version。
func (s *Store) UpdateHuman(ctx context.Context, model any, columns ...string) error {
	return s.updateClass(ctx, model, "UpdateHuman", schema.ClassHuman, columns)
}

func (s *Store) updateClass(ctx context.Context, model any, verb string, class schema.Class, columns []string) error {
	tg, err := s.resolve(model)
	if err != nil {
		return err
	}
	if err := s.checkColumns(tg, verb, class, columns); err != nil {
		return err
	}
	id, err := tg.id()
	if err != nil {
		return err
	}
	if _, ok := tg.field("updated_at"); ok {
		tg.setTime("updated_at", now())
		columns = append(columns, "updated_at")
	}
	res, err := s.db.NewUpdate().Model(model).Column(columns...).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translate(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound(tg, id)
	}
	return nil
}

// SoftDelete 写 deleted_at 并抬版本。
func (s *Store) SoftDelete(ctx context.Context, model any) error {
	return s.setDeleted(ctx, model, true)
}

// Restore 清空 deleted_at 并抬版本；id 不变。
func (s *Store) Restore(ctx context.Context, model any) error {
	return s.setDeleted(ctx, model, false)
}

func (s *Store) setDeleted(ctx context.Context, model any, deleted bool) error {
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
	if deleted {
		tg.setTime("deleted_at", now())
	} else {
		deletedAt.Set(reflect.Zero(deletedAt.Type()))
	}
	expected, _ := tg.field("resource_version")
	q := s.db.NewUpdate().Model(model).Column("deleted_at").Where("id = ?", id)
	return s.bumpVersion(ctx, tg, q, id, expected.Int(), true)
}
