// Package jobs 是仓储层的长任务：读写 jobs（kind Job，action；master-jobs）。写一律经写入原语：插入用 Insert，
// 状态变化用带「当前状态」前置条件的 UpdateStatus，所以同一个 job 不会被结束两次。返回领域结构 Job，模型不出本包。
package jobs

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

// job 的状态（schema 里 status 的枚举）。unknown 留给 M2 的 server exec（连接断开、结果未知）。
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusUnknown = "unknown"
)

// Job 是一个长任务；字段名就是 job get / job list 与长任务命令的输出字段名。Output 是结果对象或四字段错误的 JSON 文本。
type Job struct {
	ID              int64           `json:"-"`
	JobID           string          `json:"job_id"`
	Kind            string          `json:"kind"`
	Args            json.RawMessage `json:"args"`
	Status          string          `json:"status"`
	StartedAt       *time.Time      `json:"started_at"`
	FinishedAt      *time.Time      `json:"finished_at"`
	ExitCode        *int64          `json:"exit_code"`
	Output          *string         `json:"output"`
	OutputTruncated bool            `json:"output_truncated"`
	CreatedAt       time.Time       `json:"created_at"`
}

// Finished 报告 job 是否已经结束。
func (j *Job) Finished() bool {
	return j.Status == StatusDone || j.Status == StatusFailed || j.Status == StatusUnknown
}

// ErrNotFound 表示没有这个 job。
var ErrNotFound = errors.New("job not found")

// Repo 是长任务的仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

func jobOf(row *model.Job) *Job {
	return &Job{ID: row.ID, JobID: row.JobID, Kind: row.Kind, Args: row.Args, Status: row.Status, StartedAt: row.StartedAt,
		FinishedAt: row.FinishedAt, ExitCode: row.ExitCode, Output: row.Output, OutputTruncated: row.OutputTruncated, CreatedAt: row.CreatedAt}
}

func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// Insert 插一行 queued 的 job。
func (r *Repo) Insert(ctx context.Context, jobID, kind string, args json.RawMessage) (*Job, error) {
	row := &model.Job{JobID: jobID, Kind: kind, Args: args, Status: StatusQueued}
	if err := r.store.Insert(ctx, row); err != nil {
		return nil, err
	}
	return jobOf(row), nil
}

// SetRunning 把一个 queued 的 job 改成 running 并写开始时间。
func (r *Repo) SetRunning(ctx context.Context, id int64, at time.Time) error {
	started := utc(at)
	row := &model.Job{ID: id, Status: StatusRunning, StartedAt: &started}
	return r.store.UpdateStatus(ctx, row, []string{"status", "started_at"}, store.Cond{Column: "status", Value: StatusQueued})
}

// Finish 把一个处在 from 状态（通常是 running；改 running 失败时是 queued）的 job 改成结束状态，写结束时间、退出码与输出。
func (r *Repo) Finish(ctx context.Context, id int64, from, status string, exitCode int64, output string, truncated bool, at time.Time) error {
	finished := utc(at)
	row := &model.Job{ID: id, Status: status, FinishedAt: &finished, ExitCode: &exitCode, Output: &output, OutputTruncated: truncated}
	return r.store.UpdateStatus(ctx, row, []string{"status", "finished_at", "exit_code", "output", "output_truncated"},
		store.Cond{Column: "status", Value: from})
}

// MarkInterrupted 把还是 queued 或 running 的 job 改成 failed（serve 启动时调：它们是上次主控停止时还没结束的），返回改了几个。
func (r *Repo) MarkInterrupted(ctx context.Context, output string, at time.Time) (int, error) {
	var rows []model.Job
	if err := r.db.NewSelect().Model(&rows).Column("id", "status").Where("status IN (?)", bun.In([]string{StatusQueued, StatusRunning})).
		Where("deleted_at IS NULL").Scan(ctx); err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "读取没结束的长任务失败", err)
	}
	finished := utc(at)
	exit := int64(1)
	n := 0
	for _, row := range rows {
		upd := &model.Job{ID: row.ID, Status: StatusFailed, FinishedAt: &finished, ExitCode: &exit, Output: &output}
		err := r.store.UpdateStatus(ctx, upd, []string{"status", "finished_at", "exit_code", "output"}, store.Cond{Column: "status", Value: row.Status})
		if v1.AsError(err).Code == v1.CodeConflict {
			continue // 这期间已经结束了
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Get 按 job_id 找一个 job；没有返回 ErrNotFound。
func (r *Repo) Get(ctx context.Context, jobID string) (*Job, error) {
	var row model.Job
	err := r.db.NewSelect().Model(&row).Where("job_id = ?", jobID).Where("deleted_at IS NULL").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取长任务失败", err)
	}
	return jobOf(&row), nil
}

func filtered(q *bun.SelectQuery, status string) *bun.SelectQuery {
	q = q.Where("deleted_at IS NULL")
	if status != "" {
		q = q.Where("status = ?", status)
	}
	return q
}

// List 按 id 倒序取一页：beforeID 大于 0 时只取 id 小于它的（keyset 翻页），最多 limit 条；status 为空不过滤。
func (r *Repo) List(ctx context.Context, status string, limit int, beforeID int64) ([]*Job, error) {
	var rows []model.Job
	q := filtered(r.db.NewSelect().Model(&rows), status).OrderExpr("id DESC").Limit(limit)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取长任务失败", err)
	}
	out := make([]*Job, 0, len(rows))
	for i := range rows {
		out = append(out, jobOf(&rows[i]))
	}
	return out, nil
}

// Count 数过滤条件下的总数。
func (r *Repo) Count(ctx context.Context, status string) (int, error) {
	n, err := filtered(r.db.NewSelect().Model((*model.Job)(nil)), status).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计长任务失败", err)
	}
	return n, nil
}

// DeleteFinishedBefore 删掉 created_at 早于 cutoff、已经结束的 job，返回删掉的条数（job_cleanup 任务调）。
// 这是按保留期的物理清理，不是用户可见的删除，所以不走软删除；没结束的一律不删。
func (r *Repo) DeleteFinishedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := r.db.NewDelete().Model((*model.Job)(nil)).
		Where("status IN (?)", bun.In([]string{StatusDone, StatusFailed, StatusUnknown})).
		Where("created_at < ?", utc(cutoff)).Exec(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "清理长任务失败", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
