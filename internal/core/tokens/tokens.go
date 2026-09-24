// Package tokens 是仓储层的 API 令牌（master-api-tokens）：读写 api_tokens，返回领域结构 Token，模型不出本包。
// 表背后是 kind ApiToken（动作类）：签发经 store.Insert；改名字、权限范围、预设、过期时间与吊销都是人类专属列，
// 经 store.UpdateHuman 写并带「未吊销」前置条件（已吊销的令牌不能再改、不能再吊销）；最后使用时间是 status，经 store.UpdateStatus 写。
// 令牌不做软删除：吊销就是它的终点，deleted_at 有值的行一律当不存在。
package tokens

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Grant 是令牌的权限范围，也是 scopes 列里 JSON 的形状：{"scopes":[...],"danger":[...]}，与身份对象同形。
// 规范化（排序、去重、read 恒在）与预设推导在 service 层做，这里原样存取。
type Grant struct {
	Scopes []v1.Scope  `json:"scopes"`
	Danger []v1.Danger `json:"danger"`
}

// Token 是一把令牌的领域结构；不含哈希，明文从来不进库。
type Token struct {
	ID         int64
	Owner      string
	Name       string
	Grant      Grant
	Preset     string
	ExpiresAt  *time.Time
	Runtime    string
	Revoked    bool
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// Filter 是列表的过滤条件：Owner 非空时只看这个签发者的，RuntimeOnly 时只看绑了 runtime 标签的。
type Filter struct {
	Owner       string
	RuntimeOnly bool
}

// ErrNotFound 表示没有这把令牌（含已软删除的行）。
var ErrNotFound = errors.New("api token not found")

// Repo 是令牌仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// Insert 签发：写一行（owner、name、hash、权限范围、预设、过期时间、runtime），回填 id 与创建时间。
func (r *Repo) Insert(ctx context.Context, t *Token, hash string) error {
	grant, err := json.Marshal(t.Grant)
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "编码令牌的权限范围失败", err)
	}
	row := &model.ApiToken{Owner: t.Owner, Name: t.Name, TokenHash: hash, Scopes: grant, Preset: t.Preset, ExpiresAt: utcPtr(t.ExpiresAt)}
	if t.Runtime != "" {
		rt := t.Runtime
		row.Runtime = &rt
	}
	if err := r.store.Insert(ctx, row); err != nil {
		return err
	}
	t.ID, t.CreatedAt = row.ID, row.CreatedAt
	return nil
}

// GetByHash 按令牌哈希找一把令牌（吊销与过期的照样返回，调用方判断）；没有返回 ErrNotFound。
func (r *Repo) GetByHash(ctx context.Context, hash string) (*Token, error) {
	return r.getOne(ctx, "token_hash = ?", hash)
}

// GetByID 按 id 找一把令牌；没有返回 ErrNotFound。
func (r *Repo) GetByID(ctx context.Context, id int64) (*Token, error) {
	return r.getOne(ctx, "id = ?", id)
}

func (r *Repo) getOne(ctx context.Context, where string, arg any) (*Token, error) {
	var row model.ApiToken
	err := r.db.NewSelect().Model(&row).Where(where, arg).Where("deleted_at IS NULL").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取令牌失败", err)
	}
	return toToken(&row)
}

// List 按 id 倒序取一页：beforeID 大于 0 时只取 id 小于它的（keyset 翻页），最多 limit 条。
func (r *Repo) List(ctx context.Context, f Filter, limit int, beforeID int64) ([]Token, error) {
	var rows []model.ApiToken
	q := applyFilter(r.db.NewSelect().Model(&rows), f).OrderExpr("id DESC").Limit(limit)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取令牌列表失败", err)
	}
	return toTokens(rows)
}

