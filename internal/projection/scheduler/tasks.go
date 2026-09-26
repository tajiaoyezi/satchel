package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/service/audit"
	"github.com/satchel/satchel/internal/service/auth"
	"github.com/satchel/satchel/internal/service/schedule"
	"github.com/satchel/satchel/internal/service/security"
)

// healthTimeout 是 db_health 单次的时限（照 mmwx）。
const healthTimeout = 30 * time.Second

// Deps 是本站内置任务要用的 service 与库。
type Deps struct {
	DB       *bun.DB
	Auth     *auth.Service
	Audit    *audit.Service
	Security *security.Service
	Schedule *schedule.Service
	Logger   *slog.Logger
}

// Tasks 是本站的内置任务（master-scheduler「本站的内置任务」）；db_checkpoint 只在 SQLite 下加入。
func Tasks(d Deps) []Task {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tasks := []Task{
		{Info: schedule.TaskInfo{Name: "session_cleanup", Summary: "删掉过期的会话", Every: time.Hour},
			Run: pruned(d.Auth.PruneSessions)},
		{Info: schedule.TaskInfo{Name: "audit_cleanup", Summary: "删掉 180 天以前的审计记录", Every: time.Hour},
			Run: pruned(d.Audit.Prune)},
		{Info: schedule.TaskInfo{Name: "security_event_cleanup", Summary: "删掉 90 天以前的安全事件", Every: time.Hour},
			Run: pruned(d.Security.PruneEvents)},
		{Info: schedule.TaskInfo{Name: "task_run_cleanup", Summary: "删掉 7 天以前的任务运行记录", Every: time.Hour},
			Run: pruned(d.Schedule.PruneRuns)},
		{Info: schedule.TaskInfo{Name: "ban_sweep", Summary: "清掉内存里已失效的封禁与过期的令牌猜测计数", Every: 10 * time.Minute},
			Run: swept(d.Security.Sweep)},
		{Info: schedule.TaskInfo{Name: "login_limit_sweep", Summary: "清掉内存里过期的登录限流计数", Every: 10 * time.Minute},
			Run: swept(d.Auth.Sweep)},
	}
	if db.DialectOf(d.DB) == schema.SQLite {
		tasks = append(tasks, Task{Info: schedule.TaskInfo{Name: "db_checkpoint", Summary: "把 SQLite 的 WAL 写回主库", Every: 5 * time.Minute},
			Run: func(ctx context.Context) (string, error) {
				busy, remaining, err := db.Checkpoint(ctx, d.DB)
				if err != nil {
					return "", err
				}
				if busy {
					return fmt.Sprintf("库忙，退回 PASSIVE，还剩 %d 帧没写回", remaining), nil
				}
				return "已写回", nil
			}})
	}
	tasks = append(tasks, Task{Info: schedule.TaskInfo{Name: "db_health", Summary: "检查数据库健康（SQLite 跑 quick_check，PostgreSQL 检查连通性）", Every: time.Minute},
		Run: healthCheck(func(ctx context.Context) error { return db.QuickCheck(ctx, d.DB) }, logger)})
	return tasks
}

// pruned 把一个「删掉 N 条」的清理函数包成任务。
func pruned(prune func(context.Context) (int, error)) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		n, err := prune(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("删掉 %d 条", n), nil
	}
}

// swept 把一个只清内存的函数包成任务。
func swept(sweep func()) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		sweep()
		return "已清理", nil
	}
}

// healthCheck 是 db_health：每次带 healthTimeout 的时限；从健康变成不健康时记一条 error 日志，恢复时记一条 info 日志。
// 主控停止打断的那一次不算检查结果：不记日志、不改状态（库并没有坏）。状态放在闭包里：同一个任务不会重叠，不需要锁。
func healthCheck(check func(context.Context) error, logger *slog.Logger) func(context.Context) (string, error) {
	healthy := true
	return func(parent context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(parent, healthTimeout)
		defer cancel()
		err := check(ctx)
		if err != nil && parent.Err() != nil {
			return "", err
		}
		switch {
		case err != nil && healthy:
			logger.Error("数据库健康检查失败", "error", err)
		case err == nil && !healthy:
			logger.Info("数据库健康检查已恢复")
		}
		healthy = err == nil
		if err != nil {
			return "", err
		}
		return "健康", nil
	}
}
