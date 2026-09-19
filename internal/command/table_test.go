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
		"whoami": ClassRead, "audit list": ClassRead, "explain": ClassRead,
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
	if c, _ := table.Lookup("audit list"); !c.List {
		t.Error("audit list 应当是列表命令")
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
	if len(table.HumanOnly()) != 0 {
		t.Errorf("m1-01 的目录里不该有人类专属命令：%v", table.HumanOnly())
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
	b := Bindings{"whoami": nil, "audit list": nil, "explain": nil}
	if err := table.CheckBindings(b); err != nil {
		t.Fatalf("非本地命令都有绑定时应当通过：%v", err)
	}
	if err := table.CheckBindings(Bindings{"whoami": nil, "explain": nil}); err == nil || !strings.Contains(err.Error(), "audit list") {
		t.Fatalf("缺绑定应当点名命令：%v", err)
	}
	if err := table.CheckBindings(Bindings{"whoami": nil, "audit list": nil, "explain": nil, "nosuch": nil}); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("表外绑定应当被点名：%v", err)
	}
}
