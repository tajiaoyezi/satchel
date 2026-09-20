// Package settings 是仓储层的系统设置：把 system_config 的单行与 system_settings 的 key 合成一个字段值表（master-settings），
// 建单例行，在一个事务里比对版本、存写前快照、写两张表、抬版本，以及快照的读。字段分档只在这里描述（Field），
// 按命令允许哪些档由 service 决定。模型不出本包。
package settings

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	bunschema "github.com/uptrace/bun/schema"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// SingletonID 是 system_config 唯一一行的主键。
const SingletonID = 1

// KindName 是这个单例的 kind 名，快照按它登记。
const KindName = "SystemSettings"

// Field 是设置对象的一个字段：system_config 的一列（Column 为真）或键值表的一个 key。
type Field struct {
	Name   string
	Type   schema.Type
	Class  schema.Class
	Masked bool
	Column bool
}

// State 是某一刻的整个设置对象：版本、时间戳与全部字段的值（bool / int64 / string / json.RawMessage）。
type State struct {
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
	Values    map[string]any
}

// Repo 是设置仓储。
type Repo struct {
	db        *bun.DB
	store     *store.Store
	table     *schema.Table
	fields    map[string]Field
	order     []string
	bunFields map[string]*bunschema.Field
}

// New 建仓储；reg 通常是 schema.Default()。
func New(bdb *bun.DB, st *store.Store, reg *schema.Registry) *Repo {
	t, ok := reg.Table("system_config")
	if !ok {
		panic("注册表里没有 system_config")
	}
	r := &Repo{db: bdb, store: st, table: t, fields: map[string]Field{}, bunFields: map[string]*bunschema.Field{}}
	for _, c := range t.Columns {
		if c.Class == schema.ClassMeta {
			continue
		}
		r.fields[c.Name] = Field{Name: c.Name, Type: c.Type, Class: c.Class, Masked: c.Masked, Column: true}
		r.order = append(r.order, c.Name)
	}
	for _, k := range t.Settings {
		r.fields[k.Name] = Field{Name: k.Name, Type: k.Type, Class: k.Class, Masked: k.Masked}
		r.order = append(r.order, k.Name)
	}
	for _, f := range bdb.Table(reflect.TypeOf(model.SystemSettings{})).Fields {
		r.bunFields[f.Name] = f
	}
	return r
}

// Field 按名字找字段；不存在返回 false。
func (r *Repo) Field(name string) (Field, bool) {
	f, ok := r.fields[name]
	return f, ok
}

// Fields 返回全部字段，列在前、key 在后，按注册表的顺序。
func (r *Repo) Fields() []Field {
	out := make([]Field, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.fields[name])
	}
	return out
}

// EnsureSingleton 确保 system_config 里 id 为 1 的那一行存在：没有就按库默认值建一行（版本 1，两个「省略即为真」的列置 true），
// 已有不动。并发建行撞主键翻成成功。serve 在迁移之后调它，测试的 setup 也调。
func (r *Repo) EnsureSingleton(ctx context.Context) error {
	n, err := r.db.NewSelect().Model((*model.SystemSettings)(nil)).Where("id = ?", SingletonID).Count(ctx)
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "读取系统设置失败", err)
	}
	if n > 0 {
		return nil
	}
	row := &model.SystemSettings{ID: SingletonID, EnableShortLink: true, EnableMiaomiaowuFeatures: true, ResourceVersion: 1}
	if err := r.store.Insert(ctx, row); err != nil {
		if v1.AsError(err).Code == v1.CodeConflict {
			return nil
		}
		return err
	}
	return nil
}

// loadAttempts 是 Load 为了读到一致的一份最多重试的次数。
const loadAttempts = 5

