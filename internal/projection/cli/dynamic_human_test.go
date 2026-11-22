package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/rest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestCLIDynamicHumanVerifyFlow(t *testing.T) {
	tbl := command.MustNew(
		&command.Command{
			Path: []string{"apply"}, Summary: "提交", Class: command.ClassConfig,
			DynamicDanger: true, DynamicHuman: true,
			Flags: []command.Flag{
				{Name: "plan", Type: command.TypeInt, Description: "计划编号"},
			},
		},
	)
	var gotInv *command.Invocation
	srv := httptest.NewServer(rest.NewHandler(tbl, command.RunnerFunc(func(_ context.Context, inv *command.Invocation) (any, error) {
		gotInv = inv
		return map[string]any{"ok": true}, nil
	}), nil))
	t.Cleanup(srv.Close)

	run := func(opts Options, args ...string) (string, string, int) {
		var out, errBuf bytes.Buffer
		opts.Table = tbl
		code := Execute(opts, args, &out, &errBuf)
		return out.String(), errBuf.String(), code
	}

	// 1. 远程场景：不带 --verify-user 时零终端提示，即使 Prompt 返回 ErrNoTerminal 也直接发出请求。
	gotInv = nil
	_, stderr, code := run(Options{
		Prompt: func(string) (string, error) { return "", ErrNoTerminal },
	}, "apply", "--plan", "7", "--server", srv.URL, "--token", "sat_remote", "--json")
	if code != 0 || gotInv == nil || gotInv.Verify != nil || gotInv.Int("plan", 0) != 7 {
		t.Fatalf("远程免人工验证应直接成功：code=%d stderr=%s inv=%+v", code, stderr, gotInv)
	}

	// 2. 命令行直接传 --verify-password 一律 usage 错误（退出码 2）。
	_, stderr, code = run(Options{}, "apply", "--plan", "7", "--verify-password", "pw", "--server", srv.URL, "--json")
	if code != v1.ExitUsage {
		t.Fatalf("--verify-password 应报 usage（2），得到 %d：%s", code, stderr)
	}

	// 3. 带 --verify-user 且走本机 unix socket 时从终端读取密码并填入 inv.Verify。
	dataDir, _ := startSocketMaster(t)
	// 用带 verify 的测试命令替换 startSocketMaster？直接复用本机 HTTP + 空 token 场景走 opts.ServerSide=false 与 unix socket：
	// 注意：startSocketMaster 挂的是真实 Catalog；这里验证无终端时带 --verify-user 返回 human_required（退出码 9）。
	_, stderr, code = run(Options{
		Prompt: func(string) (string, error) { return "", ErrNoTerminal },
	}, "apply", "--plan", "7", "--verify-user", "admin", "--data-dir", dataDir, "--json")
	if code != v1.ExitHumanRequired || !strings.Contains(stderr, `"human_required"`) {
		t.Fatalf("无终端带 --verify-user 应返回 human_required（9），得到 %d：%s", code, stderr)
	}
}
