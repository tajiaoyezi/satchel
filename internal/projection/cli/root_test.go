package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/buildinfo"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// run 执行一次根命令，返回 stdout、stderr 与退出码。
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Execute(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// decodeJSONObject 把 stdout 解成 JSON 对象并确认带 apiVersion。
func decodeJSONObject(t *testing.T, stdout string) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("输出不是合法 JSON 对象：%v\n%s", err, stdout)
	}
	if string(got["apiVersion"]) != `"`+v1.APIVersion+`"` {
		t.Fatalf("JSON 输出应当带 apiVersion %s：%s", v1.APIVersion, stdout)
	}
	return got
}

// decodeError 把 stderr 解成四字段错误，确认恰好四个键。
func decodeError(t *testing.T, stderr string) v1.Error {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stderr), &keys); err != nil {
		t.Fatalf("stderr 不是合法 JSON：%v\n%s", err, stderr)
	}
	for _, k := range []string{"code", "reason", "state", "next"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("错误缺少 %s：%s", k, stderr)
		}
	}
	if len(keys) != 4 {
		t.Fatalf("错误应当恰好四个键：%s", stderr)
	}
	var e v1.Error
	if err := json.Unmarshal([]byte(stderr), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {}} {
		stdout, _, code := run(t, args...)
		if code != 0 {
			t.Fatalf("satchel %v 退出码 %d", args, code)
		}
		if !strings.Contains(stdout, "version") || !strings.Contains(stdout, "db") {
			t.Fatalf("satchel %v 的帮助里没有列出子命令：\n%s", args, stdout)
		}
	}
}

// 用法错误：未知子命令、未知 flag、多余参数 → usage、退出码 2。
func TestUsageErrorsExit2(t *testing.T) {
	cases := [][]string{{"nosuch"}, {"db", "status", "--bogus"}, {"db", "migrate", "extra"}, {"version", "extra"}}
	for _, args := range cases {
		stdout, stderr, code := run(t, args...)
		if code != v1.ExitUsage {
			t.Errorf("satchel %v 退出码应当是 2，得到 %d\nstdout=%s\nstderr=%s", args, code, stdout, stderr)
		}
		if !strings.Contains(stderr, "usage") || !strings.Contains(stderr, "用法错误") {
			t.Errorf("satchel %v 的 stderr 应当含 usage 与中文的用法错误说明：\n%s", args, stderr)
		}
		if stdout != "" {
			t.Errorf("用法错误不该往 stdout 写东西：%q", stdout)
		}
	}
	_, stderr, code := run(t, "nosuch", "--json")
	if code != v1.ExitUsage {
		t.Fatalf("退出码应当是 2，得到 %d", code)
	}
	e := decodeError(t, stderr)
	if e.Code != v1.CodeUsage || e.Reason != "用法错误：没有子命令 nosuch" {
		t.Fatalf("JSON 错误不对：%+v", e)
	}
	_, stderr, _ = run(t, "db", "status", "--bogus", "--json")
	if e := decodeError(t, stderr); e.Reason != "用法错误：没有参数 --bogus" {
		t.Fatalf("未知参数的 reason 不对：%+v", e)
	}
	// cobra 把多余的位置参数报成「unknown command」，reason 至少要点名那个片段。
	_, stderr, _ = run(t, "db", "migrate", "extra", "--json")
	if e := decodeError(t, stderr); !strings.HasPrefix(e.Reason, "用法错误：") || !strings.Contains(e.Reason, "extra") {
		t.Fatalf("多余参数的 reason 不对：%+v", e)
	}
}

func TestUsageReasonFallsBackToOriginal(t *testing.T) {
	if got := usageReason("something cobra invents later"); got != "用法错误：something cobra invents later" {
		t.Fatalf("认不出的文案应当原样带上：%q", got)
	}
	if got := usageReason(`invalid argument "abc" for "--port" flag: strconv.ParseInt`); got != "用法错误：参数 --port 的值 abc 不合法" {
		t.Fatalf("参数值不合法的文案不对：%q", got)
	}
}

func TestVersionText(t *testing.T) {
	stdout, _, code := run(t, "version")
	if code != 0 {
		t.Fatal(code)
	}
	info := buildinfo.Get()
	for _, want := range []string{info.Version, info.Commit, info.Date} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("version 输出缺少 %q：\n%s", want, stdout)
		}
	}
}

func TestVersionJSONFlag(t *testing.T) {
	stdout, _, code := run(t, "version", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	got := decodeJSONObject(t, stdout)
	for _, key := range []string{"version", "commit", "date"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("JSON 缺少字段 %q：%s", key, stdout)
		}
	}
	if string(got["version"]) != `"`+buildinfo.Version+`"` {
		t.Fatalf("version = %s，想要 %q", got["version"], buildinfo.Version)
	}
}

func TestVersionJSONEnv(t *testing.T) {
	t.Setenv("SATCHEL_OUTPUT", "json")
	stdout, _, code := run(t, "version")
	if code != 0 {
		t.Fatal(code)
	}
	decodeJSONObject(t, stdout)
}

func TestFlagOverridesEnv(t *testing.T) {
	t.Setenv("SATCHEL_OUTPUT", "json")
	stdout, _, code := run(t, "version", "--json=false")
	if code != 0 {
		t.Fatal(code)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("显式 --json=false 应当输出文本，得到：\n%s", stdout)
	}
}

func TestTextErrorShape(t *testing.T) {
	_, stderr, code := run(t, "nosuch")
	if code != v1.ExitUsage {
		t.Fatal(code)
	}
	if !strings.HasPrefix(stderr, "错误 usage：用法错误：没有子命令 nosuch") || !strings.Contains(stderr, "下一步：") || !strings.Contains(stderr, "原因：unknown command") {
		t.Fatalf("用法错误的文本应当有中文 reason、下一步与英文原文的原因行：\n%s", stderr)
	}
	sqliteOnly(t)
	t.Setenv("SATCHEL_DATABASE_DRIVER", "postgres")
	t.Setenv("SATCHEL_DATABASE_HOST", "127.0.0.1")
	t.Setenv("SATCHEL_DATABASE_PORT", "1")
	_, stderr, code = run(t, "db", "status", "--data-dir", t.TempDir())
	if code != v1.ExitFailure {
		t.Fatal(code)
	}
	if !strings.HasPrefix(stderr, "错误 database：") || !strings.Contains(stderr, "下一步：") || !strings.Contains(stderr, "原因：") {
		t.Fatalf("连不上库的文本错误应当有 code、reason、下一步与底层原因：\n%s", stderr)
	}
}
