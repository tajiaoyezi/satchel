package schema

import (
	"sort"
	"strings"
	"testing"
)

// storage-schema「Settings key catalog」：mmwx 主控源码里出现过的全部 system_settings key（114 个，
// 从 GetSystemSetting / SetSystemSetting / 直接 SQL（含参数化的 key = ?）的调用点解析常量得到），每一个要么在目录里、要么在不搬清单里。
// 这份清单是手抄的 golden：mmwx 快照不再变，它就是「一个不漏」的证据；复核方法见 design 第 4 条。
var mmwxSettingKeys = []string{
	"_migrate_credential_email_done", "_migrate_merge_email_traffic_done",
	"_migrate_orphan_inbound_configs_done", "announce_official_probe", "announce_probe_server_ids",
	"announce_probe_tester_ids", "announcement_config", "api_token", "block_unknown_subscription_ua",
	"branding_brand_title", "branding_logo_ext", "branding_logo_url", "branding_site_title",
	"brute_force_block_minutes", "brute_force_enabled", "brute_force_max_failures",
	"brute_force_window_minutes", "dashboard_refresh_interval_ms", "default_theme", "external_https",
	"license_badge_display", "license_key", "license_server_url", "license_status", "login_rate_lock_minutes",
	"login_rate_max_attempts", "login_rate_window_minutes", "login_wallpaper", "master_cert_pending",
	"master_force_public_http", "master_https_recovery_enabled", "master_https_recovery_pending",
	"master_https_recovery_reason", "master_local_only", "master_recovery_failure_minutes",
	"master_recovery_startup_grace_minutes", "master_recovery_url", "master_url",
	"notify_daily_traffic_template", "notify_server_tolerance_seconds", "pending_outbound_address_replacements",
	"primary_admin_username", "probe_cdn_regions_endpoint", "probe_disguise_block_login",
	"probe_disguise_enabled", "probe_disguise_logo", "probe_disguise_metric_cpu", "probe_disguise_metric_disk",
	"probe_disguise_metric_mem", "probe_disguise_metric_ping", "probe_disguise_metric_speed",
	"probe_disguise_metric_traffic", "probe_disguise_ping_interval_ms", "probe_disguise_ping_targets",
	"probe_disguise_ping_targets_override", "probe_disguise_server_ids", "probe_disguise_show_daily_trend",
	"probe_disguise_show_expiry", "probe_disguise_show_external_license", "probe_disguise_show_globe",
	"probe_disguise_show_health_score", "probe_disguise_show_name", "probe_disguise_show_price",
	"probe_disguise_show_renewal_timeline", "probe_disguise_show_resource_heatmap",
	"probe_disguise_show_return_route", "probe_disguise_show_traffic_7d",
	"probe_disguise_show_traffic_hotspots", "probe_disguise_show_traffic_quota", "probe_disguise_theme",
	"probe_disguise_title", "probe_external_access_only", "probe_external_enabled",
	"probe_external_token_sha256", "probe_internal_enabled", "probe_quality_alert_config",
	"probe_quality_alert_states", "reality_domain_share_enabled", "reality_domains", "reality_domains_blocked",
	"reality_domains_share_optout", "reality_domains_shared", "redeem_copy_template", "require_encryption",
	"skip_local_ip", "sub_rate_enabled", "sub_rate_limit", "sub_rate_window_minutes", "subscription_url",
	"system_traffic_offset_resync_v1_done", "tgbot_admin_ids", "tgbot_enabled", "tgbot_token", "tgbot_url",
	"tgbot_webapp_dev_preview", "traffic_offset_clear_negative_v1_done", "traffic_restore_node_v1_done",
	"traffic_restore_user_v1_done", "traffic_total_reset_v2_done", "turnstile_secret_key", "turnstile_site_key",
	"update_cdn_enabled", "use_grpc", "user_hidden_rule_templates", "user_perm_pages", "user_quota_override",
	"user_quota_routed_outbound", "user_quota_subscribe", "user_quota_template",
	"user_routed_outbound_daily_limit", "user_routed_outbound_enabled", "user_visible_owned_rule_templates",
	"weighted_attrib_repair_v1_done", "weighted_traffic_backfill_done",
}

