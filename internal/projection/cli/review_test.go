package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-command-table「三个投影从表构造」：cobra 自带的 help / completion / __complete 不是表里的命令，树里不能有。
func TestNoBuiltinCobraCommands(t *testing.T) {
	stdout, _, code := run(t, "--help")
	if code != 0 {
		t.Fatal(code)
	}
	section := stdout[strings.Index(stdout, "Available Commands"):]
	if strings.Contains(section, "completion") || strings.Contains(section, "\n  help") {
		t.Fatalf("帮助里不该列出 cobra 自带的命令：\n%s", section)
	}
	for _, args := range [][]string{{"help"}, {"help", "serve"}, {"completion", "bash"}, {"completion"}, {"__complete", "whoami", ""}, {"__completeNoDesc", "whoami", ""}} {
		stdout, stderr, code := run(t, append(args, "--json")...)
		if code != v1.ExitUsage {
			t.Errorf("satchel %v 应当是用法错误，得到 %d：%s%s", args, code, stdout, stderr)
			continue
		}
		if stdout != "" {
			t.Errorf("satchel %v 不该往 stdout 写东西：%q", args, stdout)
		}
	}
	// 执行时 cobra 才挂上的 help 命令是隐藏的桩，不在帮助里也不在表里。
	root := NewRoot(testOptions())
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	for _, sub := range root.Commands() {
		if _, ok := sub.Annotations[annotationName]; ok || sub.HasSubCommands() { // 表里的命令与分组节点
			continue
		}
		if !sub.Hidden || sub.Name() == "help" {
			t.Errorf("树里出现了表外且不隐藏（或叫 help）的命令 %s", sub.Name())
		}
	}
	_ = cobra.ShellCompRequestCmd
}

// master-rest-api「列表分页」在 CLI 上的同一条：--limit 显式给 0 或越界要拒绝，不静默换默认值。
func TestLimitExplicitZeroRejected(t *testing.T) {
	for _, limit := range []string{"0", "-1", "501"} {
		_, stderr, code := run(t, "audit", "list", "--limit", limit, "--json", "--data-dir", t.TempDir())
		if code != v1.ExitFailure {
			t.Errorf("--limit %s 应当退出码 1，得到 %d：%s", limit, code, stderr)
			continue
		}
		if e := decodeError(t, stderr); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "limit") {
			t.Errorf("--limit %s 应当 bad_request 并点名 limit：%+v", limit, e)
		}
	}
}

// 回归 L-01：通用渲染对超过 2^53 的 int64 保持精度，同时保留列表总条数渲染。
func TestRenderPreservesLargeInt64PrecisionAndPageTotal(t *testing.T) {
	const big int64 = 9007199254740993 // 2^53 + 1
	var buf bytes.Buffer
	c := &command.Command{Path: []string{"demo", "show"}, Columns: []string{"id"}}
	if err := renderGeneric(&buf, c, map[string]any{"id": big}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "9007199254740993") {
		t.Fatalf("超过 2^53 的 int64 末位不应丢失精度：\n%s", buf.String())
	}

	buf.Reset()
	page := &command.PageResult{
		Items:      []any{map[string]any{"id": big}},
		Total:      42,
		NextCursor: "c2",
	}
	if err := renderGeneric(&buf, c, page); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "9007199254740993") || !strings.Contains(out, "共 42 条，还有下一页：--cursor c2") {
		t.Fatalf("分页输出丢失大整数精度或总条数：\n%s", out)
	}
}