// Load 读整个设置对象：行的每一列加键值表里的每个 key（缺的补默认值），require_encryption 恒为 true。
// 读不在写事务里，列与 key 是两条查询：靠「任何一档的写都抬整单版本」核对一致性——读完再看一次版本，
// 与读到的不同就说明中间有写提交过，整份重读；连续几次都撞上才放弃。
func (r *Repo) Load(ctx context.Context) (*State, error) {
	for attempt := 0; attempt < loadAttempts; attempt++ {
		st, _, err := r.load(ctx, r.db, false)
		if err != nil {
			return nil, err
		}
		var version int64
		if err := r.db.NewSelect().Model((*model.SystemSettings)(nil)).Column("resource_version").Where("id = ?", SingletonID).Scan(ctx, &version); err != nil {
			return nil, v1.Wrap(v1.CodeDatabase, "读取系统设置失败", err)
		}
		if version == st.Version {
			return st, nil
		}
	}
	return nil, v1.New(v1.CodeConflict, "系统设置正在被连续修改，没能读到一致的一份").WithNext("稍后重试")
}

// load 用给定的句柄读，同时返回行模型（写路径要在它上面改列）；forUpdate 为真时在 PostgreSQL 上锁住那一行
// （SQLite 的写事务 BEGIN IMMEDIATE 本来就排他）。
func (r *Repo) load(ctx context.Context, q bun.IDB, forUpdate bool) (*State, *model.SystemSettings, error) {
	var row model.SystemSettings
	sel := q.NewSelect().Model(&row).Where("id = ?", SingletonID)
	if forUpdate && q.Dialect().Name() == dialect.PG {
		sel = sel.For("UPDATE")
	}
	if err := sel.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, v1.New(v1.CodeNotFound, "系统设置的单例行还没有建立").WithNext("启动一次主控（serve 会建它）")
		}
		return nil, nil, v1.Wrap(v1.CodeDatabase, "读取系统设置失败", err)
	}
	var entries []model.SystemSettingEntry
	if err := q.NewSelect().Model(&entries).Scan(ctx); err != nil {
		return nil, nil, v1.Wrap(v1.CodeDatabase, "读取系统设置的键值表失败", err)
	}
	stored := make(map[string]string, len(entries))
	for _, e := range entries {
		stored[e.Key] = e.Value
	}
	st := &State{Version: row.ResourceVersion, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Values: make(map[string]any, len(r.order))}
	rv := reflect.ValueOf(&row).Elem()
	for _, name := range r.order {
		f := r.fields[name]
		if f.Column {
			st.Values[name] = normalizeColumnValue(f.Type, r.bunFields[name].Value(rv).Interface())
			continue
		}
		raw, ok := stored[name]
		if !ok {
			st.Values[name] = defaultFor(f)
			continue
		}
		v, err := Decode(f.Type, raw)
		if err != nil {
			// 库里的值不合编码（只可能是导入或手改），按默认值读、不让整个对象读不出来，但要留下痕迹：
			// 写前快照记的将是默认值，不告警的话坏值会在下一次写之后无声消失。打码字段不打印原值。
			shown := raw
			if f.Masked {
				shown = "（打码字段，长度 " + strconv.Itoa(len(raw)) + "）"
			}
			slog.Warn("系统设置的键值不合编码，按默认值读", "key", name, "value", shown, "error", v1.AsError(err).Reason)
			st.Values[name] = defaultFor(f)
			continue
		}
		st.Values[name] = v
	}
	st.Values["require_encryption"] = true
	return st, &row, nil
}

// normalizeColumnValue 把模型字段的 Go 值归成字段值表的固定类型（int 列在模型里已是 int64，这里只兜住别的整数宽度）。
func normalizeColumnValue(t schema.Type, v any) any {
	if t != schema.TypeInt {
		return v
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	}
	return v
}

// defaultFor 给出一个 key 的默认值：默认值表里有就用它，没有按类型给零值。
func defaultFor(f Field) any {
	if v, ok := defaults[f.Name]; ok {
		return v
	}
	switch f.Type {
	case schema.TypeBool:
		return false
	case schema.TypeInt:
		return int64(0)
	case schema.TypeJSON:
		return nil
	}
	return ""
}

// DefaultOf 返回一个 key 的默认值（测试与 explain 用）；列没有默认值表，返回 false。
func DefaultOf(name string) (any, bool) {
	v, ok := defaults[name]
	return v, ok
}
