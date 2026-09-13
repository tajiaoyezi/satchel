package schema

import (
	"strconv"
	"strings"
	"testing"
)

// 表名清单，按外键拓扑顺序；加表要同时改这里。
const goldenTables = "audit_logs,automation_rules,batch_op_counters,config_snapshots,custom_rules,dns_providers,invite_code_uses,invite_codes,ip_bans,node_traffic_snapshots,notify_channels,notify_deliveries,package_node_traffic_suspensions,package_user_node_traffic_baselines,packages,plans,rule_template_owners,rule_versions,security_events,server_system_traffic_snapshots,servers,batch_inbounds,batch_outbounds,certificates,evidence_packages,alerts,alert_deliveries,federated_servers,inbounds,jobs,node_traffic,outbounds,return_routes,routing_rules,shared_servers,shared_server_inbounds,speed_test_results,speed_testers,subscribe_files,custom_rule_applications,subscription_links,system_config,system_settings,task_runs,templates,tg_audit,traffic_daily_incomplete_dates,traffic_daily_meta,traffic_daily_nodes,traffic_daily_system_servers,traffic_daily_user_emails,traffic_daily_user_nodes,traffic_daily_users,traffic_daily_users_archived,traffic_records,traffic_snapshots,traffic_threshold_notified,user_email_traffic,user_email_traffic_snapshots,user_routed_outbound_actions,user_traffic,user_traffic_cycle_carry,user_traffic_records,user_traffic_snapshots,users,api_tokens,external_subscriptions,nodes,announcements,node_reachability,override_scripts,package_assignments,package_assignment_inbound_configs,package_assignment_subaccounts,proxy_provider_configs,renewal_requests,routing_rule_presets,sessions,tasks,traffic_daily_external_subscriptions,user_inbound_configs,user_outbounds,user_settings,user_subaccounts,user_subscriptions,user_tokens,websites"

func TestDefaultTables(t *testing.T) {
	r := Default()
	var names []string
	for _, tbl := range r.Tables() {
		names = append(names, tbl.Name)
	}
	if got := strings.Join(names, ","); got != goldenTables {
		t.Fatalf("表名清单变了：\n得到 %s\n想要 %s", got, goldenTables)
	}
}

func TestKindTablesCarryVersionColumnsByClass(t *testing.T) {
	for _, tbl := range Default().Tables() {
		_, hasDeleted := tbl.Column("deleted_at")
		switch tbl.KindClass {
		case KindConfig, KindAction:
			if !tbl.HasVersion() || !hasDeleted {
				t.Errorf("表 %s（%s）应当有 resource_version 与 deleted_at", tbl.Name, tbl.KindClass)
			}
		case KindMasterSettings:
			if !tbl.HasVersion() || hasDeleted {
				t.Errorf("表 %s（主控设置类）应当只有 resource_version", tbl.Name)
			}
		default:
			if tbl.HasVersion() || hasDeleted {
				t.Errorf("表 %s（%s）不该有 resource_version 或 deleted_at", tbl.Name, tbl.KindClass)
			}
		}
	}
}

func TestAppendOnlyTables(t *testing.T) {
	want := map[string]bool{
		"audit_logs": true, "config_snapshots": true, "batch_op_counters": true, "user_routed_outbound_actions": true,
		"rule_versions": true, "security_events": true, "speed_test_results": true, "tg_audit": true, "invite_code_uses": true,
	}
	for _, tbl := range Default().Tables() {
		if tbl.AppendOnly != want[tbl.Name] {
			t.Errorf("表 %s 的 append-only = %v，想要 %v", tbl.Name, tbl.AppendOnly, want[tbl.Name])
		}
	}
}

func TestEveryKindHasClass(t *testing.T) {
	for _, tbl := range Default().Tables() {
		if tbl.IsKind() && tbl.KindClass == KindNone {
			t.Errorf("kind %s 没有类别", tbl.Kind)
		}
	}
}

func TestNaturalKeysAndChecks(t *testing.T) {
	r := Default()
	// 只有用户起的名字是自然键（含「某台服务器下的 tag / domain / carrier」这类复合名字）；
	// job_id、delivery_id、token、short_code 是系统生成的键，撞上要报 conflict 而不是 name_taken。
	wantNatural := map[string]bool{
		"automation_rules_name_key": true, "notify_channels_name_key": true, "users_username_key": true,
		"packages_name_key": true, "servers_name_key": true, "subscribe_files_name_key": true,
		"subscription_links_name_key": true, "templates_name_key": true, "custom_rules_name_type_key": true,
		"dns_providers_name_key": true, "certificates_domain_server_key": true, "certificates_domain_local_key": true,
		"inbounds_server_tag_key": true, "outbounds_server_tag_key": true, "websites_server_domain_key": true,
		"return_routes_server_carrier_key": true,
	}
	for _, tbl := range r.Tables() {
		for _, ix := range tbl.Indexes {
			if ix.NaturalKey != wantNatural[ix.Name] {
				t.Errorf("索引 %s 的 NaturalKey = %v，与清单不符", ix.Name, ix.NaturalKey)
			}
			delete(wantNatural, ix.Name)
		}
	}
	if len(wantNatural) != 0 {
		t.Errorf("清单里的自然键索引不存在：%v", wantNatural)
	}
	alerts, _ := r.Table("alerts")
	if c, _ := alerts.Column("dedup_key"); c.Check != "dedup_key <> ''" {
		t.Errorf("alerts.dedup_key 应当有非空 CHECK，得到 %q", c.Check)
	}
	channels, _ := r.Table("notify_channels")
	if c, _ := channels.Column("enabled"); c.Default != "FALSE" {
		t.Errorf("notify_channels.enabled 默认值应当是 FALSE，得到 %q", c.Default)
	}
}

// 注册表里每个标了打码的列都要出现在生成的 kind 清单里（对照生成物）。
func TestMaskedColumnsAppearInGeneratedKinds(t *testing.T) {
	src, err := GenerateKinds(Default())
	if err != nil {
		t.Fatal(err)
	}
	text := squash(string(src))
	for _, tbl := range Default().Tables() {
		if !tbl.IsKind() {
			continue
		}
		for _, c := range tbl.Columns {
			if !c.Masked {
				continue
			}
			want := "Name: " + strconv.Quote(tbl.Kind)
			start := strings.Index(text, want)
			if start < 0 {
				t.Fatalf("生成物里没有 kind %s", tbl.Kind)
			}
			block := text[start:]
			if end := strings.Index(block, "}, {"); end > 0 {
				block = block[:end]
			}
			const marker = "MaskedFields: []string{"
			i := strings.Index(block, marker)
			if i < 0 {
				t.Fatalf("kind %s 的清单里没有 MaskedFields", tbl.Kind)
			}
			masked := block[i+len(marker):]
			masked = masked[:strings.Index(masked, "}")]
			if !strings.Contains(masked, strconv.Quote(c.Name)) {
				t.Errorf("kind %s 的打码列 %s 不在生成的 MaskedFields 里，得到 {%s}", tbl.Kind, c.Name, masked)
			}
			// 打码列在 Spec / Status 结构体里必须是 Secret / SecretJSON，序列化默认打码。
			field := GoName(c.Name)
			if !strings.Contains(text, field+" Secret ") && !strings.Contains(text, field+" *Secret ") && !strings.Contains(text, field+" SecretJSON ") {
				t.Errorf("kind %s 的打码列 %s 在生成的结构体里应当是 Secret / SecretJSON 类型", tbl.Kind, c.Name)
			}
		}
	}
}
