// Package plans 是仓储层的配置变更预演计划（kind Plan，action 类别）：
// 不依赖其它 internal/core/* 包，所有写操作经 store 原语执行。
package plans

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

// KindName 是 Plan 的 kind 名。
const KindName = "Plan"

// Plan 状态枚举（对应 schema 中 plans.status）。
const (
	StatusPending  = "pending"
	StatusApplied  = "applied"
	StatusExpired  = "expired"
	StatusRejected = "rejected"
)

// DefaultTTL 是计划的默认有效期（10 分钟）。
const DefaultTTL = 10 * time.Minute

// Plan 是预演计划资源对象。
type Plan = v1.Object[v1.PlanSpec, v1.PlanStatus]

// Repo 是计划仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// CreateParams 是新建预演计划的参数。
type CreateParams struct {
	Objects       json.RawMessage
	Diff          json.RawMessage
	AffectedCount int64
	ExpiresAt     time.Time
}

// Create 插入一条新的 pending 预演计划。
func (r *Repo) Create(ctx context.Context, p CreateParams) (*Plan, error) {
	if len(p.Objects) == 0 {
		p.Objects = json.RawMessage(`[]`)
	}
	if len(p.Diff) == 0 {
		p.Diff = json.RawMessage(`{}`)
	}
	m := &model.Plan{
		Objects:       p.Objects,
		Diff:          p.Diff,
		AffectedCount: p.AffectedCount,
		ExpiresAt:     p.ExpiresAt.UTC().Truncate(time.Microsecond),
		Status:        StatusPending,
	}
	if err := r.store.Insert(ctx, m); err != nil {
		return nil, err
	}
	return toObject(m), nil
}

// Get 按 id 读取计划；不存在返回 v1.CodeNotFound。
func (r *Repo) Get(ctx context.Context, id int64) (*Plan, error) {
	return r.get(ctx, r.db, id)
}

// GetTx 在事务中按 id 读取计划。
func (r *Repo) GetTx(ctx context.Context, tx bun.Tx, id int64) (*Plan, error) {
	return r.get(ctx, tx, id)
}

func (r *Repo) get(ctx context.Context, q bun.IDB, id int64) (*Plan, error) {
	m := new(model.Plan)
	err := q.NewSelect().Model(m).Where("id = ?", id).Where("deleted_at IS NULL").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, v1.Newf(v1.CodeNotFound, "Plan 里没有 id 为 %d 的计划", id)
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取计划失败", err)
	}
	return toObject(m), nil
}

// EffectiveStatus 返回计划在 now 时刻的实际状态：库里仍为 pending 但超过 expires_at 的视为 expired。
func EffectiveStatus(p *Plan, now time.Time) string {
	if p.Status.Status == StatusPending && !now.Before(p.Spec.ExpiresAt) {
		return StatusExpired
	}
	return p.Status.Status
}

// Count 统计满足状态过滤的计划总数。
func (r *Repo) Count(ctx context.Context, status string, now time.Time) (int, error) {
	q := applyStatusFilter(r.db.NewSelect().Model((*model.Plan)(nil)).Where("deleted_at IS NULL"), status, now)
	n, err := q.Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计计划失败", err)
	}
	return n, nil
}

// List 按 id 倒序分页列出计划（beforeID > 0 时查 id < beforeID）。
func (r *Repo) List(ctx context.Context, status string, now time.Time, beforeID int64, limit int) ([]*Plan, error) {
	var rows []model.Plan
	q := applyStatusFilter(r.db.NewSelect().Model(&rows).Where("deleted_at IS NULL"), status, now)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.OrderExpr("id DESC").Limit(limit).Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出计划失败", err)
	}
	out := make([]*Plan, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, nil
}

func applyStatusFilter(q *bun.SelectQuery, status string, now time.Time) *bun.SelectQuery {
	now = now.UTC().Truncate(time.Microsecond)
	switch status {
	case "":
		return q
	case StatusPending:
		return q.Where("status = ? AND expires_at > ?", StatusPending, now)
	case StatusExpired:
		return q.Where("(status = ? OR (status = ? AND expires_at <= ?))", StatusExpired, StatusPending, now)
	default:
		return q.Where("status = ?", status)
	}
}

// MarkAppliedTx 在事务中将 pending 状态的计划原子流转为 applied 并记录 apply_id。
func (r *Repo) MarkAppliedTx(ctx context.Context, tx bun.Tx, id int64, applyID string) error {
	m := &model.Plan{
		ID:      id,
		Status:  StatusApplied,
		ApplyID: &applyID,
	}
	return r.store.WithTx(tx).UpdateStatus(ctx, m, []string{"status", "apply_id"}, store.Cond{Column: "status", Value: StatusPending})
}

// MarkExpired 将已过期的 pending 计划落盘标为 expired（忽略已被并发流转的 conflict）。
func (r *Repo) MarkExpired(ctx context.Context, id int64) error {
	m := &model.Plan{
		ID:     id,
		Status: StatusExpired,
	}
	err := r.store.UpdateStatus(ctx, m, []string{"status"}, store.Cond{Column: "status", Value: StatusPending})
	if err != nil && v1.AsError(err).Code == v1.CodeConflict {
		return nil
	}
	return err
}

func toObject(m *model.Plan) *Plan {
	return &Plan{
		APIVersion: v1.APIVersion,
		Kind:       v1.Kind(KindName),
		Metadata: v1.Metadata{
			ID:              m.ID,
			ResourceVersion: m.ResourceVersion,
			CreatedAt:       m.CreatedAt,
			UpdatedAt:       m.UpdatedAt,
			DeletedAt:       m.DeletedAt,
		},
		Spec: v1.PlanSpec{
			Objects:       m.Objects,
			Diff:          m.Diff,
			AffectedCount: m.AffectedCount,
			ShareIdentity: m.ShareIdentity,
			ShareScope:    m.ShareScope,
			ExpiresAt:     m.ExpiresAt,
		},
		Status: v1.PlanStatus{
			Status:  m.Status,
			ApplyID: m.ApplyID,
		},
	}
}
