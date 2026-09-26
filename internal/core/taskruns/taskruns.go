// Package taskruns 是仓储层的内置任务运行记录：读写 task_runs（master-scheduler「运行记录」）。
// 这张表不是 kind、不是 append-only：开始时插一行 running，结束时原地改状态。直接用 bun，模型不出本包；时间一律按 UTC 存。
package taskruns

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 运行记录的状态。
const (
	StatusRunning = "running"
	StatusOK      = "ok"
	StatusError   = "error"
)

// Run 是一条运行记录；字段名就是 schedule runs list 输出的字段名。
type Run struct {
	ID         int64     `json:"id"`
	Task       string    `json:"task"`
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail"`
}

// Filter 是 schedule runs list 的过滤条件：任务名与状态都是精确匹配，空表示不过滤。
type Filter struct {
	Task   string
	Status string
}

// Repo 是运行记录的仓储。
type Repo struct {
	db *bun.DB
}

// New 建仓储。
func New(bdb *bun.DB) *Repo {
	return &Repo{db: bdb}
}

func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func runOf(row model.TaskRun) Run {
	return Run{ID: row.ID, Task: row.TaskName, StartedAt: row.StartedAt, DurationMs: row.DurationMs, Status: row.Status, Detail: row.Detail}
}

// Start 插一行 running，返回它的 id。
func (r *Repo) Start(ctx context.Context, task string, startedAt time.Time) (int64, error) {
	row := &model.TaskRun{TaskName: task, StartedAt: utc(startedAt), Status: StatusRunning}
	if _, err := r.db.NewInsert().Model(row).Returning("id").Exec(ctx); err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "写入任务运行记录失败", err)
	}
	return row.ID, nil
}

// Finish 把 id 那一行原地改成结束状态，写上耗时与结果。
func (r *Repo) Finish(ctx context.Context, id int64, duration time.Duration, status, detail string) error {
	_, err := r.db.NewUpdate().Model((*model.TaskRun)(nil)).
		Set("duration_ms = ?", duration.Milliseconds()).Set("status = ?", status).Set("detail = ?", detail).
		Where("id = ?", id).Exec(ctx)
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "更新任务运行记录失败", err)
	}
	return nil
}

// Insert 直接插一整行已经结束的记录（高频任务开始时不插 running，结束时才决定写不写）。
func (r *Repo) Insert(ctx context.Context, run Run) error {
	row := &model.TaskRun{TaskName: run.Task, StartedAt: utc(run.StartedAt), DurationMs: run.DurationMs, Status: run.Status, Detail: run.Detail}
	if _, err := r.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return v1.Wrap(v1.CodeDatabase, "写入任务运行记录失败", err)
	}
	return nil
}

// MarkInterrupted 把还是 running 的行改成 error（serve 启动时调：它们是上次主控停止时还在跑的），返回改了几行。
func (r *Repo) MarkInterrupted(ctx context.Context, detail string) (int, error) {
	res, err := r.db.NewUpdate().Model((*model.TaskRun)(nil)).
		Set("status = ?", StatusError).Set("detail = ?", detail).
		Where("status = ?", StatusRunning).Exec(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "收尾中断的任务运行记录失败", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func applyFilter(q *bun.SelectQuery, f Filter) *bun.SelectQuery {
	if f.Task != "" {
		q = q.Where("task_name = ?", f.Task)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	return q
}

// List 按 id 倒序取一页：beforeID 大于 0 时只取 id 小于它的（keyset 翻页），最多 limit 条。
func (r *Repo) List(ctx context.Context, f Filter, limit int, beforeID int64) ([]Run, error) {
	var rows []model.TaskRun
	q := applyFilter(r.db.NewSelect().Model(&rows), f).OrderExpr("id DESC").Limit(limit)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取任务运行记录失败", err)
	}
	out := make([]Run, 0, len(rows))
	for _, row := range rows {
		out = append(out, runOf(row))
	}
	return out, nil
}

// Count 数过滤条件下的总条数。
func (r *Repo) Count(ctx context.Context, f Filter) (int, error) {
	n, err := applyFilter(r.db.NewSelect().Model((*model.TaskRun)(nil)), f).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计任务运行记录失败", err)
	}
	return n, nil
}

// Latest 返回每个任务最近的一条记录（按 id），键是任务名。
func (r *Repo) Latest(ctx context.Context) (map[string]Run, error) {
	var rows []model.TaskRun
	sub := r.db.NewSelect().Model((*model.TaskRun)(nil)).ColumnExpr("MAX(id)").Group("task_name")
	if err := r.db.NewSelect().Model(&rows).Where("id IN (?)", sub).Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取任务最近的运行记录失败", err)
	}
	out := make(map[string]Run, len(rows))
	for _, row := range rows {
		out[row.TaskName] = runOf(row)
	}
	return out, nil
}

// DeleteBefore 删掉 started_at 早于 cutoff 的记录，返回删掉的行数。
func (r *Repo) DeleteBefore(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := store.PruneBefore(ctx, r.db, "task_runs", "started_at", cutoff, store.PruneBatch)
	if err != nil {
		return n, v1.Wrap(v1.CodeDatabase, "清理任务运行记录失败", err)
	}
	return n, nil
}
