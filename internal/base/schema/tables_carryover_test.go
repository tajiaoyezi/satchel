package schema

import (
	"strings"
	"testing"
)

func hasForeignKey(tbl *Table, column, refTable, onDelete string) bool {
	for _, fk := range tbl.ForeignKeys {
		if strings.Join(fk.Columns, ",") == column && fk.RefTable == refTable && fk.OnDelete == onDelete {
			return true
		}
	}
	return false
}

func TestSubscriptionKinds(t *testing.T) {
	for _, kind := range []string{"SubscribeFile", "SubscriptionLink", "Template", "CustomRule", "ExternalSubscription", "ProxyProviderConfig", "OverrideScript"} {
		if kindTable(t, kind).KindClass != KindConfig {
			t.Errorf("%s 应当是配置类", kind)
		}
	}
	rule := kindTable(t, "CustomRule")
	found := false
	for _, ix := range rule.Indexes {
		if ix.NaturalKey && strings.Join(ix.Columns, ",") == "name,type" {
			found = true
		}
	}
	if !found {
		t.Error("CustomRule 的自然键应当是 (name, type)")
	}
	ext := kindTable(t, "ExternalSubscription")
	if got := strings.Join(columnsOfClass(ext, ClassStatus), ","); got != "download,expire,last_sync_at,node_count,total,upload" {
		t.Errorf("ExternalSubscription 的 status 列不对：%s", got)
	}
	for _, tbl := range []*Table{kindTable(t, "SubscribeFile"), ext} {
		if c, _ := tbl.Column("url"); !c.Masked || c.Class != ClassSpec {
			t.Errorf("%s.url 应当是打码的 spec 列", tbl.Name)
		}
	}
	for _, ix := range ext.Indexes {
		if ix.Name == "external_subscriptions_username_url_key" && (!ix.Unique || ix.NaturalKey) {
			t.Error("(username, url) 应当唯一但不是自然键")
		}
	}
	partial := map[string]bool{}
	for _, tbl := range Default().Tables() {
		for _, ix := range tbl.Indexes {
			if ix.Unique && ix.Where != "" {
				partial[ix.Name] = true
			}
		}
	}
	for _, name := range []string{"subscribe_files_file_short_code_key", "subscribe_files_custom_short_code_key", "subscription_links_short_url_key"} {
		if !partial[name] {
			t.Errorf("短码索引 %s 应当是非空时唯一", name)
		}
	}
	for _, name := range []string{"user_subscriptions", "custom_rule_applications", "routing_rule_presets", "rule_versions"} {
		if tbl := mustTable(t, name); tbl.IsKind() || tbl.HasVersion() {
			t.Errorf("%s 应当是附属表", name)
		}
	}
	if got := strings.Join(mustTable(t, "user_subscriptions").PKColumns(), ","); got != "username,subscription_id" {
		t.Errorf("user_subscriptions 的主键应当是 (username, subscription_id)，得到 %s", got)
	}
	if !hasForeignKey(mustTable(t, "proxy_provider_configs"), "external_subscription_id", "external_subscriptions", "CASCADE") {
		t.Error("proxy_provider_configs 应当级联指向 external_subscriptions")
	}
}

