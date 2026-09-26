// Package schedule 是业务层的内置任务运行记录（master-scheduler「运行记录」「查看任务与运行记录」）：写 task_runs 的 Recorder
// （含高频任务的节流）、启动时收尾上次中断的记录、按保留期清理，以及 schedule list 与 schedule runs list 两条命令。
// 任务怎么按时运行在投影层的 scheduler；它引用本包，本包不引用它，所以任务的元数据 TaskInfo 定义在这里。
package schedule

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/taskruns"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

const (
	// RunRetention 是任务记录的保留期（照 mmwx）。
	RunRetention = 7 * 24 * time.Hour
	// quietBelow：间隔短于它的任务，成功的记录每 quietBelow 最多一条，开始时也不插 running 行。
	quietBelow = time.Hour
	// interruptedDetail 是启动时收尾上次中断的记录时写的 detail。
	interruptedDetail = "主控停止时它还在运行"
	// writeTimeout 是结束时写记录的时限：serve 停止时任务的 ctx 已经取消，记录仍要写进去。
	writeTimeout = 5 * time.Second
)

// TaskInfo 是一个内置任务的元数据：名字（小写字母与下划线）、一句话说明、运行间隔。
type TaskInfo struct {
	Name    string
	Summary string
	Every   time.Duration
}

func (t TaskInfo) quiet() bool { return t.Every < quietBelow }

// Service 持有运行记录的仓储与节流状态。节流状态只在内存里：重启后第一次成功一定会写，不需要持久化。
type Service struct {
	repo   *core.Repo
	logger *slog.Logger
	now    func() time.Time

	mu         sync.Mutex
	tasks      []TaskInfo
	lastOK     map[string]time.Time // 每个任务上一条写进库的 ok 的时间
	failed     map[string]bool      // 每个任务上一次运行是不是失败了
	unfinished map[int64]finish     // 插了 running、结束时没改成功的行，Flush 再写一次
}

// finish 是一次没写进库的结束状态。
type finish struct {
	task     string
	duration time.Duration
	status   string
	detail   string
}

// New 建服务；logger 为 nil 时用 slog.Default()。
func New(repo *core.Repo, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger, now: time.Now, lastOK: map[string]time.Time{}, failed: map[string]bool{}, unfinished: map[int64]finish{}}
}

// SetNow 替换时钟，只给测试用。
func (s *Service) SetNow(now func() time.Time) { s.now = now }

// SetTasks 登记本进程注册的内置任务（cmd/satchel 装配时与交给 scheduler 的是同一份），schedule list 按它列出。
func (s *Service) SetTasks(tasks []TaskInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append([]TaskInfo(nil), tasks...)
}

// Run 是一次正在进行的运行；Begin 返回、End 收尾。
type Run struct {
	task    TaskInfo
	id      int64 // 开始时插的 running 行；高频任务不插，为 0
	started time.Time
}

// Begin 记一次运行的开始：间隔不短于一小时的任务插一行 running；高频任务只记开始时间。写库失败只记日志。
func (s *Service) Begin(ctx context.Context, task TaskInfo) *Run {
	r := &Run{task: task, started: s.now()}
	if task.quiet() {
		return r
	}
	id, err := s.repo.Start(ctx, task.Name, r.started)
	if err != nil {
		s.logger.Error("写任务运行记录失败", "task", task.Name, "error", err)
		return r
	}
	r.id = id
	return r
}

