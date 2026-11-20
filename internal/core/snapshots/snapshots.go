// Package snapshots 是仓储层的通用配置快照（kind ConfigSnapshot，append-only）：
// 不依赖其它 internal/core/* 包，负责写前快照追加、分页列表与按 id / apply_id 读取。
package snapshots

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
	"github.com/satchel/satchel/pkg/xrpc"
)

// KindName 是 ConfigSnapshot 的 kind 名。
const KindName = "ConfigSnapshot"

// 常见来源与状态常量。
const (
	SourceApply    = "apply"
	SourceRollback = "rollback"
	StatusSaved    = "saved"
)

// Summary 是 snapshot list 返回的元数据条目（不含 content 正文）。
type Summary struct {
	ID            int64     `json:"id"`
	ObjectKind    string    `json:"object_kind"`
	ObjectID      int64     `json:"object_id"`
	ObjectVersion int64     `json:"object_version"`
	ContentHash   string    `json:"content_hash"`
	Source        string    `json:"source"`
	Status        string    `json:"status"`
	ApplyID       *string   `json:"apply_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Snapshot 是带完整 content 的单条快照记录。
type Snapshot struct {
	Summary
	Content json.RawMessage `json:"content"`
}

// Filter 是 snapshot list 的可选过滤条件。
type Filter struct {
	Kind     string
	ObjectID int64
	ApplyID  string
}

// AppendInput 是写前快照的追加参数。
type AppendInput struct {
	ObjectKind    string
	ObjectID      int64
	ObjectVersion int64
	Content       json.RawMessage
	Source        string
	Status        string
	ApplyID       string
}

// Repo 是配置快照仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// AppendTx 在事务中追加一条写前快照；content 会先规范化并计算稳定 SHA-256 哈希。
func (r *Repo) AppendTx(ctx context.Context, tx bun.Tx, in AppendInput) (*Snapshot, error) {
	canonical, err := xrpc.CanonicalJSON(in.Content)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "规范化快照内容失败", err)
	}
	status := in.Status
	if status == "" {
		status = StatusSaved
	}
	source := in.Source
	if source == "" {
		source = SourceApply
	}
	var applyID *string
	if in.ApplyID != "" {
		v := in.ApplyID
		applyID = &v
	}
	m := &model.ConfigSnapshot{
		ObjectKind:    in.ObjectKind,
		ObjectID:      in.ObjectID,
		ObjectVersion: in.ObjectVersion,
		Content:       canonical,
		ContentHash:   xrpc.CanonicalConfigHash(canonical),
		Source:        source,
		Status:        status,
		ApplyID:       applyID,
	}
	if err := r.store.WithTx(tx).Insert(ctx, m); err != nil {
		return nil, err
	}
	return toSnapshot(m), nil
}

// Get 按 id 读单条快照（含 content）；不存在返回 v1.CodeNotFound。
func (r *Repo) Get(ctx context.Context, id int64) (*Snapshot, error) {
	m := new(model.ConfigSnapshot)
	err := r.db.NewSelect().Model(m).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, v1.Newf(v1.CodeNotFound, "没有 id 为 %d 的配置快照", id)
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取配置快照失败", err)
	}
	return toSnapshot(m), nil
}

// ListByApplyID 按 apply_id 查出整批快照（含 content，按 id ASC 排序）。
func (r *Repo) ListByApplyID(ctx context.Context, applyID string) ([]*Snapshot, error) {
	var rows []model.ConfigSnapshot
	err := r.db.NewSelect().Model(&rows).
		Where("apply_id = ?", applyID).
		OrderExpr("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "按 apply_id 查询配置快照失败", err)
	}
	out := make([]*Snapshot, len(rows))
	for i := range rows {
		out[i] = toSnapshot(&rows[i])
	}
	return out, nil
}

// Count 统计满足过滤条件的快照总数。
func (r *Repo) Count(ctx context.Context, f Filter) (int, error) {
	q := applyFilter(r.db.NewSelect().Model((*model.ConfigSnapshot)(nil)), f)
	n, err := q.Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计配置快照失败", err)
	}
	return n, nil
}

// List 按 id 倒序分页列出快照摘要（不读 content 正文）。
func (r *Repo) List(ctx context.Context, f Filter, beforeID int64, limit int) ([]Summary, error) {
	var rows []model.ConfigSnapshot
	q := applyFilter(r.db.NewSelect().Model(&rows).
		Column("id", "object_kind", "object_id", "object_version", "content_hash", "source", "status", "apply_id", "created_at"), f)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.OrderExpr("id DESC").Limit(limit).Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出配置快照失败", err)
	}
	out := make([]Summary, len(rows))
	for i := range rows {
		out[i] = toSummary(&rows[i])
	}
	return out, nil
}

func applyFilter(q *bun.SelectQuery, f Filter) *bun.SelectQuery {
	if f.Kind != "" {
		q = q.Where("object_kind = ?", f.Kind)
	}
	if f.ObjectID > 0 {
		q = q.Where("object_id = ?", f.ObjectID)
	}
	if f.ApplyID != "" {
		q = q.Where("apply_id = ?", f.ApplyID)
	}
	return q
}

func toSummary(m *model.ConfigSnapshot) Summary {
	return Summary{
		ID:            m.ID,
		ObjectKind:    m.ObjectKind,
		ObjectID:      m.ObjectID,
		ObjectVersion: m.ObjectVersion,
		ContentHash:   m.ContentHash,
		Source:        m.Source,
		Status:        m.Status,
		ApplyID:       m.ApplyID,
		CreatedAt:     m.CreatedAt,
	}
}

func toSnapshot(m *model.ConfigSnapshot) *Snapshot {
	return &Snapshot{
		Summary: toSummary(m),
		Content: m.Content,
	}
}
