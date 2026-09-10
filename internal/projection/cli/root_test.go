package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/buildinfo"
)

// run 执行一次根命令，返回 stdout、stderr 与错误。
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {}} {
		stdout, _, err := run(t, args...)
		if err != nil {
			t.Fatalf("satchel %v 出错：%v", args, err)
		}
		if !strings.Contains(stdout, "version") {
			t.Fatalf("satchel %v 的帮助里没有列出 version 子命令：\n%s", args, stdout)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	_, stderr, err := run(t, "nosuch")
	if err == nil {
		t.Fatal("未知子命令应当返回错误")
	}
	if !strings.Contains(stderr, "nosuch") {
		t.Fatalf("stderr 应当指出未知的子命令名：\n%s", stderr)
	}
}

func TestVersionText(t *testing.T) {
	stdout, _, err := run(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	info := buildinfo.Get()
	for _, want := range []string{info.Version, info.Commit, info.Date} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("version 输出缺少 %q：\n%s", want, stdout)
		}
	}
}

func decodeVersion(t *testing.T, stdout string) map[string]string {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("输出不是合法 JSON：%v\n%s", err, stdout)
	}
	for _, key := range []string{"version", "commit", "date"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("JSON 缺少字段 %q：%v", key, got)
		}
	}
	return got
}

func TestVersionJSONFlag(t *testing.T) {
	stdout, _, err := run(t, "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeVersion(t, stdout)
	if got["version"] != buildinfo.Version {
		t.Fatalf("version = %q，想要 %q", got["version"], buildinfo.Version)
	}
}

func TestVersionJSONEnv(t *testing.T) {
	t.Setenv("SATCHEL_OUTPUT", "json")
	stdout, _, err := run(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	decodeVersion(t, stdout)
}

func TestFlagOverridesEnv(t *testing.T) {
	t.Setenv("SATCHEL_OUTPUT", "json")
	stdout, _, err := run(t, "version", "--json=false")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("显式 --json=false 应当输出文本，得到：\n%s", stdout)
	}
}
