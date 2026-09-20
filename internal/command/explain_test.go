package command

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-mcp「explain 工具」与 master-cli「whoami、audit list 与 explain 子命令」：三种 target。
func TestExplainOverview(t *testing.T) {
	got, err := Explain(Catalog(), "")
	if err != nil {
		t.Fatal(err)
	}
	o := got.(Overview)
	names := map[string]bool{}
	for _, c := range o.Commands {
		names[c.Name] = true
	}
	for _, want := range []string{"whoami", "audit list", "explain", "version", "db migrate", "serve"} {
		if !names[want] {
			t.Errorf("总览缺命令 %s", want)
		}
	}
	if names["__verify"] {
		t.Error("隐藏命令不该出现在总览里")
	}
	if len(o.Kinds) != len(v1.Kinds()) {
		t.Errorf("总览应当列出全部 %d 个 kind，得到 %d", len(v1.Kinds()), len(o.Kinds))
	}
}

func TestExplainCommand(t *testing.T) {
	got, err := Explain(Catalog(), "audit list")
	if err != nil {
		t.Fatal(err)
	}
	info := got.(CommandInfo)
	if info.Class != ClassRead || info.Scope != "read" || !info.List || info.REST == nil || info.REST.Method != "GET" || info.REST.Path != "/api/v1/audit" {
		t.Fatalf("audit list 的说明不对：%+v", info)
	}
	if strings.Join(info.Reserved, ",") != "limit,cursor" {
		t.Fatalf("列表命令应当报出 limit 与 cursor：%v", info.Reserved)
	}
	human, _ := Explain(Catalog(), "account set-password")
	if strings.Join(human.(CommandInfo).Reserved, ",") != "verify-password,verify-code,verify-user" || !human.(CommandInfo).HumanOnly {
		t.Fatalf("人类专属命令应当报出 verify-* 保留 flag：%+v", human)
	}
	anon, _ := Explain(Catalog(), "setup status")
	if !anon.(CommandInfo).Anonymous {
		t.Fatal("setup status 应当报出不要身份")
	}
	flags := map[string]bool{}
	for _, f := range info.Flags {
		flags[f.Name] = true
	}
	for _, want := range []string{"actor", "command", "since"} {
		if !flags[want] {
			t.Errorf("缺 flag %s", want)
		}
	}
	// 各段多个空格也认。
	if _, err := Explain(Catalog(), "audit   list"); err != nil {
		t.Fatal(err)
	}
	local, _ := Explain(Catalog(), "version")
	if local.(CommandInfo).REST != nil || local.(CommandInfo).Scope != "" {
		t.Fatal("本地命令没有 REST 与 scope")
	}
}

func TestExplainKind(t *testing.T) {
	got, err := Explain(Catalog(), "Task")
	if err != nil {
		t.Fatal(err)
	}
	k := got.(KindExplain)
	if k.Class != v1.ClassAction {
		t.Fatalf("Task 应当是动作类：%+v", k)
	}
	fields := map[string]FieldInfo{}
	for _, f := range k.Fields {
		fields[f.Name] = f
	}
	if f := fields["title"]; f.Tier != "spec" || f.Type != "string" {
		t.Errorf("title 应当是 spec 的 string：%+v", f)
	}
	if f := fields["expires_at"]; f.Tier == "spec" || f.Tier == "" || !strings.Contains(f.Type, "time") {
		t.Errorf("expires_at 应当是 status 档的时间：%+v", f)
	}
	user, _ := Explain(Catalog(), "User")
	uf := map[string]FieldInfo{}
	for _, f := range user.(KindExplain).Fields {
		uf[f.Name] = f
	}
	if f := uf["password_hash"]; !f.Masked || f.Tier == "spec" || !strings.Contains(f.Type, "masked") {
		t.Errorf("password_hash 应当打码、不在 spec：%+v", f)
	}
	if f := uf["username"]; !f.Immutable || f.Tier != "spec" {
		t.Errorf("username 应当不可改的 spec：%+v", f)
	}
	if f := uf["is_active"]; !f.DefaultTrue {
		t.Errorf("is_active 应当省略即为真：%+v", f)
	}
	data, _ := json.Marshal(k)
	if !strings.Contains(string(data), `"name_fields"`) || !strings.Contains(string(data), `"fields"`) {
		t.Fatalf("JSON 形状不对：%s", data)
	}
}

func TestExplainNotFound(t *testing.T) {
	_, err := Explain(Catalog(), "nosuch")
	if v1.AsError(err).Code != v1.CodeNotFound {
		t.Fatalf("应当 not_found：%v", err)
	}
}
