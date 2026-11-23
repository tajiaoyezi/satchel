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
		"whoami": ClassRead, "overview": ClassRead, "audit list": ClassRead, "explain": ClassRead, "setup status": ClassRead, "account show": ClassRead,
		"settings show": ClassRead, "settings snapshots list": ClassRead, "token list": ClassRead, "mcp status": ClassRead,
		"security events list": ClassRead, "security bans list": ClassRead,
		"logs list": ClassRead, "logs files list": ClassRead, "schedule list": ClassRead, "schedule runs list": ClassRead,
		"job get": ClassRead, "job list": ClassRead, "backup list": ClassRead,
		"database show": ClassRead, "database test": ClassAction, "database migrate": ClassAction,
		"update check": ClassRead, "update apply": ClassAction, "settings update-cdn set": ClassMasterSettings,
		"backup create": ClassAction, "backup upload": ClassAction, "backup download": ClassAction, "backup restore": ClassAction, "setup restore": ClassAction,
		"setup init": ClassAction, "account set-password": ClassAction, "account totp setup": ClassAction, "account totp confirm": ClassAction,
		"account totp disable": ClassAction, "account recovery-codes regenerate": ClassAction,
		"token create": ClassAction, "token update": ClassAction, "token revoke": ClassAction,
		"security ban": ClassAction, "security unban": ClassAction,
		"settings set": ClassMasterSettings, "settings rollback": ClassMasterSettings, "settings master-url set": ClassMasterSettings, "settings master-url migrate": ClassMasterSettings,
		"settings gates set": ClassMasterSettings, "settings turnstile test": ClassAction, "settings image upload": ClassAction, "settings batch-threshold set": ClassMasterSettings,
		"plan": ClassConfig, "apply": ClassConfig, "plans list": ClassRead, "plans get": ClassRead,
		"snapshot list": ClassRead, "snapshot get": ClassRead, "snapshot rollback": ClassConfig,
		"server add": ClassConfig, "server install-command": ClassRead, "server list": ClassRead, "server get": ClassRead,
		"server token show": ClassRead, "server token rotate": ClassAction, "server token revoke": ClassAction,
		"server limit set": ClassAction, "server limit clear": ClassAction, "server kick": ClassAction,
		"server connections": ClassRead, "server traffic": ClassRead, "server restart-kernel": ClassAction,
		"server update check": ClassRead, "server update apply": ClassAction, "server update batch": ClassAction,
		"server uninstall": ClassAction, "server switch-listen-port": ClassAction, "server probe-master-url": ClassRead,
		"server system-info": ClassRead, "server nics": ClassRead, "server speed": ClassRead,
		"server traffic-stats set": ClassConfig, "server logs list": ClassRead, "server logs files": ClassRead,
		"server config status": ClassRead, "server config files": ClassRead, "server config read": ClassRead, "server config test": ClassRead,
		"server service status": ClassRead, "server service control": ClassAction,
		"server nginx install": ClassAction, "server nginx uninstall": ClassAction,
		"server nginx servers": ClassRead, "server nginx clear-stream": ClassAction,
		"server steal status": ClassRead, "server steal plan": ClassConfig,
		"server scan": ClassAction, "server domain-latency": ClassAction,
		"server warp status": ClassRead, "server warp install": ClassAction, "server warp authorize": ClassAction,
		"server warp remove": ClassAction, "server warp outbound-plan": ClassConfig,
		"server diag": ClassRead, "server exec": ClassAction, "server evidence": ClassAction,
		"evidence list": ClassRead, "evidence get": ClassRead,
		"return-route list": ClassRead, "return-route get": ClassRead, "return-route test": ClassAction,
		"tunnel list": ClassRead, "tunnel plan": ClassConfig, "tunnel multiplex plan": ClassConfig, "tunnel delete-plan": ClassConfig,
		"tunnel chain list": ClassRead, "tunnel chain plan": ClassConfig, "tunnel chain delete-plan": ClassConfig,
		"website list": ClassRead, "website get": ClassRead, "website scan": ClassAction, "website validate": ClassAction, "website build": ClassRead, "website templates": ClassRead,
		"inbound list": ClassRead, "inbound get": ClassRead, "inbound protocols": ClassRead, "inbound examples": ClassRead,
		"inbound keygen": ClassRead, "inbound build": ClassRead, "inbound batch list": ClassRead, "inbound batch plan": ClassConfig,
		"outbound list": ClassRead, "outbound get": ClassRead, "outbound probe": ClassAction, "outbound landing plan": ClassConfig,
		"outbound batch list": ClassRead, "outbound batch plan": ClassConfig,
		"routing list": ClassRead, "routing get": ClassRead, "routing preset builtin": ClassRead,
		"routing preset list": ClassRead, "routing preset save": ClassAction, "routing preset delete": ClassAction,
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
	for _, name := range []string{"audit list", "settings snapshots list", "token list", "mcp status", "security events list", "security bans list", "logs list", "logs files list", "schedule list", "schedule runs list", "job list", "backup list", "plans list", "snapshot list", "server logs list", "server logs files", "server config files", "server nginx servers", "website list", "return-route list", "tunnel list", "tunnel chain list", "inbound list", "inbound batch list", "outbound list", "outbound batch list", "routing list", "routing preset list", "evidence list"} {
		if c, _ := table.Lookup(name); !c.List {
			t.Errorf("%s 应当是列表命令", name)
		}
	}
	// m1-07 的执行形状、人类专属与不要身份。
	for name, shape := range map[string]Shape{"backup create": ShapeJob, "update apply": ShapeJob, "server nginx install": ShapeJob, "server nginx uninstall": ShapeJob, "server warp install": ShapeJob, "server exec": ShapeJob, "return-route test": ShapeJob, "backup upload": ShapeUpload, "setup restore": ShapeUpload, "settings image upload": ShapeUpload, "backup download": ShapeDownload} {
		if c, _ := table.Lookup(name); c.Shape != shape {
			t.Errorf("%s 的执行形状应当是 %s，得到 %q", name, shape, c.Shape)
		}
	}
	if c, _ := table.Lookup("server exec"); c.Danger != v1.DangerExec || c.Confirm == nil || c.Confirm.Kind != ConfirmObject || c.Confirm.Arg != "name" || c.TrailingCommandFlag != "command" {
		t.Errorf("server exec 应当是 exec 危险类、confirm 取 name 且 TrailingCommandFlag=command：%+v", c)
	}
	// m1-09：升级与更新 CDN 开关是主控自身类，confirm 取位置参数。
	for name, arg := range map[string]string{"update apply": "version", "settings update-cdn set": "enabled"} {
		if c, _ := table.Lookup(name); c.Danger != v1.DangerMaster || c.Confirm == nil || c.Confirm.Kind != ConfirmObject || c.Confirm.Arg != arg {
			t.Errorf("%s 应当是主控自身类、confirm 取 %s：%+v", name, arg, c)
		}
	}
	if c, _ := table.Lookup("server restart-kernel"); c.Danger != v1.DangerRestart || c.Confirm == nil || c.Confirm.Kind != ConfirmObject || c.Confirm.Arg != "name" {
		t.Errorf("server restart-kernel 应当是重启类、confirm 取 name：%+v", c)
	}
	for _, name := range []string{"server token rotate", "server token revoke"} {
		if c, _ := table.Lookup(name); c.Danger != v1.DangerPermission || c.Confirm == nil || c.Confirm.Kind != ConfirmObject || c.Confirm.Arg != "name" {
			t.Errorf("%s 应当是权限类、confirm 取 name：%+v", name, c)
		}
	}
	for _, name := range []string{"backup download", "backup restore"} {
		if c, _ := table.Lookup(name); !c.HumanOnly {
			t.Errorf("%s 应当是人类专属", name)
		}
	}
	if c, _ := table.Lookup("setup restore"); !c.Anonymous {
		t.Error("setup restore 应当不要身份")
	}
	for _, name := range []string{"settings set", "settings gates set"} {
		if c, _ := table.Lookup(name); func() bool {
			f, ok := c.FlagByName("set")
			return ok && f.Type == TypeObject && f.Kind == "SystemSettings"
		}() != true {
			t.Errorf("%s 的 set 应当是 object 类型、kind 为 SystemSettings", name)
		}
	}
	if c, _ := table.Lookup("settings set"); func() bool { scope, ok := c.Scope(); return ok && scope == v1.ScopeOperate }() != true {
		t.Error("settings set 的 scope 应当是 operate")
	}
	for _, c := range table.All() {
		if _, ok := c.FlagByName("force"); ok && c.Name() != "apply" {
			t.Errorf("%s 不该登记 --force（仅 apply 允许）", c.Name())
		}
	}
	if c, _ := table.Lookup("apply"); !c.DynamicDanger || !c.HasConfirm() || !c.DynamicHuman || !c.HasVerify() {
		t.Errorf("apply 应当同时标 DynamicDanger 与 DynamicHuman 并支持 --confirm 与 verify-*：%+v", c)
	}
	for _, name := range []string{"server service control", "server nginx install", "server nginx uninstall", "server nginx clear-stream"} {
		if c, _ := table.Lookup(name); !c.DynamicHuman || !c.HasVerify() || c.HumanOnly {
			t.Errorf("%s 应当标 DynamicHuman 且不标 HumanOnly：%+v", name, c)
		}
	}
	if c, _ := table.Lookup("settings batch-threshold set"); c.Danger != v1.DangerPermission || c.Confirm == nil || c.Confirm.Kind != ConfirmObject || c.Confirm.Arg != "key" {
		t.Errorf("settings batch-threshold set 应当是权限类、confirm 取 key：%+v", c)
	}
	if c, _ := table.Lookup("routing preset delete"); c.Danger != v1.DangerDelete || c.Confirm == nil || c.Confirm.Kind != ConfirmObject || c.Confirm.Arg != "id" {
		t.Errorf("routing preset delete 应当是删除类、confirm 取 id：%+v", c)
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
	if strings.Join(table.HumanOnly(), ",") != "account recovery-codes regenerate,account set-password,account totp confirm,account totp disable,account totp setup,backup download,backup restore,database migrate,security ban,security unban,settings gates set,settings master-url migrate,settings master-url set,token create,token revoke,token update" {
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
		// 三种执行形状的约束（m1-07）。
		{"read 的长任务", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, Shape: ShapeJob}}, "action"},
		{"人类专属的上传", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Shape: ShapeUpload, HumanOnly: true,
			Flags: []Flag{{Name: UploadFlag, Type: TypeFile}}}}, "人类专属"},
		{"没有 output 的下载", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Shape: ShapeDownload}}, "--output"},
		{"上传没有 file", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Shape: ShapeUpload, Flags: []Flag{{Name: "path", Type: TypeFile}}}}, "--file"},
		{"不认识的形状", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Shape: "stream"}}, "不认识"},
		{"长任务撞名 no-wait", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Shape: ShapeJob, Flags: []Flag{{Name: NoWaitFlag, Type: TypeBool}}}}, "保留"},
		{"本地命令也不许登记 force", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, Flags: []Flag{{Name: "force", Type: TypeBool}}}}, "force"},
		{"动态危险类标在 read 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, DynamicDanger: true}}, "动态危险类"},
		{"动态危险类标在 local 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, DynamicDanger: true}}, "动态危险类"},
		{"静态与动态危险类同时登记", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassConfig, Danger: v1.DangerDelete, Confirm: &Confirm{Kind: ConfirmCount}, DynamicDanger: true}}, "动态危险类"},
		{"不要身份 + 动态危险类", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Anonymous: true, DynamicDanger: true}}, "动态危险类"},
		{"非 apply 不许登记 force", []*Command{{Path: []string{"plan"}, Summary: "s", Class: ClassConfig, DynamicDanger: true, Flags: []Flag{{Name: "force", Type: TypeBool}}}}, "force"},
		{"条件人类专属标在 read 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassRead, DynamicHuman: true}}, "条件人类专属"},
		{"条件人类专属标在 local 上", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassLocal, DynamicHuman: true}}, "条件人类专属"},
		{"同时标人类专属与条件人类专属", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, HumanOnly: true, DynamicHuman: true}}, "条件人类专属"},
		{"不要身份 + 条件人类专属", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Anonymous: true, DynamicHuman: true}}, "条件人类专属"},
		{"上传类 + 条件人类专属", []*Command{{Path: []string{"x"}, Summary: "s", Class: ClassAction, Shape: ShapeUpload, DynamicHuman: true, Flags: []Flag{{Name: UploadFlag, Type: TypeFile}}}}, "条件人类专属"},
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
	// apply（config + DynamicDanger + DynamicHuman）允许登记 bool 型 --force，HasConfirm 与 HasVerify 均为真。
	applyCmd := &Command{Path: []string{"apply"}, Summary: "s", Class: ClassConfig, DynamicDanger: true, DynamicHuman: true, Flags: []Flag{{Name: "force", Type: TypeBool}}}
	if _, err := New(applyCmd); err != nil {
		t.Fatalf("合法 apply 应当通过：%v", err)
	}
	if !applyCmd.HasConfirm() || !applyCmd.HasVerify() {
		t.Fatal("DynamicDanger + DynamicHuman 命令 HasConfirm 与 HasVerify 应当均为真")
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
