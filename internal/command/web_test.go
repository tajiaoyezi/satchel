package command

import (
	"strings"
	"testing"
)

// master-web「错误提示里的网页入口」：只有页面清单里有提供这条命令的页面时才提网页，并写出页面名。
func TestWebHints(t *testing.T) {
	t.Cleanup(func() { SetWebEntries(WebEntries{}) })
	if WebLogin() || WebHint("token revoke", "操作") != "" || strings.Contains(HumanOnlyNext("token revoke"), "网页") {
		t.Fatal("没设置时等于没有网页，不提网页")
	}
	SetWebEntries(WebEntries{Login: true, Pages: map[string]string{"token revoke": "API 令牌", "account set-password": "账号"}})
	if got := HumanOnlyNext("token revoke"); got != "在网页的「API 令牌」页用管理员账号操作；或在主控本机不带令牌执行 satchel token revoke --verify-user <管理员账号>" {
		t.Fatalf("有入口的人类专属命令：%s", got)
	}
	if got := HumanOnlyNext("security ban"); strings.Contains(got, "网页") || !strings.Contains(got, "satchel security ban --verify-user") {
		t.Fatalf("没有入口的只给主控本机的做法：%s", got)
	}
	if got := HumanOnlyNext("account set-password"); got != "由账号本人登录网页，在网页的「账号」页操作" {
		t.Fatalf("账号的命令只能由本人登录后做：%s", got)
	}
	SetWebEntries(WebEntries{})
	if got := HumanOnlyNext("account set-password"); strings.Contains(got, "网页") || strings.Contains(got, "satchel account") {
		t.Fatalf("没有网页时账号的命令也不给主控本机的做法：%s", got)
	}
}
