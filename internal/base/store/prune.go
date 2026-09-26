package store

import (
	"context"
	"time"

	"github.com/uptrace/bun"
)

// PruneBatch 是 PruneBefore 每批最多删的行数。
const PruneBatch = 1000

// PruneBefore 按保留期删记录表里的旧行（master-scheduler「本站的内置任务」）：按 id 从小到大每次看 batch 行，
// 删掉其中 timeColumn 早于 cutoff 的；碰到保留期以内的行、或一批不满 batch 行就停。返回删掉的总行数。
// 这些表的 id 自增、行按时间先后插入，所以按主键往后扫、碰到第一条新行就能停，不需要时间列上的索引（audit_logs 就没有）；
// 分批删避免一次删太多行时长时间占住 SQLite 的写锁。它是清理任务专用的：append-only 表只有这里会删行（storage-schema）。
func PruneBefore(ctx context.Context, db bun.IDB, table, timeColumn string, cutoff time.Time, batch int) (int, error) {
	total := 0
	for {
		n, more, err := pruneBatch(ctx, db, table, timeColumn, cutoff.UTC(), batch)
		total += n
		if err != nil || !more {
			return total, err
		}
	}
}

// pruneBatch 在一个事务里看一批、删一批；more 报告要不要接着看下一批。
func pruneBatch(ctx context.Context, db bun.IDB, table, timeColumn string, cutoff time.Time, batch int) (deleted int, more bool, err error) {
	err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var rows []struct {
			ID  int64 `bun:"id"`
			Old int   `bun:"old"`
		}
		err := tx.NewSelect().Table(table).
			ColumnExpr("id").
			ColumnExpr("CASE WHEN ? < ? THEN 1 ELSE 0 END AS old", bun.Ident(timeColumn), cutoff).
			OrderExpr("id ASC").Limit(batch).Scan(ctx, &rows)
		if err != nil {
			return err
		}
		var last int64
		reachedNew := false
		for _, r := range rows {
			if r.Old == 0 {
				reachedNew = true
				break
			}
			last = r.ID
		}
		more = !reachedNew && len(rows) == batch
		if last == 0 {
			return nil
		}
		// 删的时候再核对一次时间：PostgreSQL 上一个 id 更小、刚才还没提交的新行，这时可能已经提交，id 落在 last 以内也不能删。
		res, err := tx.NewDelete().Table(table).Where("id <= ?", last).Where("? < ?", bun.Ident(timeColumn), cutoff).Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted = int(n)
		return nil
	})
	return deleted, more, err
}
