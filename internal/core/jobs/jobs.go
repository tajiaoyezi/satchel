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
	// Progress 是运行中的长任务自己报告的进度（只在主控内存里，不入库；service/jobs 在返回时填上）。
	Progress any `json:"progress,omitempty"`
}

// Finished 报告 job 是否已经结束。
func (j *Job) Finished() bool {
	return j.Status == StatusDone || j.Status == StatusFailed || j.Status == StatusUnknown
}

// ErrNotFound 表示没有这个 job。
var ErrNotFound = errors.New("job not found")

// ErrHandedOff 由长任务的工作返回，表示这个 job 的结局由别处写、这里不写（自升级：exec 之后由新进程或回退后的旧进程收尾，
// master-self-update）。job 行保持 running。
var ErrHandedOff = errors.New("job handed off")

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
// skip 里的 job_id 不动（升级标记里的那个，由 master-self-update 收尾）。
func (r *Repo) MarkInterrupted(ctx context.Context, output string, at time.Time, skip ...string) (int, error) {
	var rows []model.Job
	q := r.db.NewSelect().Model(&rows).Column("id", "status").Where("status IN (?)", bun.In([]string{StatusQueued, StatusRunning})).
		Where("deleted_at IS NULL")
	if len(skip) > 0 {
		q = q.Where("job_id NOT IN (?)", bun.In(skip))
	}
	if err := q.Scan(ctx); err != nil {
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

// MarkDoneIn 在 q（在线迁移的目标库事务）里把一个 job 改成 done：迁移拷过去的这一行还是 running，重启后看到的应当是完成的。
// 目标库不是本仓储管的库，所以直接写这一行，不经写入原语。
func MarkDoneIn(ctx context.Context, q bun.IDB, jobID, output string, at time.Time) error {
	_, err := q.NewUpdate().Model((*model.Job)(nil)).
		Set("status = ?", StatusDone).Set("exit_code = ?", 0).Set("output = ?", output).Set("finished_at = ?", utc(at)).Set("updated_at = ?", utc(at)).
		Where("job_id = ?", jobID).Exec(ctx)
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "在目标库里标记迁移完成失败", err)
	}
	return nil
}

// runKey 让长任务的工作 ctx 带上自己的 job_id 与报进度的函数（service/jobs 在 Start 时放进去）。放在仓储层，
// 是为了让别的业务模块（如在线迁移）不必引用 service/jobs 就能报进度、知道自己是哪个 job。
type runKey struct{}

type runInfo struct {
	id     string
	report func(any)
}

// WithRun 给工作 ctx 带上 job_id 与报进度的函数。
func WithRun(ctx context.Context, jobID string, report func(any)) context.Context {
	return context.WithValue(ctx, runKey{}, runInfo{jobID, report})
}

// Report 报告当前长任务的进度（master-jobs「查看 job」的 progress）；ctx 不是长任务的工作 ctx 时什么都不做。
func Report(ctx context.Context, v any) {
	if r, ok := ctx.Value(runKey{}).(runInfo); ok && r.report != nil {
		r.report(v)
	}
}

// CurrentID 返回当前长任务的 job_id；ctx 不是长任务的工作 ctx 时是空。
func CurrentID(ctx context.Context) string {
	r, _ := ctx.Value(runKey{}).(runInfo)
	return r.id
}
