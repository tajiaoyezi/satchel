package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-cli「settings 子命令」：object 类型的 flag 在命令行上是可重复的 字段=值，按第一个 = 拆，值可以含 =。
func TestObjectFlagFromPairs(t *testing.T) {
	cap := &capture{}
	opts := promptOptions(&fakePrompt{}, cap)
	_, stderr, code := runWith(t, opts, "settings", "set", "--set", "heartbeat_interval=45", "--set", "sub_info_expire_prefix=到期=2026", "--resource-version", "1", "--json")
	if code != 0 {
		t.Fatalf("退出码 %d：%s", code, stderr)
	}
	if cap.last == nil || cap.last.Name() != "settings set" {
		t.Fatalf("应当发出 settings set：%+v", cap.last)
	}
	obj, ok := cap.last.Flags["set"].(map[string]any)
	if !ok || obj["heartbeat_interval"] != "45" || obj["sub_info_expire_prefix"] != "到期=2026" || len(obj) != 2 {
		t.Fatalf("set 应当是字段名到字符串值的对象：%#v", cap.last.Flags["set"])
	}
	if cap.last.Flags["resource-version"] != 1 {
		t.Fatalf("resource-version 应当是 int 1：%#v", cap.last.Flags["resource-version"])
	}
	// 没给 --set 时对象不出现在 Flags 里。
	cap.last = nil
	if _, stderr, code := runWith(t, opts, "settings", "set", "--resource-version", "1", "--json"); code != 0 {
		t.Fatalf("退出码 %d：%s", code, stderr)
	}
	if _, present := cap.last.Flags["set"]; present {
		t.Fatalf("没给 --set 不该有 set：%#v", cap.last.Flags)
	}
}

// 重复字段与缺等号是 usage（退出码 2），不发请求。
func TestObjectFlagUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"settings", "set", "--set", "heartbeat_interval=45", "--set", "heartbeat_interval=50", "--resource-version", "1"}, "给了两次"},
		{[]string{"settings", "set", "--set", "heartbeat_interval", "--resource-version", "1"}, "字段=值"},
		{[]string{"settings", "set", "--set", "=45", "--resource-version", "1"}, "字段=值"},
	} {
		cap := &capture{}
		opts := promptOptions(&fakePrompt{}, cap)
		_, stderr, code := runWith(t, opts, append(tc.args, "--json")...)
		if code != v1.ExitUsage {
			t.Errorf("%v 应当退出码 2，得到 %d：%s", tc.args, code, stderr)
			continue
		}
		if e := decodeError(t, stderr); e.Code != v1.CodeUsage || !strings.Contains(e.Reason, tc.want) {
			t.Errorf("%v 的 reason 应当含 %q：%+v", tc.args, tc.want, e)
		}
		if cap.last != nil {
			t.Errorf("%v 不该发请求", tc.args)
		}
	}
}

// 资源信封的文本输出分 metadata、spec、status 三段。
func TestEnvelopeTextRendering(t *testing.T) {
	envelope := map[string]any{
		"apiVersion": "satchel/v1", "kind": "SystemSettings",
		"metadata": map[string]any{"id": 1, "resourceVersion": 3},
		"spec":     map[string]any{"heartbeat_interval": 45, "branding_site_title": "Satchel"},
		"status":   map[string]any{"master_url": "", "require_encryption": true},
	}
	opts := testOptions()
	opts.Remote = func(string) command.Runner {
		return command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) { return envelope, nil })
	}
	stdout, stderr, code := runWith(t, opts, "settings", "show")
	if code != 0 {
		t.Fatalf("退出码 %d：%s", code, stderr)
	}
	for _, want := range []string{"kind：SystemSettings", "[metadata]", "resourceVersion：3", "[spec]", "heartbeat_interval：45", "branding_site_title：Satchel", "[status]", "require_encryption：true"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("文本输出缺 %q：\n%s", want, stdout)
		}
	}
	if strings.Index(stdout, "[metadata]") > strings.Index(stdout, "[spec]") || strings.Index(stdout, "[spec]") > strings.Index(stdout, "[status]") {
		t.Errorf("三段顺序应当是 metadata、spec、status：\n%s", stdout)
	}
	// --json 时就是信封本身。
	stdout, _, _ = runWith(t, opts, "settings", "show", "--json")
	got := decodeJSONObject(t, stdout)
	if string(got["kind"]) != `"SystemSettings"` || len(got) != 5 {
		t.Fatalf("JSON 输出应当是五个键的信封：%s", stdout)
	}
}
