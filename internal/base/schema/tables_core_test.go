package schema

import (
	"sort"
	"strings"
	"testing"
)

func kindTable(t *testing.T, kind string) *Table {
	t.Helper()
	for _, tbl := range Default().Tables() {
		if tbl.Kind == kind {
			return tbl
		}
	}
	t.Fatalf("注册表里没有 kind %s", kind)
	return nil
}

func columnsOfClass(tbl *Table, class Class) []string {
	var out []string
	for _, c := range tbl.Columns {
		if c.Class == class {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

func hasColumn(tbl *Table, name string) bool {
	_, ok := tbl.Column(name)
	return ok
}

// 全部 32 个 kind 的类别（storage-schema「Agent-native tables」「Core kind tables」「Carry-over tables」），一个不落。
var wantKindClasses = map[string]KindClass{
	// agent-native
	"Task": KindAction, "Alert": KindAction, "Job": KindAction, "Plan": KindAction,
	"EvidencePackage": KindSystem, "NotifyDelivery": KindSystem, "AuditLog": KindSystem, "ConfigSnapshot": KindSystem,
	"AutomationRule": KindConfig, "NotifyChannel": KindConfig,
	// 核心五簇
	"User": KindConfig, "ApiToken": KindAction, "Package": KindConfig, "PackageAssignment": KindConfig,
	"Server": KindConfig, "Inbound": KindConfig, "Outbound": KindConfig, "RoutingRule": KindConfig,
	"Website": KindConfig, "ReturnRoute": KindConfig, "Node": KindConfig,
	// 照抄七簇
	"SubscribeFile": KindConfig, "SubscriptionLink": KindConfig, "Template": KindConfig, "CustomRule": KindConfig,
	"ExternalSubscription": KindConfig, "ProxyProviderConfig": KindConfig, "OverrideScript": KindConfig,
	"Certificate": KindConfig, "DnsProvider": KindConfig, "Announcement": KindConfig,
	"SystemSettings": KindMasterSettings,
}

func TestCoreKindsAndClasses(t *testing.T) {
	seen := map[string]bool{}
	for _, tbl := range Default().Tables() {
		if !tbl.IsKind() {
			continue
		}
		seen[tbl.Kind] = true
		class, ok := wantKindClasses[tbl.Kind]
		if !ok {
			t.Errorf("kind %s 不在类别清单里", tbl.Kind)
			continue
		}
		if tbl.KindClass != class {
			t.Errorf("kind %s 的类别应当是 %s，得到 %s", tbl.Kind, class, tbl.KindClass)
		}
	}
	for kind := range wantKindClasses {
		if !seen[kind] {
			t.Errorf("清单里的 kind %s 不存在", kind)
		}
	}
	if len(seen) != 32 {
		t.Fatalf("kind 应当恰好 32 个，得到 %d", len(seen))
	}
	for _, name := range []string{"xray_servers", "server_xray_config_snapshots", "remote_servers", "user_package_assignments", "user_api_tokens", "license", "licenses", "migrate_mmw"} {
		if _, ok := Default().Table(name); ok {
			t.Errorf("表 %s 不该存在", name)
		}
	}
}

func TestUsersClassification(t *testing.T) {
	users := kindTable(t, "User")
	spec := columnsOfClass(users, ClassSpec)
	for _, want := range []string{"email", "traffic_limit_override", "role", "username"} {
		if !contains(spec, want) {
			t.Errorf("User 的 spec 应当含 %s，得到 %v", want, spec)
		}
	}
	for _, notSpec := range []string{"password_hash", "is_active", "is_over_limit", "telegram_id", "totp_secret"} {
		if contains(spec, notSpec) {
			t.Errorf("User 的 spec 不该含 %s", notSpec)
		}
	}
	for _, legacy := range []string{"package_id", "package_start_date", "package_end_date", "is_reset", "reset_day"} {
		if hasColumn(users, legacy) {
			t.Errorf("users 不该有遗留直挂字段 %s", legacy)
		}
	}
	if got := columnsOfClass(users, ClassHuman); strings.Join(got, ",") != "password_hash,recovery_codes,totp_enabled,totp_secret" {
		t.Errorf("User 的人类专属列不对：%v", got)
	}
	if got := columnsOfClass(users, ClassAction); strings.Join(got, ",") != "is_active,telegram_bound_at,telegram_id,telegram_username" {
		t.Errorf("User 的动作专属列不对：%v", got)
	}
	for _, masked := range []string{"password_hash", "totp_secret", "recovery_codes"} {
		if c, _ := users.Column(masked); !c.Masked {
			t.Errorf("users.%s 应当打码", masked)
		}
	}
	for _, ix := range users.Indexes {
		if ix.Name == "users_username_key" && !ix.NaturalKey {
			t.Error("username 应当是自然键")
		}
	}
}

func TestSideTablesAreNotKinds(t *testing.T) {
	sides := map[string]string{
		"user_tokens": "username", "sessions": "token_hash", "user_settings": "username", "user_subaccounts": "id",
		"user_outbounds": "id", "user_routed_outbound_actions": "id", "package_assignment_inbound_configs": "id",
		"package_assignment_subaccounts": "id", "package_user_node_traffic_baselines": "username,package_id,node_id",
		"package_node_traffic_suspensions": "username,package_id,node_id,kind", "user_inbound_configs": "id",
		"batch_inbounds": "id", "batch_outbounds": "id", "node_reachability": "node_id",
	}
	for name, pk := range sides {
		tbl, ok := Default().Table(name)
		if !ok {
			t.Errorf("附属表 %s 不存在", name)
			continue
		}
		if tbl.IsKind() || tbl.HasVersion() || hasColumn(tbl, "deleted_at") {
			t.Errorf("附属表 %s 不该是 kind、不该有 resource_version 或 deleted_at", name)
		}
		if got := strings.Join(tbl.PKColumns(), ","); got != pk {
			t.Errorf("附属表 %s 的主键应当是 %s，得到 %s", name, pk, got)
		}
	}
	if tbl, _ := Default().Table("user_routed_outbound_actions"); !tbl.AppendOnly {
		t.Error("user_routed_outbound_actions 应当是 append-only")
	}
	if c, _ := kindTable(t, "User").Column("username"); c.Class != ClassSpec {
		t.Error("username 应当是 spec 列")
	}
	if c, _ := mustTable(t, "user_tokens").Column("token"); !c.Masked {
		t.Error("user_tokens.token 应当打码")
	}
}

func mustTable(t *testing.T, name string) *Table {
	t.Helper()
	tbl, ok := Default().Table(name)
	if !ok {
		t.Fatalf("表 %s 不存在", name)
	}
	return tbl
}

func TestPackageAssignmentClassification(t *testing.T) {
	pa := kindTable(t, "PackageAssignment")
	if got := columnsOfClass(pa, ClassStatus); strings.Join(got, ",") != "last_reset_at,over_limit_enforced,status,traffic_warned_80" {
		t.Errorf("PackageAssignment 的 status 列应当恰好四个，得到 %v", got)
	}
	if hasColumn(pa, "legacy_source") {
		t.Error("package_assignments 不该有 legacy_source")
	}
	for _, ix := range pa.Indexes {
		if ix.Name == "package_assignments_short_code_key" && (ix.NaturalKey || !ix.Unique) {
			t.Error("short_code 应当唯一但不是自然键")
		}
	}
	pkg := kindTable(t, "Package")
	if len(columnsOfClass(pkg, ClassStatus))+len(columnsOfClass(pkg, ClassAction))+len(columnsOfClass(pkg, ClassHuman)) != 0 {
		t.Error("Package 的列应当全部是 spec 或 meta")
	}
}

func TestServerClassification(t *testing.T) {
	srv := kindTable(t, "Server")
	spec := columnsOfClass(srv, ClassSpec)
	for _, notSpec := range []string{"token", "agent_token", "pull_token", "token_expires_at", "status", "last_heartbeat", "applied_hash"} {
		if contains(spec, notSpec) {
			t.Errorf("Server 的 spec 不该含 %s", notSpec)
		}
	}
	for _, want := range []string{"name", "connection_mode", "traffic_calibration", "ddns_record_name", "core_log_level", "core_dns", "core_stats_enabled"} {
		if !contains(spec, want) {
			t.Errorf("Server 的 spec 应当含 %s", want)
		}
	}
	if got := columnsOfClass(srv, ClassAction); strings.Join(got, ",") != "agent_token,agent_token_expires_at,last_agent_token_refresh,last_token_refresh,pull_token,token,token_expires_at" {
		t.Errorf("Server 的动作专属列不对：%v", got)
	}
	status := columnsOfClass(srv, ClassStatus)
	for _, want := range []string{"rotation_pending", "last_rotated_at", "revoke_pending", "applied_hash", "applied_generation", "first_apply_eligible", "traffic_reset_baseline", "core_running", "core_version", "same_host_as_master"} {
		if !contains(status, want) {
			t.Errorf("Server 的 status 应当含 %s", want)
		}
	}
	for _, c := range srv.Columns {
		if strings.HasPrefix(c.Name, "xray") {
			t.Errorf("servers 不该有 xray 前缀的列：%s", c.Name)
		}
	}
	for _, masked := range []string{"token", "agent_token", "pull_token"} {
		if c, _ := srv.Column(masked); !c.Masked {
			t.Errorf("servers.%s 应当打码", masked)
		}
	}
	for _, ix := range srv.Indexes {
		if ix.Name == "servers_token_key" && (ix.NaturalKey || !ix.Unique) {
			t.Error("token 应当唯一但不是自然键")
		}
	}
	if c, _ := srv.Column("connection_mode"); c.Default != "'auto'" || strings.Join(c.Enum, ",") != "auto,websocket,http,pull" {
		t.Errorf("connection_mode 应当默认 auto 且只认四个值，得到 %+v", c)
	}
}

func TestServerOwnedKinds(t *testing.T) {
	for kind, statusWant := range map[string]string{
		"Inbound": "", "Outbound": "last_probe_latency_ms,last_probe_ok,last_probed_at", "RoutingRule": "",
		"Website": "conf_path,managed,scanned", "ReturnRoute": "entry_asn,entry_ip,reason,region,route_type,tested_at",
	} {
		tbl := kindTable(t, kind)
		if tbl.KindClass != KindConfig {
			t.Errorf("%s 应当是配置类", kind)
		}
		if got := strings.Join(columnsOfClass(tbl, ClassStatus), ","); got != statusWant {
			t.Errorf("%s 的 status 列应当是 %q，得到 %q", kind, statusWant, got)
		}
		fkOK := false
		for _, fk := range tbl.ForeignKeys {
			if fk.RefTable == "servers" && fk.OnDelete == "CASCADE" && strings.Join(fk.Columns, ",") == "server_id" {
				fkOK = true
			}
		}
		if !fkOK {
			t.Errorf("%s 应当有指向 servers 的级联外键", kind)
		}
	}
	if c, _ := kindTable(t, "Outbound").Column("settings"); !c.Masked {
		t.Error("outbounds.settings 含凭据，应当打码")
	}
	for _, col := range []string{"tls", "settings"} {
		if c, _ := kindTable(t, "Inbound").Column(col); !c.Masked {
			t.Errorf("inbounds.%s 会放私钥或 PSK，应当打码", col)
		}
	}
}

func TestNodeClassification(t *testing.T) {
	node := kindTable(t, "Node")
	spec := columnsOfClass(node, ClassSpec)
	for _, want := range []string{"raw_url", "relay_group_name", "enabled", "routed_admin_credential"} {
		if !contains(spec, want) {
			t.Errorf("Node 的 spec 应当含 %s", want)
		}
	}
	if got := columnsOfClass(node, ClassAction); strings.Join(got, ",") != "detach_reason,detached" {
		t.Errorf("Node 的动作专属列应当是 detached 与 detach_reason，得到 %v", got)
	}
	for _, col := range []string{"routed_admin_credential", "routed_outbound_json"} {
		if c, _ := node.Column(col); !c.Masked {
			t.Errorf("nodes.%s 应当打码", col)
		}
	}
	serverFK := false
	for _, fk := range node.ForeignKeys {
		if fk.RefTable == "servers" && fk.OnDelete == "SET NULL" {
			serverFK = true
		}
	}
	if !serverFK {
		t.Error("nodes 应当有指向 servers 的 SET NULL 外键（同步来的节点的权威归属）")
	}
	reach := mustTable(t, "node_reachability")
	if len(reach.ForeignKeys) != 1 || reach.ForeignKeys[0].RefTable != "nodes" || reach.ForeignKeys[0].OnDelete != "CASCADE" {
		t.Error("node_reachability 应当级联指向 nodes")
	}
}

func TestApiTokenHasNoSpec(t *testing.T) {
	tok := kindTable(t, "ApiToken")
	if tok.KindClass != KindAction {
		t.Error("ApiToken 应当是动作类")
	}
	if got := columnsOfClass(tok, ClassSpec); len(got) != 0 {
		t.Errorf("ApiToken 不该有 spec 列，得到 %v", got)
	}
	if got := columnsOfClass(tok, ClassHuman); strings.Join(got, ",") != "expires_at,name,owner,preset,revoked,revoked_at,runtime,scopes,token_hash" {
		t.Errorf("ApiToken 的人类专属列不对：%v", got)
	}
	if got := columnsOfClass(tok, ClassStatus); strings.Join(got, ",") != "last_used_at" {
		t.Errorf("ApiToken 的 status 列应当只有 last_used_at，得到 %v", got)
	}
	if c, _ := tok.Column("token_hash"); !c.Masked {
		t.Error("api_tokens.token_hash 应当打码")
	}
}

func TestRestoredForeignKeys(t *testing.T) {
	want := map[string]string{
		"evidence_packages": "server_id→servers SET NULL",
		"jobs":              "server_id→servers SET NULL",
		"tasks":             "claimed_by→api_tokens SET NULL",
	}
	for table, spec := range want {
		parts := strings.SplitN(spec, "→", 2)
		ref := strings.SplitN(parts[1], " ", 2)
		if !hasForeignKey(mustTable(t, table), parts[0], ref[0], ref[1]) {
			t.Errorf("表 %s 应当有外键 %s", table, spec)
		}
	}
	for _, appendOnly := range []string{"audit_logs", "batch_op_counters", "user_routed_outbound_actions"} {
		if len(mustTable(t, appendOnly).ForeignKeys) != 0 {
			t.Errorf("append-only 表 %s 不该有外键", appendOnly)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestUsernameIsImmutableAndSideTableUniques(t *testing.T) {
	if c, _ := kindTable(t, "User").Column("username"); !c.Immutable {
		t.Error("users.username 应当标 Immutable")
	}
	for _, tbl := range Default().Tables() {
		for _, c := range tbl.Columns {
			if c.Immutable && tbl.Kind != "User" {
				t.Errorf("只有 username 不可改，%s.%s 不该标 Immutable", tbl.Name, c.Name)
			}
		}
	}
	want := map[string]string{
		"user_tokens_user_short_code_key":        "user_short_code <> ''",
		"user_tokens_custom_user_short_code_key": "custom_user_short_code <> ''",
	}
	for _, ix := range mustTable(t, "user_tokens").Indexes {
		if where, ok := want[ix.Name]; ok {
			if !ix.Unique || ix.Where != where {
				t.Errorf("索引 %s 应当是非空时唯一，得到 %+v", ix.Name, ix)
			}
			delete(want, ix.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("user_tokens 缺少短码唯一索引：%v", want)
	}
	found := false
	for _, ix := range mustTable(t, "user_inbound_configs").Indexes {
		if ix.Unique && strings.Join(ix.Columns, ",") == "username,server_id,inbound_tag" {
			found = true
		}
	}
	if !found {
		t.Error("user_inbound_configs 应当有 (username, server_id, inbound_tag) 唯一索引")
	}
}

func TestValidateRejectsImmutableOutsideSpec(t *testing.T) {
	r := New()
	r.Add(Table{Name: "a", Kind: "A", KindClass: KindConfig, Columns: []Column{col("id", TypeSerial), col("x", TypeText).cls(ClassStatus).immutable()}})
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "Immutable") {
		t.Fatalf("status 列标 Immutable 应当被拒，得到 %v", err)
	}
}