func TestCertificateCluster(t *testing.T) {
	cert := kindTable(t, "Certificate")
	spec := columnsOfClass(cert, ClassSpec)
	for _, want := range []string{"cert_pem", "key_pem", "server_id", "dns_provider_id", "challenge_mode", "auto_renew"} {
		if !contains(spec, want) {
			t.Errorf("Certificate 的 spec 应当含 %s", want)
		}
	}
	if got := strings.Join(columnsOfClass(cert, ClassStatus), ","); got != "cert_path,expiry_date,issue_date,key_path,message,status" {
		t.Errorf("Certificate 的 status 列不对：%s", got)
	}
	if hasColumn(cert, "remote_server_id") {
		t.Error("certificates 不该有 remote_server_id")
	}
	for _, c := range cert.Columns {
		if c.Masked != (c.Name == "key_pem") {
			t.Errorf("certificates 里只有 key_pem 打码，%s 的打码标记不对", c.Name)
		}
	}
	if !hasForeignKey(cert, "server_id", "servers", "RESTRICT") || !hasForeignKey(cert, "dns_provider_id", "dns_providers", "SET NULL") {
		t.Error("certificates 应当有指向 servers 的 RESTRICT 外键与指向 dns_providers 的 SET NULL 外键")
	}
	wantWhere := map[string]string{"certificates_domain_server_key": "server_id IS NOT NULL", "certificates_domain_local_key": "server_id IS NULL"}
	for _, ix := range cert.Indexes {
		if where, ok := wantWhere[ix.Name]; ok {
			if !ix.Unique || !ix.NaturalKey || ix.Where != where {
				t.Errorf("索引 %s 应当是自然键部分唯一索引，得到 %+v", ix.Name, ix)
			}
			delete(wantWhere, ix.Name)
		}
	}
	if len(wantWhere) != 0 {
		t.Errorf("certificates 缺少部分唯一索引：%v", wantWhere)
	}
	dns := kindTable(t, "DnsProvider")
	if c, _ := dns.Column("credentials"); !c.Masked || c.Type != TypeJSON {
		t.Error("dns_providers.credentials 应当是打码的 JSON 列")
	}
	// m0-04 留下的两个外键。
	if !hasForeignKey(kindTable(t, "Website"), "certificate_id", "certificates", "SET NULL") {
		t.Error("websites.certificate_id 应当指向 certificates SET NULL")
	}
	srv := kindTable(t, "Server")
	if c, _ := srv.Column("ddns_provider_id"); !c.Nullable || c.Default != "" {
		t.Error("servers.ddns_provider_id 应当可空且没有默认值 0")
	}
	if !hasForeignKey(srv, "ddns_provider_id", "dns_providers", "SET NULL") {
		t.Error("servers.ddns_provider_id 应当指向 dns_providers SET NULL")
	}
}

var ledgerTables = []string{
	"node_traffic", "user_traffic", "user_email_traffic", "user_traffic_cycle_carry",
	"node_traffic_snapshots", "user_traffic_snapshots", "user_email_traffic_snapshots", "traffic_snapshots", "server_system_traffic_snapshots",
	"traffic_daily_users", "traffic_daily_users_archived", "traffic_daily_user_nodes", "traffic_daily_user_emails", "traffic_daily_nodes",
	"traffic_daily_system_servers", "traffic_daily_external_subscriptions", "traffic_daily_incomplete_dates", "traffic_daily_meta",
	"traffic_records", "user_traffic_records", "traffic_threshold_notified",
}