// 不搬的 22 个：并入 ApiToken、License 不适用（含探针页的许可徽章开关）、无调用方、PRO 官方探测源、mmwx 已废弃、
// 一次性数据修复标记、Reality 域名共享池（第 10 章：需要中心服务器，不做）。
var droppedSettingKeys = []string{
	"api_token", "license_key", "license_status", "license_server_url", "license_badge_display", "use_grpc",
	"announce_official_probe", "announce_probe_server_ids", "_migrate_credential_email_done",
	"_migrate_merge_email_traffic_done", "_migrate_orphan_inbound_configs_done",
	"system_traffic_offset_resync_v1_done", "traffic_offset_clear_negative_v1_done",
	"traffic_restore_node_v1_done", "traffic_restore_user_v1_done", "traffic_total_reset_v2_done",
	"weighted_traffic_backfill_done", "weighted_attrib_repair_v1_done", "reality_domain_share_enabled",
	"reality_domains_shared", "reality_domains_share_optout", "probe_disguise_show_external_license",
}

func settingsTable(t *testing.T) *Table {
	t.Helper()
	return mustTable(t, "system_config")
}

func TestSettingsCatalogCoversMMWX(t *testing.T) {
	catalog := map[string]bool{}
	for _, k := range settingsTable(t).Settings {
		catalog[k.Name] = true
	}
	dropped := map[string]bool{}
	for _, k := range droppedSettingKeys {
		dropped[k] = true
		if catalog[k] {
			t.Errorf("%s 在不搬清单里，不该进目录", k)
		}
	}
	seen := map[string]bool{}
	for _, k := range mmwxSettingKeys {
		seen[k] = true
		if !catalog[k] && !dropped[k] {
			t.Errorf("mmwx 的 key %s 既不在目录里也不在不搬清单里", k)
		}
	}
	for k := range catalog {
		if !seen[k] {
			t.Errorf("目录里的 key %s 在 mmwx 里不存在", k)
		}
	}
	if len(mmwxSettingKeys) != 114 || len(droppedSettingKeys) != 22 || len(catalog) != 92 {
		t.Errorf("mmwx 114 个、不搬 22 个、目录 92 个，得到 %d / %d / %d", len(mmwxSettingKeys), len(droppedSettingKeys), len(catalog))
	}
}

func TestSettingsCatalogTiers(t *testing.T) {
	tbl := settingsTable(t)
	counts := map[Class]int{}
	var masked []string
	for _, k := range tbl.Settings {
		counts[k.Class]++
		if k.Masked {
			masked = append(masked, k.Name)
		}
		if _, clash := tbl.Column(k.Name); clash {
			t.Errorf("key %s 与 system_config 的列同名", k.Name)
		}
	}
	want := map[Class]int{ClassHuman: 17, ClassMasterSelf: 9, ClassSpec: 58, ClassReadOnly: 1, ClassStatus: 7}
	for class, n := range want {
		if counts[class] != n {
			t.Errorf("%s 档应当 %d 个 key，得到 %d", class, n, counts[class])
		}
	}
	if len(counts) != len(want) {
		t.Errorf("出现了清单外的分档：%v", counts)
	}
	if strings.Join(masked, ",") != "turnstile_secret_key,tgbot_token,probe_external_token_sha256" {
		t.Errorf("打码的 key 应当恰好三个，得到 %v", masked)
	}
	for _, name := range []string{"master_url", "subscription_url", "master_recovery_url", "master_local_only", "probe_disguise_block_login", "tgbot_token", "tgbot_admin_ids"} {
		if c := findSetting(tbl, name); c == nil || c.Class != ClassHuman {
			t.Errorf("%s 应当是人类专属", name)
		}
	}
	if c := findSetting(tbl, "require_encryption"); c == nil || c.Class != ClassReadOnly {
		t.Error("require_encryption 应当是只读展示")
	}
	for _, name := range []string{"master_https_recovery_pending", "master_force_public_http", "primary_admin_username", "probe_quality_alert_states"} {
		if c := findSetting(tbl, name); c == nil || c.Class != ClassStatus {
			t.Errorf("%s 应当是运行态（status）", name)
		}
	}
	// 类型按 mmwx 的读写方式核过：整数配额四个都是 atoi / Itoa，不是 JSON。
	wantType := map[string]Type{
		"user_perm_pages": TypeJSON, "user_quota_subscribe": TypeInt, "user_quota_template": TypeInt, "user_quota_routed_outbound": TypeInt,
		"user_quota_override": TypeInt, "user_routed_outbound_daily_limit": TypeInt, "tgbot_admin_ids": TypeJSON, "probe_disguise_server_ids": TypeJSON,
		"probe_disguise_ping_targets": TypeJSON, "dashboard_refresh_interval_ms": TypeInt, "probe_disguise_ping_interval_ms": TypeInt,
		"master_url": TypeText, "master_local_only": TypeBool, "require_encryption": TypeBool, "announcement_config": TypeJSON,
	}
	for name, typ := range wantType {
		if c := findSetting(tbl, name); c == nil || c.Type != typ {
			t.Errorf("%s 的类型应当是 %s，得到 %+v", name, typ, c)
		}
	}
	// 目录只有主控设置类那张表带。
	for _, other := range Default().Tables() {
		if other.Name != "system_config" && len(other.Settings) != 0 {
			t.Errorf("表 %s 不该带 key 目录", other.Name)
		}
	}
}

