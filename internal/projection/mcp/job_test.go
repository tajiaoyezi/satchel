package mcp

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// jobOptions 给 backup create 与 job get 装上假的处理函数：job get 在第 finishAfter 次起返回 done（<0 表示永不结束）。
func jobOptions(t *testing.T, finishAfter int32, gets *int32) func(string) command.Runner {
	t.Helper()
	return func(string) command.Runner {
		return command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
			switch inv.Name() {
			case "backup create":
				return map[string]any{"job_id": "job-0123456789abcdef", "status": "queued"}, nil
			case "job get":
				n := atomic.AddInt32(gets, 1)
				if finishAfter >= 0 && n >= finishAfter {
					return map[string]any{"job_id": inv.Args[0], "status": "done"}, nil
				}
				return map[string]any{"job_id": inv.Args[0], "status": "running"}, nil
			}
			return nil, v1.Newf(v1.CodeNotFound, "没有 %s", inv.Name())
		})
	}
}

// master-jobs「三个投影怎么等 job」的 MCP 侧：等到结束、到时返回当时的 job（不标为错误）、--no-wait 不等。
func TestJobWaitOverMCP(t *testing.T) {
	var gets int32
	opts := testOptions(t)
	opts.Remote = jobOptions(t, 3, &gets)
	opts.JobPoll, opts.JobMaxWait = time.Millisecond, time.Second
	s := connect(t, opts, admin())
	if text, isErr := s.run([]string{"backup", "create"}, ""); isErr || !strings.Contains(text, `"done"`) || gets != 3 {
		t.Fatalf("应当等到 done：%v %s gets=%d", isErr, text, gets)
	}

	gets = 0
	opts.Remote = jobOptions(t, -1, &gets)
	opts.JobMaxWait = 30 * time.Millisecond
	s = connect(t, opts, admin())
	if text, isErr := s.run([]string{"backup", "create"}, ""); isErr || !strings.Contains(text, `"running"`) {
		t.Fatalf("到时应当返回当时的 job 且不标为错误：%v %s", isErr, text)
	}
	gets = 0
	if text, isErr := s.run([]string{"backup", "create", "--no-wait"}, ""); isErr || !strings.Contains(text, `"queued"`) || gets != 0 {
		t.Fatalf("--no-wait 不应当等：%v %s gets=%d", isErr, text, gets)
	}
}

// master-mcp「解析器约束与拒绝清单」：上传与下载类命令在 MCP 上不可用；人类专属的下载照旧是 human_required。
func TestFileCommandsRejected(t *testing.T) {
	s := connect(t, testOptions(t), admin())
	for _, c := range []struct {
		args []string
		code v1.Code
	}{
		{[]string{"backup", "upload"}, v1.CodeBadRequest},
		{[]string{"backup", "upload", "--file", "/tmp/b.zip"}, v1.CodeBadRequest},
		{[]string{"backup", "download", "b.zip", "--output", "/tmp/x"}, v1.CodeBadRequest},
		{[]string{"backup", "download", "b.zip"}, v1.CodeHumanRequired},
		{[]string{"backup", "restore", "b.zip"}, v1.CodeHumanRequired},
		{[]string{"setup", "restore"}, v1.CodeBadRequest},
	} {
		if text, isErr := s.run(c.args, ""); !isErr || decodeErr(t, text).Code != c.code {
			t.Errorf("%v 应当是 %s：%s", c.args, c.code, text)
		}
	}
}
