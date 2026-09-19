package audit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	svc "github.com/satchel/satchel/internal/service/audit"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

type ctxRecorder struct {
	seen context.Context
}

func (c *ctxRecorder) Record(ctx context.Context, _ svc.Entry) error {
	c.seen = ctx
	return nil
}

// 命令跑完后客户端取消了请求：记录仍然要写（用不带取消的 ctx）。
func TestRecordSurvivesCancelledRequest(t *testing.T) {
	rec := &ctxRecorder{}
	ctx, cancel := context.WithCancel(v1.WithIdentity(context.Background(), v1.LocalAdmin("root")))
	r := Wrap(rec, table(t), nil, command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) {
		cancel() // 处理函数返回前请求就被取消了
		return map[string]any{"ok": true}, nil
	}))
	if _, err := r.Run(ctx, &command.Invocation{Path: []string{"whoami"}}); err != nil {
		t.Fatal(err)
	}
	if rec.seen == nil {
		t.Fatal("应当写记录")
	}
	if err := rec.seen.Err(); err != nil {
		t.Fatalf("写记录用的 ctx 不该带着取消：%v", err)
	}
	if v1.IdentityFrom(rec.seen).Actor != "root" {
		t.Fatal("写记录用的 ctx 应当仍带着身份")
	}
}

// 摘要含 confirm 与分页；写失败的日志带上整条记录的每个字段。
func TestDigestConfirmPageAndFailureLog(t *testing.T) {
	cmd, _ := table(t).Lookup("demo login")
	d := Digest(cmd, &command.Invocation{Path: []string{"demo", "login"}, Confirm: "alice", Page: &command.Page{Limit: 2, Cursor: "c1"}})
	if !strings.Contains(d, `"confirm":"alice"`) || !strings.Contains(d, `"page":{"limit":2,"cursor":"c1"}`) {
		t.Fatalf("摘要应当含 confirm 与分页：%s", d)
	}
	if strings.Contains(Digest(cmd, &command.Invocation{Path: []string{"demo", "login"}}), `"confirm"`) {
		t.Fatal("没给 confirm 时不该出现这个键")
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	failing := &memRecorder{err: errors.New("disk full")}
	tokenID := int64(7)
	ctx := v1.WithIdentity(context.Background(), v1.Identity{Actor: "t", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokenID, Scopes: v1.AllScopes})
	r := Wrap(failing, table(t), logger, next(map[string]any{"ok": true}, nil))
	if _, err := r.Run(ctx, &command.Invocation{Path: []string{"whoami"}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"level=ERROR", "disk full", "at=", "actor=t", "actor_kind=token", "token_id=7", "command=whoami", "args_digest=", "plan_id=<nil>", "result=ok"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("日志缺 %q：%s", want, logs.String())
		}
	}
}
