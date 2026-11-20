// Package batch 是仓储层的批量类滚动窗口计数器（表 batch_op_counters，append-only）：
// 不依赖其它 internal/core/* 包，提供按身份/令牌汇总滚动时间窗操作对象数与持久化记账能力。
package batch

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Repo 是批量操作滚动窗口计数器仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// WindowUsed 返回指定身份自 since 以来在滚动时间窗内已累计操作的对象总数。
// 令牌身份按 token_id 统计；非令牌身份（本机管理员 / 账号会话）按 actor 且 token_id IS NULL 统计。
func (r *Repo) WindowUsed(ctx context.Context, id v1.Identity, since time.Time) (int64, error) {
	return r.windowUsed(ctx, r.db, id, since)
}

// WindowUsedTx 在事务内查询滚动时间窗累计对象数。
func (r *Repo) WindowUsedTx(ctx context.Context, tx bun.Tx, id v1.Identity, since time.Time) (int64, error) {
	return r.windowUsed(ctx, tx, id, since)
}

func (r *Repo) windowUsed(ctx context.Context, q bun.IDB, id v1.Identity, since time.Time) (int64, error) {
	var total int64
	sel := q.NewSelect().
		TableExpr("batch_op_counters").
		ColumnExpr("COALESCE(SUM(objects), 0)").
		Where("at >= ?", since.UTC().Truncate(time.Microsecond))
	if id.TokenID != nil {
		sel = sel.Where("token_id = ?", *id.TokenID)
	} else {
		sel = sel.Where("token_id IS NULL AND actor = ?", id.Actor)
	}
	if err := sel.Scan(ctx, &total); err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "查询批量操作滚动窗口计数失败", err)
	}
	return total, nil
}

// Record 向 batch_op_counters 追加一条记账记录（objects <= 0 时忽略）。
func (r *Repo) Record(ctx context.Context, id v1.Identity, objects int64, cmdName string, at time.Time) error {
	return r.record(ctx, r.store, id, objects, cmdName, at)
}

// RecordTx 在事务内向 batch_op_counters 追加一条记账记录（主控重启不清零）。
func (r *Repo) RecordTx(ctx context.Context, tx bun.Tx, id v1.Identity, objects int64, cmdName string, at time.Time) error {
	return r.record(ctx, r.store.WithTx(tx), id, objects, cmdName, at)
}

func (r *Repo) record(ctx context.Context, st *store.Store, id v1.Identity, objects int64, cmdName string, at time.Time) error {
	if objects <= 0 {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var tokID *int64
	if id.TokenID != nil {
		v := *id.TokenID
		tokID = &v
	}
	m := &model.BatchOpCounter{
		TokenID: tokID,
		Actor:   id.Actor,
		At:      at.UTC().Truncate(time.Microsecond),
		Objects: objects,
		Command: cmdName,
	}
	return st.Insert(ctx, m)
}
