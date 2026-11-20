// Package routingrules 是仓储层的路由规则（kind RoutingRule，config 类别）：
// 不依赖其它 internal/core/* 包，所有写操作经 store 原语执行。
package routingrules

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// KindName 是 RoutingRule 的 kind 名。
const KindName = "RoutingRule"

// ErrNotFound 表示目标路由规则不存在。
var ErrNotFound = errors.New("routing rule not found")

// RoutingRule 是路由规则资源对象。
type RoutingRule = v1.Object[v1.RoutingRuleSpec, v1.RoutingRuleStatus]

var specColumns = []string{"server_id", "sort_order", "match", "outbound_tag", "enabled", "catch_all"}

// Repo 是路由规则仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// NormalizeSpec 规范并校验 RoutingRuleSpec。
func NormalizeSpec(spec v1.RoutingRuleSpec) (v1.RoutingRuleSpec, error) {
	spec.OutboundTag = strings.TrimSpace(spec.OutboundTag)
	if spec.ServerID <= 0 {
		return spec, v1.New(v1.CodeBadRequest, "RoutingRule.spec.server_id 必须为正整数")
	}
	if spec.OutboundTag == "" {
		return spec, v1.New(v1.CodeBadRequest, "RoutingRule.spec.outbound_tag 不能为空")
	}
	trimmed := bytes.TrimSpace(spec.Match)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		spec.Match = json.RawMessage(`{}`)
	} else if !json.Valid(trimmed) {
		return spec, v1.New(v1.CodeBadRequest, "RoutingRule.spec.match 不是合法的 JSON")
	} else {
		spec.Match = trimmed
	}
	return spec, nil
}

// ByID 读取未软删除路由规则。
func (r *Repo) ByID(ctx context.Context, id int64) (*RoutingRule, error) {
	return r.byID(ctx, r.db, id, false)
}

// ByIDIncludeDeleted 按 id 读取路由规则（含软删除行）。
func (r *Repo) ByIDIncludeDeleted(ctx context.Context, id int64) (*RoutingRule, error) {
	return r.byID(ctx, r.db, id, true)
}

// ByIDTx 在事务中按 id 读取路由规则。
func (r *Repo) ByIDTx(ctx context.Context, tx bun.Tx, id int64, includeDeleted bool) (*RoutingRule, error) {
	return r.byID(ctx, tx, id, includeDeleted)
}

func (r *Repo) byID(ctx context.Context, q bun.IDB, id int64, includeDeleted bool) (*RoutingRule, error) {
	m := new(model.RoutingRule)
	sel := q.NewSelect().Model(m).Where("id = ?", id)
	if !includeDeleted {
		sel = sel.Where("deleted_at IS NULL")
	}
	err := sel.Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取路由规则失败", err)
	}
	return toObject(m), nil
}

// ListByServer 列出单台服务器未软删除的全部路由规则，按 sort_order ASC, id ASC 排序。
func (r *Repo) ListByServer(ctx context.Context, serverID int64) ([]*RoutingRule, error) {
	return r.listByServer(ctx, r.db, serverID)
}

// ListByServerTx 在事务内列出单台服务器未软删除的全部路由规则。
func (r *Repo) ListByServerTx(ctx context.Context, tx bun.Tx, serverID int64) ([]*RoutingRule, error) {
	return r.listByServer(ctx, tx, serverID)
}

func (r *Repo) listByServer(ctx context.Context, q bun.IDB, serverID int64) ([]*RoutingRule, error) {
	var rows []model.RoutingRule
	err := q.NewSelect().Model(&rows).
		Where("server_id = ?", serverID).
		Where("deleted_at IS NULL").
		OrderExpr("sort_order ASC, id ASC").
		Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出路由规则失败", err)
	}
	out := make([]*RoutingRule, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, nil
}

// CreateTx 在事务中插入新路由规则。
func (r *Repo) CreateTx(ctx context.Context, tx bun.Tx, spec v1.RoutingRuleSpec) (*RoutingRule, error) {
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	m := modelFromSpec(norm)
	if err := r.store.WithTx(tx).Insert(ctx, m); err != nil {
		return nil, err
	}
	return toObject(m), nil
}

// UpdateTx 在事务中更新未软删除路由规则：禁止跨 server_id 迁移。
func (r *Repo) UpdateTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool, spec v1.RoutingRuleSpec) (*RoutingRule, error) {
	cur, err := r.byID(ctx, tx, id, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "RoutingRule 里没有 id 为 %d 的对象", id)
		}
		return nil, err
	}
	if cur.Metadata.DeletedAt != nil {
		return nil, v1.Newf(v1.CodeConflict, "RoutingRule %d 处于软删除状态，不能修改 spec（需先 restore）", id)
	}
	if spec.ServerID != cur.Spec.ServerID {
		return nil, v1.Newf(v1.CodeBadRequest, "RoutingRule %d 不允许跨服务器修改 server_id（当前 %d，尝试改为 %d）", id, cur.Spec.ServerID, spec.ServerID)
	}
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	m := modelFromSpec(norm)
	m.ID = id
	m.ResourceVersion = expectedVersion
	if err := r.store.WithTx(tx).UpdateSpec(ctx, m, force, specColumns...); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

// SoftDeleteTx 在事务中软删除路由规则。
func (r *Repo) SoftDeleteTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*RoutingRule, error) {
	m := &model.RoutingRule{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).SoftDelete(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, true)
}

// RestoreTx 在事务中恢复路由规则。
func (r *Repo) RestoreTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*RoutingRule, error) {
	m := &model.RoutingRule{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).Restore(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

func modelFromSpec(spec v1.RoutingRuleSpec) *model.RoutingRule {
	return &model.RoutingRule{
		ServerID:    spec.ServerID,
		SortOrder:   spec.SortOrder,
		Match:       spec.Match,
		OutboundTag: spec.OutboundTag,
		Enabled:     spec.Enabled,
		CatchAll:    spec.CatchAll,
	}
}

func toObject(m *model.RoutingRule) *RoutingRule {
	return &RoutingRule{
		APIVersion: v1.APIVersion,
		Kind:       v1.Kind(KindName),
		Metadata: v1.Metadata{
			ID:              m.ID,
			ResourceVersion: m.ResourceVersion,
			CreatedAt:       m.CreatedAt,
			UpdatedAt:       m.UpdatedAt,
			DeletedAt:       m.DeletedAt,
		},
		Spec: v1.RoutingRuleSpec{
			ServerID:    m.ServerID,
			SortOrder:   m.SortOrder,
			Match:       m.Match,
			OutboundTag: m.OutboundTag,
			Enabled:     m.Enabled,
			CatchAll:    m.CatchAll,
		},
	}
}
