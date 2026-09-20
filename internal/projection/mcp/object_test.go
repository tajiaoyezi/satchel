package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-settings「三个投影同一份结果」的 MCP 侧：--set 字段=值 到达执行链时是对象；人类专属的 master-url 被拒；show 放行。
func TestSettingsCommandsOverMCP(t *testing.T) {
	opts := testOptions(t)
	inner := opts.Remote("")
	var captured *command.Invocation
	opts.Remote = func(string) command.Runner {
		return command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
			captured = inv
			return inner.Run(ctx, inv)
		})
	}
	s := connect(t, opts, admin())
	text, isErr := s.run([]string{"settings", "set", "--set", "heartbeat_interval=45", "--set", "branding_site_title=S", "--resource-version", "1"}, "")
	if isErr || !strings.Contains(text, `"ran":"settings set"`) {
		t.Fatalf("settings set 应当到达执行链：%v %s", isErr, text)
	}
	obj, ok := captured.Flags["set"].(map[string]any)
	if !ok || obj["heartbeat_interval"] != "45" || obj["branding_site_title"] != "S" || captured.Flags["resource-version"] != 1 {
		t.Fatalf("解析出的调用对象不对：%#v", captured.Flags)
	}
	if text, isErr := s.run([]string{"settings", "set", "--set", "heartbeat_interval=45", "--set", "heartbeat_interval=50"}, ""); !isErr || decodeErr(t, text).Code != v1.CodeUsage {
		t.Fatalf("重复字段应当是 usage：%s", text)
	}
	captured = nil
	text, isErr = s.run([]string{"settings", "master-url", "set", "--url", "https://a.example", "--resource-version", "1"}, "")
	if !isErr || decodeErr(t, text).Code != v1.CodeHumanRequired || captured != nil {
		t.Fatalf("人类专属命令应当在解析器就被拒：%v %s", isErr, text)
	}
	if text, isErr := s.run([]string{"settings", "show"}, ""); isErr || !strings.Contains(text, `"ran":"settings show"`) {
		t.Fatalf("settings show 应当放行：%s", text)
	}
	if text, isErr := s.run([]string{"settings", "snapshots", "list", "--limit", "5"}, ""); isErr || !strings.Contains(text, `"ran":"settings snapshots list"`) {
		t.Fatalf("settings snapshots list 应当放行：%s", text)
	}
}