func findSetting(t *Table, name string) *Column {
	for i := range t.Settings {
		if t.Settings[i].Name == name {
			return &t.Settings[i]
		}
	}
	return nil
}

// key 不是列：DDL 与 bun 模型里不能出现任何一个 key 的名字（列名与 key 名不撞，所以能整文件搜）。
func TestSettingsKeysStayOutOfDDLAndModel(t *testing.T) {
	outputs, err := Outputs(Default())
	if err != nil {
		t.Fatal(err)
	}
	var kinds string
	for path, data := range outputs {
		text := string(data)
		if strings.HasSuffix(path, "zz_generated_kinds.go") {
			kinds = text
			continue
		}
		for _, k := range settingsTable(t).Settings {
			if strings.Contains(text, "\""+k.Name+"\"") || strings.Contains(text, " "+k.Name+" ") {
				t.Errorf("%s 里出现了设置 key %s", path, k.Name)
			}
		}
	}
	for _, k := range settingsTable(t).Settings {
		if !strings.Contains(kinds, "`json:\""+k.Name+"\"`") {
			t.Errorf("kind 清单里缺少设置 key %s 的结构体字段", k.Name)
		}
	}
}

func TestValidateRejectsBadSettings(t *testing.T) {
	base := func() Table {
		return Table{Name: "cfg", Kind: "Cfg", KindClass: KindMasterSettings,
			Columns: []Column{col("id", TypeInt).def("1").check("id = 1"), col("title", TypeText).def("''")}, PrimaryKey: []string{"id"}}
	}
	cases := map[string]struct {
		mutate func(*Table)
		want   string
	}{
		"配置类表挂目录": {func(t *Table) { t.KindClass = KindConfig; t.Settings = []Column{col("a", TypeText)} }, "只有主控设置类"},
		"与列同名":    {func(t *Table) { t.Settings = []Column{col("title", TypeText)} }, "与列同名"},
		"重复":      {func(t *Table) { t.Settings = []Column{col("a", TypeText), col("a", TypeInt)} }, "重复"},
		"名字不合法":   {func(t *Table) { t.Settings = []Column{col("Bad-Key", TypeText)} }, "小写下划线"},
		"元数据档":    {func(t *Table) { t.Settings = []Column{col("a", TypeText).cls(ClassMeta)} }, "分档只能是"},
		"动作档":     {func(t *Table) { t.Settings = []Column{col("a", TypeText).cls(ClassAction)} }, "分档只能是"},
		"时间类型":    {func(t *Table) { t.Settings = []Column{col("a", TypeTime)} }, "类型只能是"},
		"bool 打码": {func(t *Table) { t.Settings = []Column{col("a", TypeBool).masked()} }, "打码只能"},
		"带默认值":    {func(t *Table) { t.Settings = []Column{col("a", TypeText).def("''")} }, "不是列"},
		"带省略即为真":  {func(t *Table) { t.Settings = []Column{col("a", TypeBool).def("FALSE").defaultTrue()} }, "不是列"},
	}
	for name, tc := range cases {
		tbl := base()
		tc.mutate(&tbl)
		r := New()
		r.Add(tbl)
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s 应当被拒并提到 %q，得到 %v", name, tc.want, err)
		}
	}
	// 合法的目录通过校验，且 KindColumns 是列加 key。
	ok := base()
	ok.Settings = []Column{col("a", TypeText).masked(), col("b", TypeBool).cls(ClassStatus)}
	r := New()
	r.Add(ok)
	if err := r.Validate(); err != nil {
		t.Fatalf("合法目录不该被拒：%v", err)
	}
	got, _ := r.Table("cfg")
	var names []string
	for _, c := range got.KindColumns() {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a,b,id,resource_version,title" {
		t.Errorf("KindColumns 应当是列加 key，得到 %v", names)
	}
}
