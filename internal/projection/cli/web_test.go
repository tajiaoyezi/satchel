package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-web「有网页入口的人类专属命令」「没有网页入口的人类专属命令」「没有网页的二进制」（CLI 本地的拒绝）：
// 带令牌做人类专属命令、读不到终端时，next 只在这个 CLI 编进的页面清单里有入口时才提网页。
func TestCLINextWebEntries(t *testing.T) {
	isolateHome(t)
	t.Cleanup(func() { command.SetWebEntries(command.WebEntries{}) })
	t.Setenv(EnvToken, "sat_good")
	refuse := func(args ...string) v1.Error {
		t.Helper()
		_, stderr, code := runWith(t, testOptions(), append(args, "--data-dir", shortTempDir(t), "--json")...)
		e := decodeError(t, stderr)
		if code != v1.ExitHumanRequired {
			t.Fatalf("%v 应当 human_required：%d %+v", args, code, e)
		}
		return e
	}
	if e := refuse("token", "revoke", "3"); strings.Contains(e.Next, "网页") {
		t.Fatalf("没有网页的二进制不提网页：%s", e.Next)
	}
	command.SetWebEntries(command.WebEntries{Login: true, Pages: map[string]string{"token revoke": "API 令牌", "account set-password": "账号"}})
	if e := refuse("token", "revoke", "3"); !strings.Contains(e.Next, "或在网页的「API 令牌」页操作") {
		t.Fatalf("有入口时写出页面名：%s", e.Next)
	}
	if e := refuse("security", "ban", "198.51.100.7"); strings.Contains(e.Next, "网页") {
		t.Fatalf("没有入口的不提网页：%s", e.Next)
	}
	os.Unsetenv(EnvToken)

	// 读不到终端：有入口时提那一页。
	opts := promptOptions(&fakePrompt{}, &capture{})
	_, stderr, _ := runWith(t, opts, "account", "set-password", "--json")
	if e := decodeError(t, stderr); e.Code != v1.CodeHumanRequired || !strings.Contains(e.Next, "或在网页的「账号」页操作") {
		t.Fatalf("没终端时有入口提那一页：%+v", e)
	}
	_, stderr, _ = runWith(t, opts, "setup", "init", "--username", "admin", "--json")
	if e := decodeError(t, stderr); e.Code != v1.CodeBadRequest || strings.Contains(e.Next, "网页") {
		t.Fatalf("清单里没有 setup init 时不提网页：%+v", e)
	}
}
