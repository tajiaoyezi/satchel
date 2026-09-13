package schema

import (
	"sort"
	"strings"
	"testing"
)

// m0-06 的修正：省略即为真的布尔列清单、username 外键的 ON UPDATE、Node 打码、去重索引不算已删行、
// 复合自然键与 metadata.name 的列。

// storage-schema「Booleans that are true when omitted」：18 列（mmwx 默认 1 的全部布尔列），一个不多一个不少。
var wantDefaultTrue = []string{
	"announcements.via_bot", "announcements.via_miniapp",
	"certificates.auto_renew",
	"custom_rules.enabled",
	"node_reachability.reachable",
	"nodes.enabled",
	"override_scripts.enabled",
	"package_assignment_subaccounts.is_active",
	"proxy_provider_configs.health_check_enabled", "proxy_provider_configs.health_check_lazy",
	"servers.include_in_traffic_stats", "servers.ipv6_enabled",
	"system_config.enable_miaomiaowu_features", "system_config.enable_short_link",
	"user_settings.keep_node_name", "user_settings.use_new_template_system",
	"user_subaccounts.is_active",
	"users.is_active",
}

func TestDefaultTrueColumnsArePinned(t *testing.T) {
	var got []string
	for _, tbl := range Default().Tables() {
		for _, c := range tbl.Columns {
			if !c.DefaultTrue {
				continue
			}
			got = append(got, tbl.Name+"."+c.Name)
			if c.Type != TypeBool || c.Default != "FALSE" {
				t.Errorf("%s.%s 标了省略即为真，却不是默认 FALSE 的布尔列：%+v", tbl.Name, c.Name, c)
			}
		}
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(wantDefaultTrue, "\n") {
		t.Errorf("省略即为真的列清单不符：\n得到 %v\n想要 %v", got, wantDefaultTrue)
	}
	if len(got) != 18 {
		t.Errorf("应当恰好 18 列，得到 %d", len(got))
	}
	// 附属表上的 5 列进不了 kind 清单，创建它们的代码要照标记显式置 true；其余 13 列在 kind 清单里。
	var side []string
	for _, tbl := range Default().Tables() {
		if tbl.IsKind() {
			continue
		}
		for _, c := range tbl.Columns {
			if c.DefaultTrue {
				side = append(side, tbl.Name+"."+c.Name)
			}
		}
	}
	sort.Strings(side)
	wantSide := "node_reachability.reachable,package_assignment_subaccounts.is_active,user_settings.keep_node_name,user_settings.use_new_template_system,user_subaccounts.is_active"
	if strings.Join(side, ",") != wantSide {
		t.Errorf("附属表上的省略即为真列应当是 %s，得到 %v", wantSide, side)
	}
	// Satchel 自己新增的布尔列没有「mmwx 默认 1」这回事，一列都不该标。
	for _, tbl := range Default().Tables() {
		if tbl.Origin != OriginSatchel {
			continue
		}
		for _, c := range tbl.Columns {
			if c.DefaultTrue {
				t.Errorf("Satchel 新增表的列 %s.%s 不该标省略即为真", tbl.Name, c.Name)
			}
		}
	}
}

func TestValidateRejectsDefaultTrueOutsideFalseBooleans(t *testing.T) {
	cases := map[string]Column{
		"文本列":          col("x", TypeText).def("''").defaultTrue(),
		"没有默认值的布尔列":    col("x", TypeBool).defaultTrue(),
		"默认 TRUE 的布尔列": col("x", TypeBool).def("TRUE").defaultTrue(),
	}
	for name, c := range cases {
		r := New()
		r.Add(Table{Name: "a", Kind: "A", KindClass: KindConfig, Columns: []Column{col("id", TypeSerial), c}})
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "省略即为真") {
			t.Errorf("%s 标省略即为真应当被拒，得到 %v", name, err)
		}
	}
}

func TestValidateRejectsBadOnUpdate(t *testing.T) {
	r := New()
	r.Add(Table{Name: "users", Kind: "User", KindClass: KindConfig, Columns: []Column{col("id", TypeSerial), col("username", TypeText)},
		Indexes: []Index{naturalKey("users_username_key", "username")}})
	r.Add(Table{Name: "a", GoName: "A", Columns: []Column{col("username", TypeText)}, PrimaryKey: []string{"username"},
		ForeignKeys: []ForeignKey{{Columns: []string{"username"}, RefTable: "users", RefColumns: []string{"username"}, OnUpdate: "BOGUS"}}})
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "ON UPDATE") {
		t.Fatalf("ON UPDATE BOGUS 应当被拒，得到 %v", err)
	}
}

