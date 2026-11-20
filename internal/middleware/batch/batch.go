// Package batch 是横切层的批量类滚动窗口门控（node-config-tx）：
// 对登记在计数表中的普通写命令按 batch_op_counters 持久化表检查单次与滚动窗口阈值，
// 超过阈值时要求 batch 危险权限（有 confirm 口径的命令同时校验 confirm），执行成功后记账落库。
package batch

import (
	"context"
	"strconv"
	"time"

	"github.com/satchel/satchel/internal/command"
	corebatch "github.com/satchel/satchel/internal/core/batch"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// CountFunc 计算一次调用将影响的对象数量；返回 <= 0 时不触发计数与门控。
type CountFunc func(inv *command.Invocation) int64

// ConstOne 是单对象写命令的通用计数函数。
func ConstOne(_ *command.Invocation) int64 { return 1 }

// Wrap 给执行链套上批量滚动窗口门控：在 authz 之后、业务处理之前运行。
// 注意：apply 命令自身在事务内合并计算 delete / permission / batch 危险类与记账，不经本中间件重复计数。
func Wrap(repo *corebatch.Repo, settings *coresettings.Repo, table *command.Table, counted map[string]CountFunc, nowFn func() time.Time, next command.Runner) command.Runner {
	if nowFn == nil {
		nowFn = time.Now
	}
	return command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
		fn, ok := counted[inv.Name()]
		if !ok || repo == nil {
			return next.Run(ctx, inv)
		}
		count := fn(inv)
		if count <= 0 {
			return next.Run(ctx, inv)
		}
		id := v1.IdentityFrom(ctx)
		if id.IsAnonymous() {
			return next.Run(ctx, inv)
		}
		th := coresettings.DefaultBatchThresholds()
		if settings != nil {
			if loaded, err := settings.LoadBatchThresholds(ctx); err == nil {
				th = loaded
			}
		}
		now := nowFn().UTC()
		since := now.Add(-time.Duration(th.WindowMinutes) * time.Minute)
		used, err := repo.WindowUsed(ctx, id, since)
		if err != nil {
			return nil, err
		}
		if count > int64(th.Single) || used+count > int64(th.Window) {
			if !id.HasDanger(v1.DangerBatch) {
				return nil, v1.Newf(v1.CodeForbidden, "%s 触发批量类危险门控（单次 %d / 窗口已用 %d，上限 %d/%d），当前身份没有开 batch 危险类",
					inv.Name(), count, used, th.Single, th.Window).
					WithState("required_danger", string(v1.DangerBatch)).
					WithState("window_used", used).
					WithState("single_threshold", th.Single).
					WithState("window_threshold", th.Window).
					WithState("window_minutes", th.WindowMinutes)
			}
			if cmd, found := table.Lookup(inv.Name()); found && cmd.HasConfirm() {
				want := strconv.FormatInt(count, 10)
				if inv.Confirm != want {
					return nil, v1.Newf(v1.CodeConfirmRequired, "%s 触发批量类危险门控，请用 --confirm %s 确认", inv.Name(), want).
						WithState("kind", string(command.ConfirmCount)).
						WithState("expected", want).
						WithNext("重新执行并加上 --confirm " + want)
				}
			}
		}
		res, err := next.Run(ctx, inv)
		if err != nil {
			return nil, err
		}
		_ = repo.Record(context.WithoutCancel(ctx), id, count, inv.Name(), now)
		return res, nil
	})
}
