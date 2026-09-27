// Package jobs 是业务层的长任务（master-jobs）：受理时插一行 queued、在 goroutine 里跑、按结果改 done 或 failed；
// 启动时收尾上次没结束的；按保留期清理；job get 与 job list 两条命令。怎么等 job（CLI 跟到结束、MCP 最多等 60 秒）在投影层。
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/jobs"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

const (
	// Retention 是已结束的 job 的保留期（job_cleanup 任务按它清理）。
	Retention = 7 * 24 * time.Hour
	// OutputLimit 是 output 的上限，超过截断并把 output_truncated 置真。
	OutputLimit = 64 << 10
	// finishTimeout 是结束时写库的时限：serve 停止时工作用的 ctx 已经取消，结局仍要写进去。
	finishTimeout = 5 * time.Second
)

// Digest 生成参数摘要（与审计摘要同一套打码）；由装配根注入横切层 audit 的 Digest，业务层引用不了横切层。
type Digest func(cmd *command.Command, inv *command.Invocation) string

// Service 持有仓储与正在跑的 job。
type Service struct {
	repo   *core.Repo
	table  *command.Table
	digest Digest
	logger *slog.Logger
	now    func() time.Time

	base   context.Context // 工作用的 ctx 从它派生，Stop 时取消
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 建服务；logger 为 nil 时用 slog.Default()。
func New(repo *core.Repo, table *command.Table, digest Digest, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	base, cancel := context.WithCancel(context.Background())
	return &Service{repo: repo, table: table, digest: digest, logger: logger, now: time.Now, base: base, cancel: cancel}
}

// Start 受理一个长任务：插一行 queued，在 goroutine 里跑 run，按结果改 done 或 failed；立刻返回受理时的 job。
// run 用的 ctx 与请求无关（请求结束后照样跑），带调用者的身份，随 Stop 取消。done 非 nil 时在这个 job 结束后一定调一次——
// 包括连 running 都没改成、run 没跑的情况——调用方靠它释放自己占着的东西（例如「正在备份」）。受理失败时 done 不调。
func (s *Service) Start(ctx context.Context, inv *command.Invocation, run func(ctx context.Context) (any, error), done func()) (*core.Job, error) {
	id := v1.IdentityFrom(ctx)
	cmd, _ := s.table.Lookup(inv.Name())
	args, err := json.Marshal(struct {
		Actor     string          `json:"actor"`
		ActorKind v1.ActorKind    `json:"actor_kind"`
		Digest    json.RawMessage `json:"digest"`
	}{id.Actor, id.ActorKind, json.RawMessage(s.digest(cmd, inv))})
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码长任务的参数失败", err)
	}
	jobID, err := newJobID()
	if err != nil {
		return nil, err
	}
	job, err := s.repo.Insert(ctx, jobID, inv.Name(), args)
	if err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if done != nil {
			defer done()
		}
		s.execute(v1.WithIdentity(s.base, id), job, run)
	}()
	return job, nil
}

func newJobID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", v1.Wrap(v1.CodeInternal, "生成 job id 失败", err)
	}
	return "job-" + hex.EncodeToString(b), nil
}

func (s *Service) execute(ctx context.Context, job *core.Job, run func(context.Context) (any, error)) {
	if err := s.repo.SetRunning(ctx, job.ID, s.now()); err != nil {
		// 连 running 都没改成：不跑 run，把这一行直接写成 failed（不受停止取消），免得一直停在 queued。
		s.logger.Error("长任务改成 running 失败，按失败结束", "job_id", job.JobID, "error", err)
		raw, _ := json.Marshal(v1.Wrap(v1.CodeDatabase, "长任务没能开始", err))
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()
		if ferr := s.repo.Finish(wctx, job.ID, core.StatusQueued, core.StatusFailed, 1, string(raw), false, s.now()); ferr != nil {
			s.logger.Error("写长任务的结局失败", "job_id", job.JobID, "error", ferr)
		}
		return
	}
	result, runErr := safeRun(ctx, run)
	status, exit := core.StatusDone, int64(0)
	var raw []byte
	var err error
	if runErr != nil {
		status, exit = core.StatusFailed, 1
		raw, err = json.Marshal(v1.AsError(runErr))
	} else {
		raw, err = json.Marshal(result)
	}
	if err != nil {
		status, exit = core.StatusFailed, 1
		raw, _ = json.Marshal(v1.Wrap(v1.CodeInternal, "编码长任务的结果失败", err))
	}
	output, truncated := string(raw), false
	if len(output) > OutputLimit {
		output, truncated = output[:OutputLimit], true
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := s.repo.Finish(wctx, job.ID, core.StatusRunning, status, exit, output, truncated, s.now()); err != nil {
		s.logger.Error("写长任务的结局失败", "job_id", job.JobID, "status", status, "error", err)
	}
}

// safeRun 跑一次；panic 记成 internal 错误并写一条带调用栈的 error 日志。
func safeRun(ctx context.Context, run func(context.Context) (any, error)) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("长任务 panic", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			result, err = nil, v1.Newf(v1.CodeInternal, "长任务 panic：%v", p)
		}
	}()
	return run(ctx)
}

// Stop 取消正在跑的 job 并等它们返回；超过 ctx 的时限就不再等（没写上结局的由下次启动收尾）。
func (s *Service) Stop(ctx context.Context) {
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("停止时仍有长任务没跑完，不再等待", "error", ctx.Err())
	}
}

// MarkInterrupted 把上次主控停止时还没结束的 job 标为 failed（serve 开始监听之前调）。
func (s *Service) MarkInterrupted(ctx context.Context) error {
	raw, _ := json.Marshal(v1.New(v1.CodeInternal, "主控停止时这个长任务还在运行，结果没有写下").WithNext("重新发起这个命令"))
	n, err := s.repo.MarkInterrupted(ctx, string(raw), s.now())
	if n > 0 {
		s.logger.Warn("上次主控停止时有长任务没结束，已标为 failed", "count", n)
	}
	return err
}

// Prune 删掉保留期以前、已经结束的 job，返回删掉的条数（job_cleanup 任务调）。
func (s *Service) Prune(ctx context.Context) (int, error) {
	return s.repo.DeleteFinishedBefore(ctx, s.now().Add(-Retention))
}

// Bindings 是 job get 与 job list 的处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{"job get": s.get, "job list": s.list}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "长任务只对管理员开放")
	}
	return nil
}

func (s *Service) get(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	job, err := s.repo.Get(ctx, inv.Arg(0))
	if err == core.ErrNotFound {
		return nil, v1.Newf(v1.CodeNotFound, "没有长任务 %s", inv.Arg(0)).WithNext("用 job list 看有哪些")
	}
	return job, err
}

func (s *Service) list(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	beforeID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	status := inv.String("status", "")
	switch status {
	case "", core.StatusQueued, core.StatusRunning, core.StatusDone, core.StatusFailed, core.StatusUnknown:
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "--status 不认识 %q", status).WithNext("写 queued、running、done、failed、unknown 之一")
	}
	total, err := s.repo.Count(ctx, status)
	if err != nil {
		return nil, err
	}
	// 多取一条，用来判断有没有下一页。
	rows, err := s.repo.List(ctx, status, page.Limit+1, beforeID)
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
