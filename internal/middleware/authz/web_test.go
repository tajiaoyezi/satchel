package authz

import (
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// withWeb 设置一份测试用的网页入口，测试结束时还原成没有网页。
func withWeb(t *testing.T, e command.WebEntries) {
	t.Helper()
	command.SetWebEntries(e)
	t.Cleanup(func() { command.SetWebEntries(command.WebEntries{}) })
}

// master-web「错误提示里的网页入口」：authz 的四处 next 只在网页真有入口时提网页，并写出页面名。
func TestNextWebEntries(t *testing.T) {
	c := &calls{}
	r := Wrap(testTable(t), nil, c.runner())
	tok := token(v1.AllScopes, nil)
	nexts := func() (unauth, human, danger, invalid string) {
		unauth = run(t, r, v1.Anonymous(), []string{"whoami"}, nil, "").Next
		human = run(t, r, tok, []string{"demo", "lock"}, nil, "").Next
		danger = run(t, r, tok, []string{"demo", "remove"}, []string{"alice"}, "alice").Next
		invalid = InvalidCredential().Next
		return
	}

	// 没有网页的二进制：一律不提网页。
	a, b, d, i := nexts()
	for _, n := range []string{a, b, d, i} {
		if strings.Contains(n, "网页") {
			t.Fatalf("没有网页时不该提网页：%s", n)
		}
	}

	withWeb(t, command.WebEntries{Login: true, Pages: map[string]string{"demo lock": "演示", "token update": "API 令牌", "token create": "API 令牌"}})
	unauth, human, danger, invalid := nexts()
	if !strings.HasPrefix(unauth, "在网页上登录；") {
		t.Fatalf("有登录页时提网页登录：%s", unauth)
	}
	if !strings.Contains(human, "在网页的「演示」页用管理员账号操作") || !strings.Contains(human, "satchel demo lock --verify-user <管理员账号>") {
		t.Fatalf("有入口的人类专属命令：%s", human)
	}
	if !strings.Contains(danger, "在网页的「API 令牌」页给这把令牌打开 delete 类") || !strings.Contains(danger, "--danger 要写上这把令牌要开的全部危险类") {
		t.Fatalf("危险类没开：%s", danger)
	}
	if !strings.Contains(invalid, "或在网页的「API 令牌」页签发") {
		t.Fatalf("无效令牌：%s", invalid)
	}

	// 页面清单里没有提供它的页面：只给主控本机的做法。
	withWeb(t, command.WebEntries{Login: true, Pages: map[string]string{"token update": "API 令牌"}})
	if _, human, _, invalid := nexts(); strings.Contains(human, "网页") || strings.Contains(invalid, "网页") {
		t.Fatalf("没有入口的不提网页：%s / %s", human, invalid)
	}
	// 普通用户的会话没有危险类：请管理员来做，不说给令牌开。
	user := v1.Identity{Actor: "carol", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}}
	if n := run(t, r, user, []string{"demo", "remove"}, []string{"alice"}, "alice").Next; n != "这一类操作要由管理员来做" {
		t.Fatalf("普通用户的会话：%s", n)
	}
}