// 账本 21 张表的形状（列、主键、索引、外键）与 mmwx 逐一对过后冻结在这里；改账本要同时改这里，并说明为什么动了 mmwx 的形状。
const ledgerGolden = `
node_traffic: id,server_id,tag,type,uplink,downlink,total_uplink,total_downlink,last_uplink,last_downlink,updated_at | pk=id | idx=node_traffic_server_tag_type_key(server_id,tag,type)! node_traffic_server_id_idx(server_id) node_traffic_tag_idx(tag) node_traffic_type_idx(type) | fk=server_id→servers CASCADE
user_traffic: id,server_id,username,uplink,downlink,total_uplink,total_downlink,last_uplink,last_downlink,cycle_start,updated_at | pk=id | idx=user_traffic_server_username_key(server_id,username)! user_traffic_server_id_idx(server_id) user_traffic_username_idx(username) | fk=server_id→servers CASCADE
user_email_traffic: id,server_id,email,uplink,downlink,total_uplink,total_downlink,last_uplink,last_downlink,cycle_base_uplink,cycle_base_downlink,weighted_uplink,weighted_downlink,cycle_base_weighted_uplink,cycle_base_weighted_downlink,attributed_username,cycle_start,updated_at | pk=id | idx=user_email_traffic_server_email_key(server_id,email)! user_email_traffic_server_id_idx(server_id) user_email_traffic_email_idx(email) user_email_traffic_attributed_username_idx(attributed_username) | fk=server_id→servers CASCADE
user_traffic_cycle_carry: username,weighted_uplink,weighted_downlink,updated_at | pk=username | idx= | fk=
node_traffic_snapshots: id,server_id,tag,type,date,uplink,downlink,created_at | pk=id | idx=node_traffic_snapshots_server_tag_type_date_key(server_id,tag,type,date)! node_traffic_snapshots_date_idx(date) | fk=
user_traffic_snapshots: id,server_id,username,date,uplink,downlink,created_at | pk=id | idx=user_traffic_snapshots_server_username_date_key(server_id,username,date)! user_traffic_snapshots_date_idx(date) | fk=
user_email_traffic_snapshots: id,server_id,email,date,uplink,downlink,created_at | pk=id | idx=user_email_traffic_snapshots_server_email_date_key(server_id,email,date)! user_email_traffic_snapshots_date_idx(date) | fk=
traffic_snapshots: id,server_id,date,inbound_uplink,inbound_downlink,outbound_uplink,outbound_downlink,user_uplink,user_downlink,created_at | pk=id | idx=traffic_snapshots_server_date_key(server_id,date)! traffic_snapshots_server_id_idx(server_id) traffic_snapshots_date_idx(date) | fk=server_id→servers CASCADE
server_system_traffic_snapshots: id,server_id,date,rx_cycle,tx_cycle,created_at | pk=id | idx=server_system_traffic_snapshots_server_date_key(server_id,date)! server_system_traffic_snapshots_date_idx(date) | fk=
traffic_daily_users: server_id,username,date,uplink,downlink,updated_at | pk=server_id,username,date | idx=traffic_daily_users_date_idx(date) | fk=server_id→servers CASCADE
traffic_daily_users_archived: username,date,uplink,downlink,updated_at | pk=username,date | idx=traffic_daily_users_archived_date_idx(date) | fk=
traffic_daily_user_nodes: server_id,node_id,username,date,uplink,downlink,weighted_uplink,weighted_downlink,updated_at | pk=server_id,node_id,username,date | idx=traffic_daily_user_nodes_date_idx(date) traffic_daily_user_nodes_user_idx(username) traffic_daily_user_nodes_node_idx(node_id) | fk=server_id→servers CASCADE
traffic_daily_user_emails: server_id,email,attributed_username,date,uplink,downlink,weighted_uplink,weighted_downlink,updated_at | pk=server_id,email,attributed_username,date | idx=traffic_daily_user_emails_date_idx(date) traffic_daily_user_emails_user_idx(attributed_username) | fk=server_id→servers CASCADE
traffic_daily_nodes: server_id,tag,type,date,uplink,downlink,updated_at | pk=server_id,tag,type,date | idx=traffic_daily_nodes_date_idx(date) | fk=server_id→servers CASCADE
traffic_daily_system_servers: server_id,date,uplink,downlink,updated_at | pk=server_id,date | idx=traffic_daily_system_servers_date_idx(date) | fk=server_id→servers CASCADE
traffic_daily_external_subscriptions: external_subscription_id,date,uplink,downlink,updated_at | pk=external_subscription_id,date | idx=traffic_daily_external_subscriptions_date_idx(date) | fk=external_subscription_id→external_subscriptions CASCADE
traffic_daily_incomplete_dates: date,reason,created_at | pk=date | idx= | fk=
traffic_daily_meta: key,value,updated_at | pk=key | idx= | fk=
traffic_records: date,total_limit,total_used,total_remaining,created_at | pk=date | idx= | fk=
user_traffic_records: username,date,total_limit,total_used,total_remaining,created_at | pk=username,date | idx= | fk=
traffic_threshold_notified: server_id,notified_at | pk=server_id | idx= | fk=
`

func ledgerShape(tbl *Table) string {
	var cols []string
	for _, c := range tbl.Columns {
		cols = append(cols, c.Name)
	}
	var idx []string
	for _, ix := range tbl.Indexes {
		s := ix.Name + "(" + strings.Join(ix.Columns, ",") + ")"
		if ix.Unique {
			s += "!"
		}
		idx = append(idx, s)
	}
	var fks []string
	for _, fk := range tbl.ForeignKeys {
		fks = append(fks, strings.Join(fk.Columns, ",")+"→"+fk.RefTable+" "+fk.OnDelete)
	}
	return tbl.Name + ": " + strings.Join(cols, ",") + " | pk=" + strings.Join(tbl.PKColumns(), ",") + " | idx=" + strings.Join(idx, " ") + " | fk=" + strings.Join(fks, " ")
}

