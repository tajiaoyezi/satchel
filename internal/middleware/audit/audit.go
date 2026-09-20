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
		// anonymous 被拒的请求不记（扫描器会刷满）；但标了不要身份的命令（setup *）执行了就要记。
		if (id.IsAnonymous() && !(known && cmd.Anonymous)) || (known && (cmd.Offline || cmd.Class == command.ClassLocal)) {
			return result, err
		}
		if id.IsAnonymous() {
			id = v1.Anonymous()
		}
		entry := svc.Entry{
			At: time.Now().UTC(), Actor: id.Actor, ActorKind: id.ActorKind, TokenID: id.TokenID,
			Command: inv.Name(), ArgsDigest: Digest(cmd, inv), Result: "ok",
		}
		if err != nil {
			entry.Result = string(v1.AsError(err).Code)
		}
		// 命令已经跑完，记录不能因为客户端取消了请求而丢：写入用不带取消的 ctx。
		if werr := rec.Record(context.WithoutCancel(ctx), entry); werr != nil {
			logger.Error("审计记录写入失败，命令结果照常返回",
				"error", werr, "at", entry.At, "actor", entry.Actor, "actor_kind", entry.ActorKind, "token_id", derefID(entry.TokenID),
				"command", entry.Command, "args_digest", entry.ArgsDigest, "plan_id", derefID(entry.PlanID), "result", entry.Result)
		}
		return result, err
	})
}

// derefID 把可空的 id 变成日志里可读的值：nil 打 <nil>。
func derefID(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

// Digest 生成参数摘要：{"args":[…],"flags":{…}} 的紧凑 JSON，带了 confirm 或分页时再加 "confirm" 与 "page" 两个键；
// 命令表里标 Secret 的 flag 值换成打码标记，超过 DigestLimit 截断并以 … 结尾（在 UTF-8 边界上截）。
// cmd 为 nil 时没有 Secret 信息，全部原样。
func Digest(cmd *command.Command, inv *command.Invocation) string {
	flags := make(map[string]any, len(inv.Flags))
	for name, v := range inv.Flags {
		if cmd != nil {
			if f, ok := cmd.FlagByName(name); ok && f.Masked() {
				flags[name] = v1.Redacted
				continue
			}
			if f, ok := cmd.FlagByName(name); ok && f.Type == command.TypeObject {
				if obj, ok := v.(map[string]any); ok {
					flags[name] = maskObject(f.Kind, obj)
					continue
				}
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
	var page *command.Page
	if inv.Page != nil {
		p := *inv.Page
		page = &p
	}
	raw, err := json.Marshal(struct {
		Args    []string       `json:"args"`
		Flags   map[string]any `json:"flags"`
		Confirm string         `json:"confirm,omitempty"`
		Page    *command.Page  `json:"page,omitempty"`
	}{args, flags, inv.Confirm, page})
	if err != nil {
		return `{"args":[],"flags":{}}`
	}
	return truncate(string(raw), DigestLimit)
}

// maskObject 给 object 类型 flag 的值按所属 kind 的打码字段集合逐键打码（master-audit-log「参数摘要」），其余键照记。
func maskObject(kind v1.Kind, obj map[string]any) map[string]any {
	masked := map[string]bool{}
	if info, ok := v1.Lookup(kind); ok {
		for _, f := range info.MaskedFields {
			masked[f] = true
		}
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if masked[k] {
			out[k] = v1.Redacted
			continue
		}
		out[k] = v
	}
	return out
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
