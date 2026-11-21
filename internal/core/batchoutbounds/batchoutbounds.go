// Package batchoutbounds 是仓储层的多机批量出站批次记录（表 batch_outbounds）：
// 严守单向分层纪律，不依赖其它 internal/core/* 包。
package batchoutbounds

import (
	"context"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Entry 是一条多机批量出站批次记录。
type Entry struct {
	ID        int64     `json:"id"`
	BatchID   string    `json:"batch_id"`
	Tag       string    `json:"tag"`
	ServerID  int64     `json:"server_id"`
	Protocol  string    `json:"protocol"`
	CreatedAt time.Time `json:"created_at"`
}

// Repo 是 batch_outbounds 仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// InsertTx 在事务内批量写入多机出站批次记录。
func (r *Repo) InsertTx(ctx context.Context, tx bun.Tx, entries []Entry) error {
	st := r.store.WithTx(tx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, e := range entries {
		createdAt := e.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		m := &model.BatchOutbound{
			BatchID:   strings.TrimSpace(e.BatchID),
			Tag:       strings.TrimSpace(e.Tag),
			ServerID:  e.ServerID,
			Protocol:  strings.TrimSpace(e.Protocol),
			CreatedAt: createdAt,
		}
		if err := st.Insert(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// List 按 id 倒序分页列出多机出站批次记录，支持按 batch_id 与 server_id 过滤。
func (r *Repo) List(ctx context.Context, batchID string, serverID int64, limit, offset int) ([]Entry, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []model.BatchOutbound
	q := r.db.NewSelect().Model(&rows)
	if bid := strings.TrimSpace(batchID); bid != "" {
		q = q.Where("batch_id = ?", bid)
	}
	if serverID > 0 {
		q = q.Where("server_id = ?", serverID)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "统计批量出站记录失败", err)
	}
	if err := q.OrderExpr("id DESC").Limit(limit).Offset(offset).Scan(ctx); err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "列出批量出站记录失败", err)
	}
	out := make([]Entry, len(rows))
	for i, m := range rows {
		out[i] = Entry{
			ID:        m.ID,
			BatchID:   m.BatchID,
			Tag:       m.Tag,
			ServerID:  m.ServerID,
			Protocol:  m.Protocol,
			CreatedAt: m.CreatedAt,
		}
	}
	return out, total, nil
}
