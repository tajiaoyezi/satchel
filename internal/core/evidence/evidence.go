// Package evidence 是仓储层的一键取证证据包（kind EvidencePackage，system 只写）：
// 复用 0001_init 已有 evidence_packages 表，零数据库迁移；所有写操作经 store 原语执行。
package evidence

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
)

// KindName 是 EvidencePackage 的 kind 名。
const KindName = "EvidencePackage"

// ErrNotFound 表示目标证据包不存在。
var ErrNotFound = errors.New("evidence package not found")

// EvidencePackage 是完整证据包资源对象（含 items 正文）。
type EvidencePackage = v1.Object[v1.EvidencePackageSpec, v1.EvidencePackageStatus]

// Summary 是 evidence list 返回的轻量摘要（不含 items 大段正文）。
type Summary struct {
	ID           int64      `json:"id"`
	Category     string     `json:"category"`
	ServerID     *int64     `json:"server_id"`
	CollectedAt  time.Time  `json:"collected_at"`
	CollectError *string    `json:"collect_error"`
	SizeBytes    int64      `json:"size_bytes"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Filter 是 evidence list 的过滤条件。
type Filter struct {
	ServerID *int64
	Category string
}

// CreateInput 是写入一份新证据包的字段。
type CreateInput struct {
	Category     string
	ServerID     *int64
	CollectedAt  time.Time
	Items        json.RawMessage
	CollectError *string
	SizeBytes    int64
	ExpiresAt    *time.Time
}

// Repo 是证据包仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func toObject(row *model.EvidencePackage) *EvidencePackage {
	items := row.Items
	if len(items) == 0 {
		items = json.RawMessage(`{}`)
	}
	return &EvidencePackage{
		Kind: KindName,
		Metadata: v1.Metadata{
			ID:        row.ID,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		},
		Spec: v1.EvidencePackageSpec{
			Category: row.Category,
			ServerID: row.ServerID,
		},
		Status: v1.EvidencePackageStatus{
			CollectedAt:  row.CollectedAt,
			Items:        items,
			CollectError: row.CollectError,
			SizeBytes:    row.SizeBytes,
			ExpiresAt:    row.ExpiresAt,
		},
	}
}

func toSummary(row *model.EvidencePackage) Summary {
	return Summary{
		ID:           row.ID,
		Category:     row.Category,
		ServerID:     row.ServerID,
		CollectedAt:  row.CollectedAt,
		CollectError: row.CollectError,
		SizeBytes:    row.SizeBytes,
		ExpiresAt:    row.ExpiresAt,
		CreatedAt:    row.CreatedAt,
	}
}

// Insert 写入一条新的 EvidencePackage（系统只写）。
func (r *Repo) Insert(ctx context.Context, in CreateInput) (*EvidencePackage, error) {
	cat := strings.TrimSpace(in.Category)
	if cat == "" {
		cat = "general"
	}
	collected := in.CollectedAt
	if collected.IsZero() {
		collected = time.Now().UTC()
	}
	items := in.Items
	if len(items) == 0 {
		items = json.RawMessage(`{}`)
	}
	var exp *time.Time
	if in.ExpiresAt != nil && !in.ExpiresAt.IsZero() {
		t := utc(*in.ExpiresAt)
		exp = &t
	}
	row := &model.EvidencePackage{
		Category:     cat,
		ServerID:     in.ServerID,
		CollectedAt:  utc(collected),
		Items:        items,
		CollectError: in.CollectError,
		SizeBytes:    in.SizeBytes,
		ExpiresAt:    exp,
	}
	if err := r.store.Insert(ctx, row); err != nil {
		return nil, err
	}
	return toObject(row), nil
}

// Get 按 ID 取完整证据包（含 items）。
func (r *Repo) Get(ctx context.Context, id int64) (*EvidencePackage, error) {
	var row model.EvidencePackage
	err := r.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取证据包失败", err)
	}
	return toObject(&row), nil
}

func applyFilter(q *bun.SelectQuery, f Filter) *bun.SelectQuery {
	if f.ServerID != nil {
		q = q.Where("server_id = ?", *f.ServerID)
	}
	if cat := strings.TrimSpace(f.Category); cat != "" {
		q = q.Where("category = ?", cat)
	}
	return q
}

// List 按 id 倒序分页列出证据包摘要（不含 items 正文）。
func (r *Repo) List(ctx context.Context, f Filter, limit int, beforeID int64) ([]Summary, error) {
	var rows []model.EvidencePackage
	q := applyFilter(r.db.NewSelect().Model(&rows).
		Column("id", "category", "server_id", "collected_at", "collect_error", "size_bytes", "expires_at", "created_at"), f).
		OrderExpr("id DESC").Limit(limit)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取证据包列表失败", err)
	}
	out := make([]Summary, 0, len(rows))
	for i := range rows {
		out = append(out, toSummary(&rows[i]))
	}
	return out, nil
}

// Count 统计满足条件的证据包总数。
func (r *Repo) Count(ctx context.Context, f Filter) (int, error) {
	n, err := applyFilter(r.db.NewSelect().Model((*model.EvidencePackage)(nil)), f).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计证据包数量失败", err)
	}
	return n, nil
}

// DeleteExpiredBefore 物理清理 expires_at 早于 cutoff 的已过期证据包（由 job_cleanup 定期调用）。
func (r *Repo) DeleteExpiredBefore(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := r.db.NewDelete().Model((*model.EvidencePackage)(nil)).
		Where("expires_at IS NOT NULL").
		Where("expires_at < ?", utc(cutoff)).
		Exec(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "清理过期证据包失败", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
