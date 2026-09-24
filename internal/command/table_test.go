package command

import (
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func read(path ...string) *Command {
	return &Command{Path: path, Summary: "测试用", Class: ClassRead}
}

func action(path ...string) *Command {
	return &Command{Path: path, Summary: "测试用", Class: ClassAction}
}

// master-command-table「命令记录的字段」：当前目录通过校验，类别与 scope 对应。
func TestCatalog(t *testing.T) {
	table := Catalog()
	want := map[string]Class{
		"version": ClassLocal, "db migrate": ClassLocal, "db status": ClassLocal, "db unlock": ClassLocal, "__verify": ClassLocal, "serve": ClassLocal,
		"admin reset-password": ClassLocal, "login": ClassLocal, "logout": ClassLocal, "mcp stdio": ClassLocal, "mcp init": ClassLocal,
		"whoami": ClassRead, "audit list": ClassRead, "explain": ClassRead, "setup status": ClassRead, "account show": ClassRead,
		"settings show": ClassRead, "settings snapshots list": ClassRead, "token list": ClassRead, "mcp status": ClassRead,
		"setup init": ClassAction, "account set-password": ClassAction, "account totp setup": ClassAction, "account totp confirm": ClassAction,
		"account totp disable": ClassAction, "account recovery-codes regenerate": ClassAction,
		"token create": ClassAction, "token update": ClassAction, "token revoke": ClassAction,
		"settings set": ClassMasterSettings, "settings rollback": ClassMasterSettings, "settings master-url set": ClassMasterSettings,
	}
	if len(table.All()) != len(want) {
		t.Fatalf("目录里应当恰好 %d 条命令，得到 %v", len(want), table.Names())
	}
	for name, class := range want {
		c, ok := table.Lookup(name)
		if !ok {
			t.Fatalf("目录里没有 %s", name)
		}
		if c.Class != class {
			t.Errorf("%s 的类别应当是 %s，得到 %s", name, class, c.Class)
		}
	}
	for _, name := range []string{"audit list", "settings snapshots list", "token list", "mcp status"} {
		if c, _ := table.Lookup(name); !c.List {
			t.Errorf("%s 应当是列表命令", name)
		}
	}
	if c, _ := table.Lookup("settings set"); func() bool {
		f, ok := c.FlagByName("set")
		return ok && f.Type == TypeObject && f.Kind == "SystemSettings"
	}() != true {
		t.Error("settings set 的 set 应当是 object 类型、kind 为 SystemSettings")
	}
	if c, _ := table.Lookup("settings set"); func() bool { scope, ok := c.Scope(); return ok && scope == v1.ScopeOperate }() != true {
		t.Error("settings set 的 scope 应当是 operate")
	}
	for _, c := range table.All() {
		if _, ok := c.FlagByName("force"); ok {
			t.Errorf("%s 不该登记 --force（force 归权限类，随 M2 的 apply 做门）", c.Name())
		}
	}
	if c, _ := table.Lookup("mcp init"); func() bool {
		_, u := c.FlagByName(VerifyUserFlag)
		_, v := c.FlagByName(VerifyCodeFlag)
		return u && v
	}() != true {
		t.Error("mcp init 要自己登记 --verify-user 与 --verify-code")
	}
	if c, _ := table.Lookup("explain"); !c.Offline || c.RequiredArgs() != 0 {
		t.Error("explain 应当是离线命令、target 可选")
	}
	if c, _ := table.Lookup("__verify"); !c.Hidden {
		t.Error("__verify 应当隐藏")
	}
	for _, name := range []string{"whoami", "audit list"} {
		c, _ := table.Lookup(name)
		if scope, ok := c.Scope(); !ok || scope != v1.ScopeRead {
			t.Errorf("%s 的 scope 应当是 read", name)
		}
	}
	if c, _ := table.Lookup("version"); func() bool { _, ok := c.Scope(); return ok }() {
		t.Error("version 是本地命令，不该有 scope")
	}
	if strings.Join(table.HumanOnly(), ",") != "account recovery-codes regenerate,account set-password,account totp confirm,account totp disable,account totp setup,settings master-url set,token create,token revoke,token update" {
		t.Errorf("人类专属命令清单不对：%v", table.HumanOnly())
	}
	for _, name := range []string{"setup status", "setup init"} {
		if c, _ := table.Lookup(name); !c.Anonymous {
			t.Errorf("%s 应当标不要身份", name)
		}
	}
	for _, name := range []string{"whoami", "account show", "audit list"} {
		if c, _ := table.Lookup(name); c.Anonymous {
			t.Errorf("%s 不该标不要身份", name)
		}
	}
	if c, _ := table.Lookup("setup init"); func() bool { f, _ := c.FlagByName("password"); return f.Type == TypePassword && f.Masked() }() != true {
		t.Error("setup init 的 password 应当是 password 类型且打码")
	}
}

// master-command-table「全表规则」：每条规则一个反向用例。
func TestTableRules(t *testing.T) {
	cases := []struct {
		name string
		cmds []*Command
		want string
	}{
		{"重复路径", []*Command{read("whoami"), read("whoami")}, "重复"},
		{"本地命令带危险类", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmCount}}}, "本地命令"},
		{"本地命令带 REST", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, REST: &REST{Method: "GET", Path: "/api/v1/x"}}}, "本地命令"},
		{"危险类没有 confirm", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Danger: v1.DangerRestart}}, "confirm"},
		{"没危险类却有 confirm", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Confirm: &Confirm{Kind: ConfirmCount}}}, "confirm"},
		{"confirm 指向不存在的参数", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmObject, Arg: "name"}}}, "name"},
		{"列表命令不是 read", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, List: true}}, "列表"},
		{"人类专属标在 read 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, HumanOnly: true}}, "人类专属"},
		{"flag 与位置参数重名", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, Args: []Arg{{Name: "a"}}, Flags: []Flag{{Name: "a", Type: TypeString}}}}, "重复"},
		{"flag 撞保留名", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, Flags: []Flag{{Name: "json", Type: TypeBool}}}}, "保留"},
		{"路径互为前缀", []*Command{read("audit", "list"), read("audit")}, "前缀"},
		{"类别不认识", []*Command{{Path: []string{"x"}, Summary: "s", Class: "weird"}}, "类别"},
		{"路径段不合法", []*Command{{Path: []string{"Bad"}, Summary: "s", Class: ClassRead}}, "路径段"},
		{"路径太长", []*Command{{Path: []string{"a", "b", "c", "d"}, Summary: "s", Class: ClassRead}}, "一到三段"},
		{"没有说明", []*Command{{Path: []string{"x"}, Class: ClassRead}}, "说明"},
		{"flag 类型不认识", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, Flags: []Flag{{Name: "a", Type: "blob"}}}}, "类型"},
		{"危险类不认识", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Danger: "nuke", Confirm: &Confirm{Kind: ConfirmCount}}}, "危险类"},
		{"必填参数排在可选之后", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, Args: []Arg{{Name: "a", Optional: true}, {Name: "b"}}}}, "可选参数之后"},
		{"REST 方法不对", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, REST: &REST{Method: "PUT", Path: "/api/v1/x"}}}, "GET 或 POST"},
		{"REST 路径没前缀", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, REST: &REST{Method: "GET", Path: "/x"}}}, "/api/v1/"},
		{"不要身份 + 人类专属", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Anonymous: true, HumanOnly: true}}, "不要身份"},
		{"不要身份 + 本地", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, Anonymous: true}}, "read 或 action"},
		{"不要身份 + 危险类", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Anonymous: true, Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmCount}}}, "不要身份"},
		{"password 登记在 read 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, Flags: []Flag{{Name: "p", Type: TypePassword}}}}, "password"},
		{"object 登记在 read 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, Flags: []Flag{{Name: "set", Type: TypeObject, Kind: "SystemSettings"}}}}, "set"},
		{"object 登记在 local 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, Flags: []Flag{{Name: "set", Type: TypeObject, Kind: "SystemSettings"}}}}, "object"},
		{"object 的 kind 不存在", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Flags: []Flag{{Name: "set", Type: TypeObject, Kind: "NoSuchKind"}}}}, "NoSuchKind"},
		{"object 没有 kind", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Flags: []Flag{{Name: "set", Type: TypeObject}}}}, "kind"},
		{"非 object 带 kind", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Flags: []Flag{{Name: "n", Type: TypeString, Kind: "Task"}}}}, "不是 object"},
		{"verify-* 撞保留名", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Flags: []Flag{{Name: "verify-password", Type: TypePassword}}}}, "保留"},
		{"非本地命令自己登记 verify-user", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Flags: []Flag{{Name: VerifyUserFlag, Type: TypeString}}}}, "保留"},
		{"本地命令也不能登记 verify-password", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, Flags: []Flag{{Name: VerifyPasswordFlag, Type: TypeString}}}}, "保留"},
		{"不许登记 force", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassMasterSettings, Flags: []Flag{{Name: "force", Type: TypeBool}}}}, "权限类"},
		{"本地命令也不许登记 force", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, Flags: []Flag{{Name: "force", Type: TypeBool}}}}, "force"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cmds...)
			if err == nil {
				t.Fatal("应当校验失败")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应当含 %q：%v", tc.want, err)
			}
		})
	}
	if _, err := New(read("audit", "list"), read("audit", "show"), action("demo", "remove")); err != nil {
		t.Fatalf("合法的表不该报错：%v", err)
	}
	// 本地命令可以自己登记 --confirm、--verify-user、--verify-code（admin reset-password、mcp init）。
	if _, err := New(&Command{Path: []string{"x"}, Summary: "s", Class: ClassLocal, Flags: []Flag{
		{Name: "confirm", Type: TypeString}, {Name: VerifyUserFlag, Type: TypeString}, {Name: VerifyCodeFlag, Type: TypeString}}}); err != nil {
		t.Fatalf("本地命令自己登记这三个保留名应当通过：%v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustNew 对不合法的表应当 panic")
		}
	}()
	MustNew(read("a"), read("a"))
}

