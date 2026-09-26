package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/service/schedule"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// firstDelay 是 serve 监听成功后到每个任务第一次运行之间的等待。
const firstDelay = time.Minute

// Task 是一个内置任务：元数据加一次运行。Run 返回一句结果（写进 detail）或错误。
type Task struct {
	Info schedule.TaskInfo
	Run  func(ctx context.Context) (string, error)
}

// Recorder 记每次运行的开始与结束（service/schedule 实现）。
type Recorder interface {
	Begin(ctx context.Context, task schedule.TaskInfo) *schedule.Run
	End(ctx context.Context, run *schedule.Run, detail string, err error)
}

// Scheduler 为每个任务起一个 goroutine：先等 firstDelay，然后循环「跑一次，再等一个间隔」。跑是同步的，同一个任务不会重叠。
type Scheduler struct {
	tasks  []Task
	rec    Recorder
	logger *slog.Logger

	firstDelay time.Duration // 测试里调小

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 建调度器；logger 为 nil 时用 slog.Default()。
func New(tasks []Task, rec Recorder, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{tasks: tasks, rec: rec, logger: logger, firstDelay: firstDelay}
}

// Infos 返回全部任务的元数据（交给 service/schedule 的 SetTasks）。
func (s *Scheduler) Infos() []schedule.TaskInfo {
	out := make([]schedule.TaskInfo, len(s.tasks))
	for i, t := range s.tasks {
		out[i] = t.Info
	}
	return out
}

// Start 开始调度；ctx 取消或调 Stop 时不再开始新的运行。
func (s *Scheduler) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	// 任务以系统身份运行（master-scheduler「内置任务的运行」）。
	ctx = v1.WithIdentity(ctx, v1.Identity{Actor: "scheduler", ActorKind: v1.ActorSystem, Role: v1.RoleAdmin})
	for _, t := range s.tasks {
		s.wg.Add(1)
		go s.loop(ctx, t)
	}
}

// Stop 取消调度并等正在跑的任务返回；超过 ctx 的时限就不再等。
func (s *Scheduler) Stop(ctx context.Context) {
	if s.cancel == nil {
		return
	}
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("停止时仍有内置任务没跑完，不再等待", "error", ctx.Err())
	}
}

func (s *Scheduler) loop(ctx context.Context, t Task) {
	defer s.wg.Done()
	wait := s.firstDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if ctx.Err() != nil {
			return
		}
		s.runOnce(ctx, t)
		wait = t.Info.Every
	}
}

func (s *Scheduler) runOnce(ctx context.Context, t Task) {
	run := s.rec.Begin(ctx, t.Info)
	detail, err := s.safeRun(ctx, t)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		err = fmt.Errorf("主控停止，这次运行被中断：%w", err)
	}
	s.rec.End(ctx, run, detail, err)
}

// safeRun 跑一次任务；panic 记成错误（detail 以 panic: 开头）并写一条带调用栈的 error 日志，别的任务照常。
func (s *Scheduler) safeRun(ctx context.Context, t Task) (detail string, err error) {
	defer func() {
		if p := recover(); p != nil {
			s.logger.Error("内置任务 panic", "task", t.Info.Name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			detail, err = "", fmt.Errorf("panic: %v", p)
		}
	}()
	return t.Run(ctx)
}