// End 记一次运行的结束：runErr 为 nil 是 ok、detail 是任务给的一句结果；否则是 error、detail 是错误原因。
// 高频任务按节流规则决定写不写：error 每次写，error 之后的第一次成功写，其余成功离上一条写进库的 ok 不到一小时的不写。
// 插了 running 的行结束时没改成功（例如 SQLite 的写锁被别的事务占着超过时限），记下来等 Flush 再写一次。
func (s *Service) End(ctx context.Context, r *Run, detail string, runErr error) {
	dur := s.now().Sub(r.started)
	status := core.StatusOK
	if runErr != nil {
		status, detail = core.StatusError, runErr.Error()
	}
	name := r.task.Name
	if !s.shouldWrite(r, status) {
		return // 节流掉的成功：上一次也是成功（否则一定要写），状态不用改
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var err error
	if r.id > 0 {
		err = s.repo.Finish(ctx, r.id, dur, status, detail)
	} else {
		err = s.repo.Insert(ctx, core.Run{Task: name, StartedAt: r.started, DurationMs: dur.Milliseconds(), Status: status, Detail: detail})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 节流状态只在写库之后更新：没写进去的 ok 不算「上一条写下的 ok」，免得接下来一小时的成功都不写。
	s.failed[name] = status == core.StatusError
	if err != nil {
		s.logger.Error("写任务运行记录失败", "task", name, "status", status, "detail", detail, "error", err)
		if r.id > 0 {
			s.unfinished[r.id] = finish{task: name, duration: dur, status: status, detail: detail}
		}
		return
	}
	if status == core.StatusOK {
		s.lastOK[name] = r.started
	}
}

// shouldWrite 按节流规则判断这次结束要不要写，不改状态。插过 running 行的一定要写（把它改成结束状态）。
func (s *Service) shouldWrite(r *Run, status string) bool {
	if r.id > 0 || status == core.StatusError {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed[r.task.Name] {
		return true
	}
	last, ok := s.lastOK[r.task.Name]
	return !ok || r.started.Sub(last) >= quietBelow
}

// Flush 把结束时没写进库的 running 行再写一次（serve 停止时、关库之前调）；仍然失败的留给下次启动的 MarkInterrupted。
func (s *Service) Flush(ctx context.Context) {
	s.mu.Lock()
	pending := s.unfinished
	s.unfinished = map[int64]finish{}
	s.mu.Unlock()
	for id, f := range pending {
		if err := s.repo.Finish(ctx, id, f.duration, f.status, f.detail); err != nil {
			s.logger.Error("停止前补写任务运行记录仍然失败，下次启动时标为 error", "task", f.task, "id", id, "error", err)
		}
	}
}

// MarkInterrupted 把上次主控停止时还是 running 的行改成 error（serve 启动时调）。
func (s *Service) MarkInterrupted(ctx context.Context) error {
	n, err := s.repo.MarkInterrupted(ctx, interruptedDetail)
	if n > 0 {
		s.logger.Warn("上次主控停止时有任务还在运行，已把它们的记录标为 error", "count", n)
	}
	return err
}

// PruneRuns 删掉保留期以前的任务记录，返回删掉的条数（task_run_cleanup 任务调）。
func (s *Service) PruneRuns(ctx context.Context) (int, error) {
	return s.repo.DeleteBefore(ctx, s.now().Add(-RunRetention))
}

// Bindings 是 schedule * 两条命令的处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{"schedule list": s.list, "schedule runs list": s.runsList}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "内置任务与运行记录只对管理员开放")
	}
	return nil
}

func pageOf(inv *command.Invocation) (command.Page, error) {
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	return page, page.Normalize()
}

// TaskView 是 schedule list 的一项。
type TaskView struct {
	Name          string     `json:"name"`
	Summary       string     `json:"summary"`
	Interval      string     `json:"interval"`
	LastStartedAt *time.Time `json:"last_started_at"`
	LastStatus    string     `json:"last_status"`
}

// list 是 schedule list：本进程注册的任务按名字排序，带最近一条运行记录；任务不多，按偏移量分页。
func (s *Service) list(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page, err := pageOf(inv)
	if err != nil {
		return nil, err
	}
	offset, err := command.DecodeOffsetCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	latest, err := s.repo.Latest(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	tasks := append([]TaskInfo(nil), s.tasks...)
	s.mu.Unlock()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	res := &command.PageResult{Items: []any{}, Total: len(tasks)}
	for i := offset; i < len(tasks) && i < offset+page.Limit; i++ {
		t := tasks[i]
		v := TaskView{Name: t.Name, Summary: t.Summary, Interval: t.Every.String()}
		if run, ok := latest[t.Name]; ok {
			at := run.StartedAt
			v.LastStartedAt, v.LastStatus = &at, run.Status
		}
		res.Items = append(res.Items, v)
	}
	if offset+page.Limit < len(tasks) {
		res.NextCursor = command.EncodeOffsetCursor(offset + page.Limit)
	}
	return res, nil
}

// runsList 是 schedule runs list：按 id 倒序，keyset 翻页。
func (s *Service) runsList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page, err := pageOf(inv)
	if err != nil {
		return nil, err
	}
	beforeID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	f := core.Filter{Task: inv.String("task", ""), Status: inv.String("status", "")}
	switch f.Status {
	case "", core.StatusRunning, core.StatusOK, core.StatusError:
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "--status 不认识 %q", f.Status).WithNext("写 running、ok、error 之一")
	}
	total, err := s.repo.Count(ctx, f)
	if err != nil {
		return nil, err
	}
	// 多取一条，用来判断有没有下一页。
	rows, err := s.repo.List(ctx, f, page.Limit+1, beforeID)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].ID)
	}
	for _, r := range rows {
		res.Items = append(res.Items, r)
	}
	return res, nil
}