// storage-schema「Cross-cluster foreign keys」：指向 users(username) 的外键都带 ON UPDATE CASCADE，指向 id 的都不带。
func TestUsernameForeignKeysCascadeOnUpdate(t *testing.T) {
	var toUsers []string
	for _, tbl := range Default().Tables() {
		for _, fk := range tbl.ForeignKeys {
			refUsername := fk.RefTable == "users" && strings.Join(fk.RefColumns, ",") == "username"
			switch {
			case refUsername && (fk.OnUpdate != "CASCADE" || fk.OnDelete != "CASCADE"):
				t.Errorf("%s.%v 指向 users.username，应当 ON UPDATE CASCADE ON DELETE CASCADE，得到 %+v", tbl.Name, fk.Columns, fk)
			case !refUsername && fk.OnUpdate != "":
				t.Errorf("%s.%v 指向整数 id，不该带 ON UPDATE，得到 %q", tbl.Name, fk.Columns, fk.OnUpdate)
			}
			if refUsername {
				toUsers = append(toUsers, tbl.Name)
			}
		}
	}
	if len(toUsers) != 17 {
		t.Errorf("指向 users.username 的外键应当有 17 条，得到 %d：%v", len(toUsers), toUsers)
	}
	for _, ddl := range []string{DDL(Default(), SQLite), DDL(Default(), Postgres)} {
		if n := strings.Count(ddl, "ON UPDATE CASCADE"); n != 17 {
			t.Errorf("DDL 里 ON UPDATE CASCADE 应当出现 17 次，得到 %d", n)
		}
		if n := strings.Count(ddl, "REFERENCES users (username) ON UPDATE CASCADE ON DELETE CASCADE"); n != 17 {
			t.Errorf("DDL 里指向 users 的外键写法不统一：%d 条合规", n)
		}
	}
}

// storage-schema「Core kind tables」：Node 的 URI 与解析结果打码，node_name 不打。
func TestNodeCredentialColumnsMasked(t *testing.T) {
	node := mustTable(t, "nodes")
	for _, name := range []string{"raw_url", "parsed_config", "clash_config", "routed_outbound_json", "routed_admin_credential"} {
		if c, _ := node.Column(name); !c.Masked {
			t.Errorf("nodes.%s 应当打码", name)
		}
	}
	for _, name := range []string{"node_name", "protocol", "tag"} {
		if c, _ := node.Column(name); c.Masked {
			t.Errorf("nodes.%s 不该打码", name)
		}
	}
}

// storage-schema「Task and alert dedup at the database level」：去重索引不算已软删除的行。
func TestDedupIndexesExcludeSoftDeletedRows(t *testing.T) {
	want := map[string]string{
		"tasks_dedup_key_active":  "status IN ('open', 'claimed') AND dedup_key <> '' AND deleted_at IS NULL",
		"alerts_dedup_key_active": "status <> 'resolved' AND deleted_at IS NULL",
	}
	for _, table := range []string{"tasks", "alerts"} {
		for _, ix := range mustTable(t, table).Indexes {
			if where, ok := want[ix.Name]; ok {
				if !ix.Unique || ix.NaturalKey || ix.Where != where {
					t.Errorf("索引 %s 应当是非自然键的部分唯一索引、谓词 %q，得到 %+v", ix.Name, where, ix)
				}
				delete(want, ix.Name)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("缺少去重索引：%v", want)
	}
}

// resource-model「Kind catalog with operation class」：metadata.name 的列来自自然键索引，按表里的列序。
func TestNameColumnsFollowNaturalKeys(t *testing.T) {
	want := map[string]string{
		"users": "username", "inbounds": "server_id,tag", "outbounds": "server_id,tag", "websites": "server_id,domain",
		"return_routes": "server_id,carrier", "certificates": "domain,server_id", "custom_rules": "name,type",
		"servers": "name", "package_assignments": "", "nodes": "", "tasks": "", "system_config": "", "api_tokens": "",
	}
	for table, cols := range want {
		var got []string
		for _, c := range nameColumns(mustTable(t, table)) {
			got = append(got, c.Name)
			if c.Class != ClassSpec {
				t.Errorf("%s 的名字列 %s 应当是 spec 档，得到 %s", table, c.Name, c.Class)
			}
		}
		if strings.Join(got, ",") != cols {
			t.Errorf("%s 的名字列应当是 [%s]，得到 %v", table, cols, got)
		}
	}
	// 复合自然键的四张表：撞上报 name_taken 点名两列，所以索引必须标自然键。
	for _, table := range []string{"inbounds", "outbounds", "websites", "return_routes"} {
		natural := 0
		for _, ix := range mustTable(t, table).Indexes {
			if ix.NaturalKey {
				natural++
				if len(ix.Columns) != 2 || ix.Columns[0] != "server_id" {
					t.Errorf("%s 的自然键应当是 (server_id, x)，得到 %v", table, ix.Columns)
				}
			}
		}
		if natural != 1 {
			t.Errorf("%s 应当恰好一条自然键索引，得到 %d", table, natural)
		}
	}
}
