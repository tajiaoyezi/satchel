package mcp

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/middleware/audit"
	svc "github.com/satchel/satchel/internal/service/audit"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

type countRecorder struct{ results []string }

func (c *countRecorder) Record(_ context.Context, e svc.Entry) error {
	c.results = append(c.results, e.Result)
	return nil
}

// master-command-table「执行链兜住处理函数的 panic」：经 satchel_run 跑的命令 panic，工具结果是 internal 四字段错误，
// 同一个处理器之后的调用照常成功。经留痕那一层时审计照常写；执行链之外的 panic 由工具处理函数自己兜住。
// 没兜住的话，SDK 在单独的 goroutine 里调工具，panic 会让整个测试进程退出。
func TestPanicDoesNotKillServer(t *testing.T) {
	opts := testOptions(t, &command.Command{Path: []string{"demo", "boom"}, Summary: "s", Class: command.ClassRead})
	inner := opts.Remote("")
	boom := command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
		if inv.Name() == "demo boom" {
			panic("boom")
		}
		return inner.Run(ctx, inv)
	})
	rec := &countRecorder{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, runner := range map[string]command.Runner{
		"经留痕那一层": audit.Wrap(rec, opts.Table, quiet, boom),
		"执行链之外":  boom,
	} {
		t.Run(name, func(t *testing.T) {
			o := opts
			o.Remote = func(string) command.Runner { return runner }
			s := connect(t, o, admin())
			text, isErr := s.run([]string{"demo", "boom"}, "")
			if !isErr || decodeErr(t, text).Code != v1.CodeInternal {
				t.Fatalf("panic 应当转成 internal 四字段错误：%s", text)
			}
			if text, isErr := s.run([]string{"whoami"}, ""); isErr {
				t.Fatalf("之后的调用应当照常成功：%s", text)
			}
		})
	}
	if len(rec.results) != 2 || rec.results[0] != "internal" || rec.results[1] != "ok" {
		t.Fatalf("经留痕那一层时应当记 internal 与 ok 两条审计：%v", rec.results)
	}
}