// ListByLastUsed 按最后使用时间倒序取一页（从没用过的排在最后，同一时刻按 id 倒序）：最后使用时间一直在变，
// 用不了 keyset，按偏移量翻页；绑了 runtime 的令牌本来就不多。
func (r *Repo) ListByLastUsed(ctx context.Context, f Filter, limit, offset int) ([]Token, error) {
	var rows []model.ApiToken
	q := applyFilter(r.db.NewSelect().Model(&rows), f).
		OrderExpr("CASE WHEN last_used_at IS NULL THEN 1 ELSE 0 END").OrderExpr("last_used_at DESC").OrderExpr("id DESC").
		Limit(limit).Offset(offset)
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取令牌列表失败", err)
	}
	return toTokens(rows)
}

// Count 数过滤条件下的令牌。
func (r *Repo) Count(ctx context.Context, f Filter) (int, error) {
	n, err := applyFilter(r.db.NewSelect().Model((*model.ApiToken)(nil)), f).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计令牌失败", err)
	}
	return n, nil
}

func applyFilter(q *bun.SelectQuery, f Filter) *bun.SelectQuery {
	q = q.Where("deleted_at IS NULL")
	if f.Owner != "" {
		q = q.Where("owner = ?", f.Owner)
	}
	if f.RuntimeOnly {
		q = q.Where("runtime IS NOT NULL AND runtime <> ''")
	}
	return q
}

// updatable 是 Update 能写的列：名字、权限范围、预设、过期时间（都是人类专属档）。
var updatable = map[string]bool{"name": true, "scopes": true, "preset": true, "expires_at": true}

// Update 把 t 里的这几列写回（令牌字符串不变）；前置条件是「未吊销」：已吊销的行不改，报 conflict（service 翻成 bad_request）。
func (r *Repo) Update(ctx context.Context, t *Token, columns ...string) error {
	for _, c := range columns {
		if !updatable[c] {
			return v1.Newf(v1.CodeInternal, "令牌的列 %s 不能经 Update 写", c)
		}
	}
	grant, err := json.Marshal(t.Grant)
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "编码令牌的权限范围失败", err)
	}
	row := &model.ApiToken{ID: t.ID, Name: t.Name, Scopes: grant, Preset: t.Preset, ExpiresAt: utcPtr(t.ExpiresAt)}
	return r.store.UpdateHuman(ctx, row, columns, store.Cond{Column: "revoked", Value: false})
}

// Revoke 吊销：置 revoked 与 revoked_at；前置条件同 Update，已吊销的再吊销报 conflict。
func (r *Repo) Revoke(ctx context.Context, id int64, at time.Time) error {
	at = at.UTC()
	row := &model.ApiToken{ID: id, Revoked: true, RevokedAt: &at}
	return r.store.UpdateHuman(ctx, row, []string{"revoked", "revoked_at"}, store.Cond{Column: "revoked", Value: false})
}

// TouchLastUsed 写最后使用时间（status 档，不抬版本）。
func (r *Repo) TouchLastUsed(ctx context.Context, id int64, at time.Time) error {
	at = at.UTC()
	return r.store.UpdateStatus(ctx, &model.ApiToken{ID: id, LastUsedAt: &at}, []string{"last_used_at"})
}

func toTokens(rows []model.ApiToken) ([]Token, error) {
	out := make([]Token, 0, len(rows))
	for i := range rows {
		t, err := toToken(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

func toToken(row *model.ApiToken) (*Token, error) {
	t := &Token{
		ID: row.ID, Owner: row.Owner, Name: row.Name, Preset: row.Preset, ExpiresAt: row.ExpiresAt,
		Revoked: row.Revoked, RevokedAt: row.RevokedAt, LastUsedAt: row.LastUsedAt, CreatedAt: row.CreatedAt,
	}
	if row.Runtime != nil {
		t.Runtime = *row.Runtime
	}
	if len(row.Scopes) > 0 {
		if err := json.Unmarshal(row.Scopes, &t.Grant); err != nil {
			return nil, v1.Wrap(v1.CodeDatabase, "令牌的权限范围不是合法 JSON", err)
		}
	}
	return t, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
