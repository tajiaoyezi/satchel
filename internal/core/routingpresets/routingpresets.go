// Package routingpresets 是仓储层的用户自定义路由规则快捷预设（表 routing_rule_presets）：
// 严守单向分层纪律，不依赖其它 internal/core/* 包。
package routingpresets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// ErrNotFound 表示路由预设不存在。
var ErrNotFound = errors.New("routing preset not found")

// Preset 是一条用户保存的自定义路由规则预设。
type Preset struct {
	ID        int64           `json:"id"`
	Username  string          `json:"username"`
	Name      string          `json:"name"`
	Rule      json.RawMessage `json:"rule"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Repo 是 routing_rule_presets 仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// Save 保存一条用户自定义路由预设（规则 JSON 规范化存储；相同用户与相同规则已存在时返回 conflict）。
func (r *Repo) Save(ctx context.Context, username, name string, ruleJSON json.RawMessage) (*Preset, error) {
	u := strings.TrimSpace(username)
	n := strings.TrimSpace(name)
	if u == "" {
		return nil, v1.New(v1.CodeBadRequest, "routing preset 缺少所属账号 username")
	}
	if n == "" || len(n) > 128 {
		return nil, v1.New(v1.CodeBadRequest, "routing preset 名称必须在 1..128 字符之间")
	}
	canon, err := xrpc.CanonicalJSON(ruleJSON)
	if err != nil {
		return nil, v1.Wrap(v1.CodeBadRequest, "routing preset 规则不是合法的 JSON", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	m := &model.RoutingRulePreset{
		Username:  u,
		Name:      n,
		RuleJSON:  canon,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.store.Insert(ctx, m); err != nil {
		if ae := v1.AsError(err); ae != nil && ae.Code == v1.CodeNameTaken {
			return nil, v1.Newf(v1.CodeConflict, "账号 %s 下已存在内容完全相同的路由预设", u)
		}
		return nil, err
	}
	return toPreset(m), nil
}

// ByID 读取单条自定义路由预设。
func (r *Repo) ByID(ctx context.Context, id int64) (*Preset, error) {
	m := new(model.RoutingRulePreset)
	err := r.db.NewSelect().Model(m).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取路由预设失败", err)
	}
	return toPreset(m), nil
}

// List 按 updated_at DESC, id DESC 分页列出自定义路由预设（username 非空时按账号过滤）。
func (r *Repo) List(ctx context.Context, username string, limit, offset int) ([]Preset, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []model.RoutingRulePreset
	q := r.db.NewSelect().Model(&rows)
	if u := strings.TrimSpace(username); u != "" {
		q = q.Where("username = ?", u)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "统计路由预设数量失败", err)
	}
	if err := q.OrderExpr("updated_at DESC, id DESC").Limit(limit).Offset(offset).Scan(ctx); err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "列出路由预设失败", err)
	}
	out := make([]Preset, len(rows))
	for i := range rows {
		out[i] = *toPreset(&rows[i])
	}
	return out, total, nil
}

// Delete 硬删除一条自定义路由预设（不存在返回 ErrNotFound）。
func (r *Repo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*model.RoutingRulePreset)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "删除路由预设失败", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func toPreset(m *model.RoutingRulePreset) *Preset {
	rule := m.RuleJSON
	if canon, err := xrpc.CanonicalJSON(rule); err == nil {
		rule = canon
	}
	return &Preset{
		ID:        m.ID,
		Username:  m.Username,
		Name:      m.Name,
		Rule:      rule,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}