func TestTrafficLedgerUntouched(t *testing.T) {
	ledger := ledgerTables
	if len(ledger) != 21 {
		t.Fatal("账本应当 21 张")
	}
	var shapes []string
	for _, name := range ledger {
		shapes = append(shapes, ledgerShape(mustTable(t, name)))
	}
	if got := strings.Join(shapes, "\n"); got != strings.TrimSpace(ledgerGolden) {
		t.Errorf("账本的形状变了：\n得到\n%s\n想要\n%s", got, strings.TrimSpace(ledgerGolden))
	}
	for _, name := range ledger {
		tbl := mustTable(t, name)
		if tbl.IsKind() || tbl.HasVersion() || tbl.AppendOnly {
			t.Errorf("%s 不该是 kind，也不该 append-only", name)
		}
		if c, ok := tbl.Column("date"); ok && c.Type != TypeText {
			t.Errorf("%s.date 应当保持文本", name)
		}
	}
	if got := strings.Join(mustTable(t, "traffic_daily_user_emails").PKColumns(), ","); got != "server_id,email,attributed_username,date" {
		t.Errorf("traffic_daily_user_emails 的主键不对：%s", got)
	}
	if hasColumn(mustTable(t, "traffic_daily_users_archived"), "server_id") {
		t.Error("traffic_daily_users_archived 不该有 server_id")
	}
	if len(mustTable(t, "node_traffic_snapshots").ForeignKeys) != 0 {
		t.Error("node_traffic_snapshots 在 mmwx 里没有外键，这里也不该有")
	}
	if c, _ := mustTable(t, "user_email_traffic").Column("weighted_uplink"); c.Type != TypeFloat {
		t.Error("user_email_traffic.weighted_uplink 应当是浮点")
	}
	nt := mustTable(t, "node_traffic")
	if c, _ := nt.Column("type"); strings.Join(c.Enum, ",") != "inbound,outbound" {
		t.Error("node_traffic.type 应当只认 inbound / outbound")
	}
	if !hasForeignKey(nt, "server_id", "servers", "CASCADE") || !hasForeignKey(mustTable(t, "traffic_daily_users"), "server_id", "servers", "CASCADE") {
		t.Error("node_traffic 与 traffic_daily_users 应当级联指向 servers")
	}
	if !hasForeignKey(mustTable(t, "traffic_daily_external_subscriptions"), "external_subscription_id", "external_subscriptions", "CASCADE") {
		t.Error("traffic_daily_external_subscriptions 应当级联指向 external_subscriptions")
	}
}

func TestOpsCluster(t *testing.T) {
	ann := kindTable(t, "Announcement")
	if ann.KindClass != KindConfig {
		t.Error("Announcement 应当是配置类")
	}
	if got := strings.Join(columnsOfClass(ann, ClassStatus), ","); got != "bot_delivered_at" {
		t.Errorf("Announcement 的 status 列应当只有 bot_delivered_at，得到 %s", got)
	}
	if !hasForeignKey(ann, "node_id", "nodes", "SET NULL") {
		t.Error("announcements.node_id 应当指向 nodes SET NULL")
	}
	for name, pk := range map[string]string{"security_events": "id", "task_runs": "id", "ip_bans": "ip", "speed_test_results": "id", "system_settings": "key"} {
		tbl := mustTable(t, name)
		if tbl.IsKind() || tbl.HasVersion() {
			t.Errorf("%s 应当是附属表", name)
		}
		if got := strings.Join(tbl.PKColumns(), ","); got != pk {
			t.Errorf("%s 的主键应当是 %s，得到 %s", name, pk, got)
		}
	}
}

func TestSystemSettingsSingleton(t *testing.T) {
	cfg := kindTable(t, "SystemSettings")
	if cfg.Name != "system_config" || cfg.KindClass != KindMasterSettings {
		t.Fatalf("SystemSettings 应当落在 system_config 上、类别主控设置类，得到 %s / %s", cfg.Name, cfg.KindClass)
	}
	if !cfg.HasVersion() || hasColumn(cfg, "deleted_at") {
		t.Error("system_config 应当只有 resource_version，没有 deleted_at")
	}
	id, _ := cfg.Column("id")
	if id.Type != TypeInt || id.Check != "id = 1" || id.Default != "1" || strings.Join(cfg.PKColumns(), ",") != "id" {
		t.Errorf("system_config 的主键应当是固定为 1、默认值也是 1 的整数 id，得到 %+v", id)
	}
	spec := columnsOfClass(cfg, ClassSpec)
	if len(spec) != 42 {
		t.Errorf("system_config 应当有 42 列 spec，得到 %d", len(spec))
	}
	for _, want := range []string{"heartbeat_interval", "notify_login", "enable_short_link", "node_name_multiplier_left"} {
		if !contains(spec, want) {
			t.Errorf("SystemSettings 的 spec 应当含 %s", want)
		}
	}
	// 七组：TG 机器人 token 与「门」里的静默模式。
	if got := columnsOfClass(cfg, ClassHuman); strings.Join(got, ",") != "silent_mode,silent_mode_timeout,telegram_bot_token" {
		t.Errorf("SystemSettings 的人类专属列不对：%v", got)
	}
	if got := columnsOfClass(cfg, ClassMasterSelf); strings.Join(got, ",") != "telegram_chat_id" {
		t.Errorf("SystemSettings 的主控自身类列应当只有 telegram_chat_id，得到 %v", got)
	}
	for _, c := range cfg.Columns {
		if c.Masked != (c.Name == "telegram_bot_token") {
			t.Errorf("system_config 里只有 telegram_bot_token 打码，%s 不对", c.Name)
		}
	}
	// 其它 kind 表的主键都是自增 id，只有这张单例表不是。
	for _, tbl := range Default().Tables() {
		if !tbl.IsKind() || tbl.Name == "system_config" {
			continue
		}
		if c, ok := tbl.Column("id"); !ok || c.Type != TypeSerial {
			t.Errorf("kind 表 %s 的主键应当是自增 id", tbl.Name)
		}
	}
}

