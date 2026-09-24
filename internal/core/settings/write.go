package settings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// WriteRequest 是一次设置写。Values 是已归一成固定 Go 类型的字段值（哪些档允许写由调用方按命令限定，这里按每个字段的档选写入原语）；
// ExpectedVersion 是调用方读到的 resourceVersion，总要比对（没有跳过比对的写法：force 归危险操作的权限类，随 M2 的 apply 一起做门）；
// Snapshot 为真时先存一份写前快照（内容是日常运维档的全部字段），Source 写进快照的 source。
type WriteRequest struct {
	Values          map[string]any
	ExpectedVersion int64
	Snapshot        bool
	Source          string
}

// Write 在一个事务里完成一次设置写（master-settings「settings set 是日常运维档的事务写」）：
// PostgreSQL 先锁住单例行 → 读写前值 → 写前快照 → 比对版本并抬版本（有 spec 列时经 UpdateSpec，否则经 Bump）→ 写别的档的列 → 写 key。
// 任一步失败整个事务回滚，快照行随之不存在。返回的是这次写之后的对象（事务内重读，不会混进别人紧接着的写）。
func (r *Repo) Write(ctx context.Context, req WriteRequest) (*State, error) {
	if len(req.Values) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "没有要写的字段")
	}
	cols := map[schema.Class][]string{}
	var keys []string
	for name := range req.Values {
		f, ok := r.fields[name]
		if !ok {
			return nil, v1.Newf(v1.CodeUnknownField, "系统设置没有字段 %s", name)
		}
		// 列与 key 同一条口径：只读档没有写路径；运行态是系统自己写的、不算设置写、不抬版本（resource-model），
		// 不走这里（M6 的自愈自己经 UpdateStatus 与键值表写，不 Bump）。哪些档允许写由调用方按命令限定。
		switch f.Class {
		case schema.ClassSpec, schema.ClassHuman, schema.ClassMasterSelf:
		default:
			return nil, v1.Newf(v1.CodeFieldNotApplyable, "字段 %s 是%s档，不能经设置写接口写入", name, f.Class)
		}
		if f.Column {
			cols[f.Class] = append(cols[f.Class], name)
		} else {
			keys = append(keys, name)
		}
	}
	for _, names := range cols {
		sort.Strings(names)
	}
	sort.Strings(keys)
	var after *State
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		before, row, err := r.load(ctx, tx, true)
		if err != nil {
			return err
		}
		row.ResourceVersion = req.ExpectedVersion
		rv := reflect.ValueOf(row).Elem()
		for _, names := range cols {
			for _, name := range names {
				if err := r.setColumn(rv, name, req.Values[name]); err != nil {
					return err
				}
			}
		}
		ts := r.store.WithTx(tx)
		// 顺序照 spec：写前快照 → 比对版本并抬版本 → 写列 → 写 key；都在同一个事务里，版本不匹配整单回滚、快照随之不存在。
		if req.Snapshot {
			snap, err := r.snapshotOf(before, req.Source)
			if err != nil {
				return err
			}
			if err := ts.Insert(ctx, snap); err != nil {
				return err
			}
		}
		if spec := cols[schema.ClassSpec]; len(spec) > 0 {
			if err := ts.UpdateSpec(ctx, row, false, spec...); err != nil {
				return err
			}
		} else if err := ts.Bump(ctx, row, false); err != nil {
			return err
		}
		if names := cols[schema.ClassHuman]; len(names) > 0 {
			if err := ts.UpdateHuman(ctx, row, names); err != nil {
				return err
			}
		}
		if names := cols[schema.ClassMasterSelf]; len(names) > 0 {
			if err := ts.UpdateMasterSelf(ctx, row, names); err != nil {
				return err
			}
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		for _, name := range keys {
			text, err := Encode(r.fields[name].Type, req.Values[name])
			if err != nil {
				return err
			}
			entry := &model.SystemSettingEntry{Key: name, Value: text, UpdatedAt: now}
			if _, err := tx.NewInsert().Model(entry).On("CONFLICT (key) DO UPDATE").
				Set("value = EXCLUDED.value").Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
				return v1.Wrap(v1.CodeDatabase, "写系统设置的键值表失败", err)
			}
		}
		after, _, err = r.load(ctx, tx, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return after, nil
}

// setColumn 把值设到行模型对应列的字段上（bun 的表元数据按列名找字段，与 store.resolve 同一套）。
func (r *Repo) setColumn(rv reflect.Value, name string, v any) error {
	f, ok := r.bunFields[name]
	if !ok {
		return v1.Newf(v1.CodeInternal, "模型里没有列 %s", name)
	}
	target := f.Value(rv)
	val := reflect.ValueOf(v)
	if !val.IsValid() || !val.Type().ConvertibleTo(target.Type()) {
		return v1.Newf(v1.CodeInternal, "列 %s 的值 %T 装不进模型字段 %s", name, v, target.Type())
	}
	target.Set(val.Convert(target.Type()))
	return nil
}

// snapshotOf 生成写前快照的行：内容是日常运维档（spec）全部字段的 canonical JSON（见 Canonical），
// 打码字段存原文；七组字段与主控自身类字段不进（后者随 m1-08 的第一条写命令再定）。
func (r *Repo) snapshotOf(before *State, source string) (*model.ConfigSnapshot, error) {
	content := map[string]any{}
	for name, f := range r.fields {
		if f.Class == schema.ClassSpec {
			content[name] = before.Values[name]
		}
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码设置快照失败", err)
	}
	canonical, err := Canonical(raw)
	if err != nil {
		return nil, err
	}
	return &model.ConfigSnapshot{
		ObjectKind: KindName, ObjectID: SingletonID, ObjectVersion: before.Version,
		Content: canonical, ContentHash: ContentHash(canonical), Source: source, Status: StatusSaved,
	}, nil
}

// Canonical 把一段 JSON 变成规范形式：所有层级的对象键按字母序、没有多余空白（Go 的 map 序列化天然如此），
// 数字保留原始字面量（json.Number，不经 float64，超过 2^53 的整数不会被改写）。
// 快照的 content 按它存、content_hash 按它算——PostgreSQL 的 jsonb 会重排键与空白，读回来的字节不一定等于写进去的，
// 但重新规范化之后一定相同，哈希因此在两库上都能核对。
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "快照内容不是合法的 JSON", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码设置快照失败", err)
	}
	return out, nil
}

// ContentHash 是快照内容的哈希：canonical 字节的 SHA-256 十六进制。
func ContentHash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
