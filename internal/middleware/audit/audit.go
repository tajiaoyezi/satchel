// Package audit 是横切层的留痕：每条经主控执行的命令在返回后写一条审计记录（master-audit-log「每条命令一条记录」）。
// 它套在执行链最外层，记的是身份检查之后的一切结果——被权限或 confirm 拒绝的也记；anonymous 与离线命令不记。
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/satchel/satchel/internal/command"
	svc "github.com/satchel/satchel/internal/service/audit"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Recorder 是写一条记录的入口，service/audit 实现它；测试用假的。
type Recorder interface {
	Record(ctx context.Context, e svc.Entry) error
}

// DigestLimit 是 args_digest 的最大字节数。
const DigestLimit = 4096

// Wrap 给执行链套上留痕：next 返回后写记录，写失败只记日志、结果照常返回。
func Wrap(rec Recorder, t *command.Table, logger *slog.Logger, next command.Runner) command.Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
		result, err := next.Run(ctx, inv)
		id := v1.IdentityFrom(ctx)
		cmd, known := t.Lookup(inv.Name())
		if id.IsAnonymous() || (known && (cmd.Offline || cmd.Class == command.ClassLocal)) {
			return result, err
		}
		entry := svc.Entry{
			At: time.Now().UTC(), Actor: id.Actor, ActorKind: id.ActorKind, TokenID: id.TokenID,
			Command: inv.Name(), ArgsDigest: Digest(cmd, inv), Result: "ok",
		}
		if err != nil {
			entry.Result = string(v1.AsError(err).Code)
		}
		if werr := rec.Record(ctx, entry); werr != nil {
			logger.Error("审计记录写入失败，命令结果照常返回",
				"error", werr, "command", entry.Command, "actor", entry.Actor, "actor_kind", entry.ActorKind,
				"args_digest", entry.ArgsDigest, "result", entry.Result, "at", entry.At)
		}
		return result, err
	})
}

// Digest 生成参数摘要：{"args":[…],"flags":{…}} 的紧凑 JSON，命令表里标 Secret 的 flag 值换成打码标记，
// 超过 DigestLimit 截断并以 … 结尾（在 UTF-8 边界上截）。cmd 为 nil 时没有 Secret 信息，全部原样。
func Digest(cmd *command.Command, inv *command.Invocation) string {
	flags := make(map[string]any, len(inv.Flags))
	for name, v := range inv.Flags {
		if cmd != nil {
			if f, ok := cmd.FlagByName(name); ok && f.Secret {
				flags[name] = v1.Redacted
				continue
			}
		}
		if d, ok := v.(time.Duration); ok {
			v = d.String()
		}
		flags[name] = v
	}
	args := inv.Args
	if args == nil {
		args = []string{}
	}
	raw, err := json.Marshal(struct {
		Args  []string       `json:"args"`
		Flags map[string]any `json:"flags"`
	}{args, flags})
	if err != nil {
		return `{"args":[],"flags":{}}`
	}
	return truncate(string(raw), DigestLimit)
}

const ellipsis = "…"

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