// master-rest-api「路径与方法从命令表推出」：默认映射三例与显式覆盖。
func TestRouteDefaults(t *testing.T) {
	table := Catalog()
	cases := map[string]REST{
		"whoami":     {Method: "GET", Path: "/api/v1/whoami"},
		"audit list": {Method: "GET", Path: "/api/v1/audit"},
		"explain":    {Method: "GET", Path: "/api/v1/explain/{target}"},
	}
	for name, want := range cases {
		c, _ := table.Lookup(name)
		if got := c.Route(); got != want {
			t.Errorf("%s 的路由应当是 %+v，得到 %+v", name, want, got)
		}
	}
	act := &Command{Path: []string{"demo", "remove"}, Summary: "s", Class: ClassAction, Args: []Arg{{Name: "name"}}}
	if got := act.Route(); got != (REST{Method: "POST", Path: "/api/v1/demo/remove/{name}"}) {
		t.Errorf("动作类默认 POST 且位置参数进路径，得到 %+v", got)
	}
	over := &Command{Path: []string{"demo", "x"}, Summary: "s", Class: ClassRead, REST: &REST{Method: "GET", Path: "/api/v1/custom"}}
	if got := over.Route(); got.Path != "/api/v1/custom" {
		t.Errorf("显式覆盖应当生效，得到 %+v", got)
	}
}

func TestCheckBindings(t *testing.T) {
	table := Catalog()
	b := Bindings{}
	for _, c := range table.Remote() {
		b[c.Name()] = nil
	}
	if err := table.CheckBindings(b); err != nil {
		t.Fatalf("非本地命令都有绑定时应当通过：%v", err)
	}
	delete(b, "audit list")
	if err := table.CheckBindings(b); err == nil || !strings.Contains(err.Error(), "audit list") {
		t.Fatalf("缺绑定应当点名命令：%v", err)
	}
	b["audit list"] = nil
	b["nosuch"] = nil
	if err := table.CheckBindings(b); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("表外绑定应当被点名：%v", err)
	}
}
