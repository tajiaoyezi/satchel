// Package audit 是仓储层的审计记录：读写 audit_logs，返回领域结构 Record，模型不出本包。
package audit

import (
	"context"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Record 是一条审计记录；字段名就是 audit list 输出的字段名。
type Record struct {
	ID         int64        `json:"id"`
	At         time.Time    `json:"at"`
	Actor      string       `json:"actor"`
	ActorKind  v1.ActorKind `json:"actor_kind"`
	TokenID    *int64       `json:"token_id"`
	Command    string       `json:"command"`
	ArgsDigest string       `json:"args_digest"`
	PlanID     *int64       `json:"plan_id"`
	Result     string       `json:"result"`
}

// Filter 是 audit list 的过滤条件：actor 精确、command 前缀、at 不早于 since。
type Filter struct {
	Actor         string
	CommandPrefix string
	Since         *time.Time
}

// Repo 是审计记录的仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(db *bun.DB, st *store.Store) *Repo {
	return &Repo{db: db, store: st}
}

// Insert 写一条记录（append-only 表，只有插入）。
func (r *Repo) Insert(ctx context.Context, rec Record) error {
	row := &model.AuditLog{
		At: rec.At, Actor: rec.Actor, ActorKind: string(rec.ActorKind), TokenID: rec.TokenID,
		Command: rec.Command, ArgsDigest: rec.ArgsDigest, PlanID: rec.PlanID, Result: rec.Result,
	}
	return r.store.Insert(ctx, row)
}

// List 按 id 倒序取一页：beforeID 大于 0 时只取 id 小于它的（keyset 翻页），最多 limit 条。
func (r *Repo) List(ctx context.Context, f Filter, limit int, beforeID int64) ([]Record, error) {
	var rows []model.AuditLog
	q := r.db.NewSelect().Model(&rows).OrderExpr("id DESC").Limit(limit)
	q = applyFilter(q, f)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取审计记录失败", err)
	}
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, Record{
			ID: row.ID, At: row.At, Actor: row.Actor, ActorKind: v1.ActorKind(row.ActorKind), TokenID: row.TokenID,
			Command: row.Command, ArgsDigest: row.ArgsDigest, PlanID: row.PlanID, Result: row.Result,
		})
	}
	return out, nil
}

// Count 数过滤条件下的总条数。
func (r *Repo) Count(ctx context.Context, f Filter) (int, error) {
	n, err := applyFilter(r.db.NewSelect().Model((*model.AuditLog)(nil)), f).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计审计记录失败", err)
	}
	return n, nil
}

func applyFilter(q *bun.SelectQuery, f Filter) *bun.SelectQuery {
	if f.Actor != "" {
		q = q.Where("actor = ?", f.Actor)
	}
	if f.CommandPrefix != "" {
		q = q.Where("command LIKE ? ESCAPE '\\'", escapeLike(f.CommandPrefix)+"%")
	}
	if f.Since != nil {
		q = q.Where("at >= ?", f.Since.UTC())
	}
	return q
}

// escapeLike 把前缀里的通配符转义，两库的 LIKE 都认 ESCAPE '\'。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// DeleteBefore 删掉 at 早于 cutoff 的记录，返回删掉的条数（audit_cleanup 任务调；append-only 表只有按保留期的清理会删行）。
func (r *Repo) DeleteBefore(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := store.PruneBefore(ctx, r.db, "audit_logs", "at", cutoff, store.PruneBatch)
	if err != nil {
		return n, v1.Wrap(v1.CodeDatabase, "清理审计记录失败", err)
	}
	return n, nil
}
