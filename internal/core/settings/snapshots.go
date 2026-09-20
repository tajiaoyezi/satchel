package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/satchel/satchel/internal/base/model"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// StatusSaved 是设置快照的 status：写前内容已完整落库。表是 append-only 的，它不会再变。
const StatusSaved = "saved"

// 快照的 source：设置写接口与回滚。
const (
	SourceSettings = "settings"
	SourceRollback = "settings_rollback"
)

// SnapshotMeta 是快照的元数据；字段名就是 settings snapshots list 输出的字段名。内容不在这里（里面有原文密钥）。
type SnapshotMeta struct {
	ID            int64     `json:"id"`
	ObjectVersion int64     `json:"object_version"`
	CreatedAt     time.Time `json:"created_at"`
	Source        string    `json:"source"`
	ContentHash   string    `json:"content_hash"`
}

// Snapshot 是带内容的快照，回滚用。
type Snapshot struct {
	SnapshotMeta
	Content json.RawMessage
}

// ListSnapshots 按 id 倒序取本单例的一页快照：beforeID 大于 0 时只取 id 小于它的（keyset 翻页），最多 limit 条。
func (r *Repo) ListSnapshots(ctx context.Context, limit int, beforeID int64) ([]SnapshotMeta, error) {
	var rows []model.ConfigSnapshot
	q := r.db.NewSelect().Model(&rows).
		ExcludeColumn("content").
		Where("object_kind = ? AND object_id = ?", KindName, SingletonID).
		OrderExpr("id DESC").Limit(limit)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取设置快照失败", err)
	}
	out := make([]SnapshotMeta, 0, len(rows))
	for _, row := range rows {
		out = append(out, metaOf(&row))
	}
	return out, nil
}

// CountSnapshots 数本单例的快照总数。
func (r *Repo) CountSnapshots(ctx context.Context) (int, error) {
	n, err := r.db.NewSelect().Model((*model.ConfigSnapshot)(nil)).
		Where("object_kind = ? AND object_id = ?", KindName, SingletonID).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计设置快照失败", err)
	}
	return n, nil
}

// GetSnapshot 读一份快照（含内容）；不存在、或不是本单例的快照都是 not_found。
func (r *Repo) GetSnapshot(ctx context.Context, id int64) (*Snapshot, error) {
	var row model.ConfigSnapshot
	err := r.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (row.ObjectKind != KindName || row.ObjectID != SingletonID)) {
		return nil, v1.Newf(v1.CodeNotFound, "没有 id 为 %d 的系统设置快照", id).WithNext("用 settings snapshots list 查看可用的快照")
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取设置快照失败", err)
	}
	return &Snapshot{SnapshotMeta: metaOf(&row), Content: row.Content}, nil
}

func metaOf(row *model.ConfigSnapshot) SnapshotMeta {
	return SnapshotMeta{ID: row.ID, ObjectVersion: row.ObjectVersion, CreatedAt: row.CreatedAt, Source: row.Source, ContentHash: row.ContentHash}
}