// mmwx 默认 1、这里改成 FALSE 的布尔列（design 第 12 条的清单）：M1 起的创建路径要显式置 true，清单在这里有一份可执行的影子。
func TestFlippedBooleanDefaults(t *testing.T) {
	for _, ref := range []string{
		"custom_rules.enabled", "override_scripts.enabled", "proxy_provider_configs.health_check_enabled", "proxy_provider_configs.health_check_lazy",
		"certificates.auto_renew", "announcements.via_bot", "announcements.via_miniapp", "system_config.enable_short_link", "system_config.enable_miaomiaowu_features",
	} {
		parts := strings.SplitN(ref, ".", 2)
		c, ok := mustTable(t, parts[0]).Column(parts[1])
		if !ok || c.Type != TypeBool || c.Default != "FALSE" || c.Nullable {
			t.Errorf("%s 应当是默认 FALSE 的非空布尔列，得到 %+v", ref, c)
		}
	}
}

func TestFederationAndTelegramClusters(t *testing.T) {
	for name, pk := range map[string]string{
		"federated_servers": "server_id", "shared_servers": "id", "shared_server_inbounds": "share_id,inbound_tag", "rule_template_owners": "filename",
		"tg_audit": "id", "invite_codes": "code", "invite_code_uses": "code,username", "renewal_requests": "id", "speed_testers": "id",
	} {
		tbl := mustTable(t, name)
		if tbl.IsKind() || tbl.HasVersion() {
			t.Errorf("%s 应当是附属表", name)
		}
		if got := strings.Join(tbl.PKColumns(), ","); got != pk {
			t.Errorf("%s 的主键应当是 %s，得到 %s", name, pk, got)
		}
	}
	shared := mustTable(t, "shared_servers")
	if hasColumn(shared, "allow_manage_xray") || !hasColumn(shared, "allow_manage_inbounds") {
		t.Error("shared_servers 应当用 allow_manage_inbounds 取代 allow_manage_xray")
	}
	for table, column := range map[string]string{"federated_servers": "share_token", "shared_servers": "token_hash", "speed_testers": "token_hash", "renewal_requests": "request_token"} {
		if c, _ := mustTable(t, table).Column(column); !c.Masked {
			t.Errorf("%s.%s 应当打码", table, column)
		}
	}
	if c, _ := mustTable(t, "renewal_requests").Column("passphrase"); !c.Masked {
		t.Error("renewal_requests.passphrase 应当打码")
	}
	if !hasForeignKey(mustTable(t, "federated_servers"), "server_id", "servers", "CASCADE") || !hasForeignKey(mustTable(t, "shared_server_inbounds"), "share_id", "shared_servers", "CASCADE") {
		t.Error("联邦表应当级联指向 servers 与 shared_servers")
	}
	renew := mustTable(t, "renewal_requests")
	if c, ok := renew.Column("assignment_id"); !ok || !c.Nullable || !hasForeignKey(renew, "assignment_id", "package_assignments", "SET NULL") {
		t.Error("renewal_requests 应当有可空的 assignment_id，外键指向 package_assignments SET NULL")
	}
	if !hasForeignKey(renew, "username", "users", "CASCADE") {
		t.Error("renewal_requests.username 应当级联指向 users（悬空的待审申请会挡住同名新用户）")
	}
	if mustTable(t, "task_runs").AppendOnly {
		t.Error("task_runs 不是 append-only：mmwx 跑完任务会原地改 status")
	}
	pending := false
	for _, ix := range renew.Indexes {
		if ix.Name == "renewal_requests_pending_user_key" && ix.Unique && strings.Contains(ix.Where, "'pending'") {
			pending = true
		}
	}
	if !pending {
		t.Error("renewal_requests 应当有待审申请每人一条的部分唯一索引")
	}
	// 历史记录表不加外键。
	for _, name := range []string{"security_events", "task_runs", "speed_test_results", "tg_audit", "invite_codes", "invite_code_uses"} {
		if len(mustTable(t, name).ForeignKeys) != 0 {
			t.Errorf("记录表 %s 不该有外键", name)
		}
	}
}
