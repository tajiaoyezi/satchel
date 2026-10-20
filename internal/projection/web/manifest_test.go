package web

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/satchel/satchel/internal/command"
)

// requiredPages 是 M1 必须交付的页面：m1-11 的六页与 m1-12 的八个运维页。
var requiredPages = []string{"login", "setup", "overview", "account", "tokens", "settings",
	"logs", "schedule", "jobs", "audit", "security", "backups", "database", "update"}

// testManifest 是一份能通过核对的小清单：登录页走会话入口，令牌页两条命令。
func testManifest() *Manifest {
	return &Manifest{
		Version: "v0.1.0",
		Pages: []Page{
			{ID: "login", Path: "/login", Title: "登录", Actions: []Action{{Label: "登录", Session: "login"}, {Label: "验证码", Session: "captcha"}}},
			{ID: "tokens", Path: "/tokens", Title: "API 令牌", Actions: []Action{{Label: "列出", Command: "token list"}, {Label: "吊销", Command: "token revoke"}}},
		},
		Commands: map[string]Route{
			"token list":   {Method: "GET", Path: "/api/v1/token"},
			"token revoke": {Method: "POST", Path: "/api/v1/token/revoke/{id}"},
		},
	}
}

func problemsOf(m *Manifest, required ...string) string {
	return strings.Join(CheckManifest(command.Catalog(), m, required), "\n")
}

// master-web「页面清单与命令表一致」的每条规则。
func TestCheckManifest(t *testing.T) {
	if p := problemsOf(testManifest(), "login", "tokens"); p != "" {
		t.Fatalf("合格的清单不该有问题：%s", p)
	}
	cases := map[string]struct {
		edit func(m *Manifest)
		want []string
	}{
		"写了不存在的命令": {func(m *Manifest) {
			m.Pages[1].Actions = append(m.Pages[1].Actions, Action{Label: "删除", Command: "token delete"})
		}, []string{"tokens", "token delete"}},
		"方法或路径对不上": {func(m *Manifest) { m.Commands["token revoke"] = Route{Method: "POST", Path: "/api/v1/token/revoke"} },
			[]string{"token revoke", "/api/v1/token/revoke/{id}"}},
		"本地命令": {func(m *Manifest) {
			m.Pages[1].Actions = append(m.Pages[1].Actions, Action{Label: "接入", Command: "mcp init"})
		}, []string{"mcp init", "本地命令"}},
		"隐藏命令":       {func(m *Manifest) { m.Commands["__verify"] = Route{Method: "POST", Path: "/api/v1/__verify"} }, []string{"__verify"}},
		"没有登记方法与路径":  {func(m *Manifest) { delete(m.Commands, "token list") }, []string{"token list", "没有登记方法与路径"}},
		"没有可见操作":     {func(m *Manifest) { m.Pages[1].Actions = nil }, []string{"tokens", "没有可见操作"}},
		"命令与会话入口都写了": {func(m *Manifest) { m.Pages[0].Actions[0].Command = "whoami" }, []string{"login", "恰好写命令或会话入口之一"}},
		"不认识的会话入口":   {func(m *Manifest) { m.Pages[0].Actions[1].Session = "register" }, []string{"register"}},
		"缺本站的页面":     {func(m *Manifest) { m.Pages = m.Pages[:1] }, []string{"缺页面 tokens"}},
	}
	for label, tc := range cases {
		m := testManifest()
		tc.edit(m)
		p := problemsOf(m, "login", "tokens")
		for _, want := range tc.want {
			if !strings.Contains(p, want) {
				t.Errorf("%s：问题清单里应当有 %q：%s", label, want, p)
			}
		}
	}
}

// 清单提供的网页入口：有登录页；命令出现在几页时取第一页。
func TestManifestEntries(t *testing.T) {
	m := testManifest()
	m.Pages = append(m.Pages, Page{ID: "account", Title: "账号", Actions: []Action{{Label: "我的令牌", Command: "token list"}}})
	e := m.Entries()
	if !e.Login || e.Pages["token revoke"] != "API 令牌" || e.Pages["token list"] != "API 令牌" || len(e.Pages) != 2 {
		t.Fatalf("网页入口：%+v", e)
	}
	var none *Manifest
	if e := none.Entries(); e.Login || len(e.Pages) != 0 {
		t.Fatalf("没有清单等于没有网页：%+v", e)
	}
}

// 清单的读取：没有文件是开发构建（nil、没有错误）；坏 JSON 报错。
func TestLoadManifest(t *testing.T) {
	if m, err := LoadManifest(fstest.MapFS{"index.html": {}}); m != nil || err != nil {
		t.Fatalf("没有清单：%v %v", m, err)
	}
	if _, err := LoadManifest(fstest.MapFS{ManifestFile: {Data: []byte("{bad")}}); err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
	m, err := LoadManifest(fstest.MapFS{ManifestFile: {Data: []byte(`{"version":"v0.1.0","pages":[],"commands":{}}`)}})
	if err != nil || m.Version != "v0.1.0" {
		t.Fatalf("读清单：%+v %v", m, err)
	}
}

// master-web「当前清单通过」「CI 里没有网页」：对编进二进制的真清单跑核对；没有清单时，设了
// SATCHEL_TEST_REQUIRE_WEB=1（CI）就失败，否则跳过（本地没跑 tools/web/build.sh）。
func TestManifestMatchesCommandTable(t *testing.T) {
	m, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		if os.Getenv("SATCHEL_TEST_REQUIRE_WEB") == "1" {
			t.Fatal("二进制里没有网页（没有 " + ManifestFile + "）：CI 必须先运行 tools/web/build.sh 编进网页")
		}
		t.Skip("没有编进网页（运行 tools/web/build.sh 后再测）")
	}
	if p := CheckManifest(command.Catalog(), m, requiredPages); len(p) != 0 {
		t.Fatalf("页面清单与命令表不一致：\n%s", strings.Join(p, "\n"))
	}
}
