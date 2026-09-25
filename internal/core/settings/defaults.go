package settings

import "encoding/json"

// defaults 是键值表里 93 个 key 的读侧默认值（master-settings「settings show 返回合并后的整个对象」）：键值表里没有这个 key 时用它。
// 值照 mmwx 各端点读侧的 fallback 抄，每项注释 mmwx 主控源码位置（详见本 change 的 notes/mmwx-settings-defaults.md）；
// 与 mmwx 不同的几处写在行内注释里。列的默认值在 DDL 里（tables_ops.go），读行即得，不在这里。
// 类型固定：bool → bool、int → int64、text → string、json → json.RawMessage；tables_settings_test 之外还有一份测试断言每个 key 都有、类型与目录一致。
var defaults = map[string]any{
	// 七组人类专属·主控地址。master_url 在 mmwx 里由 setup 写、没有常量默认（handler/setup.go:255）；
	// master_recovery_url 在 mmwx 里是由 master_url 加监听端口推导的（handler/master_https_recovery.go），这里默认空串，M6 自愈再算有效值。
	"master_url":          "",
	"master_recovery_url": "",
	"subscription_url":    "", // 为空时 TG 机器人退回用 master_url（tgbot/manager.go:127-131）

	// 七组人类专属·门（securityDefaults handler/security_settings.go:51-64；读侧空 / 非法回默认 :250-272）。
	"master_local_only":          false, // handler/system_settings.go:146
	"probe_disguise_block_login": false, // handler/system_settings.go:491
	"brute_force_enabled":        true,
	"brute_force_max_failures":   int64(5),
	"brute_force_window_minutes": int64(1440),
	"brute_force_block_minutes":  int64(1440),
	"login_rate_max_attempts":    int64(5),
	"login_rate_window_minutes":  int64(60),
	"login_rate_lock_minutes":    int64(60),
	"skip_local_ip":              true,
	"turnstile_site_key":         "",
	"turnstile_secret_key":       "",
	"trusted_proxies":            json.RawMessage(`[]`), // Satchel 新增：没有登记任何反向代理，请求头一律不信（第 06 章）

	// 七组人类专属·Telegram（tgbot/manager.go:62-70）。
	"tgbot_token":     "",
	"tgbot_admin_ids": json.RawMessage(`[]`),

	// 主控自身类。
	"update_cdn_enabled":                    true,                                           // handler/update_cdn.go:31-33；启动时只有 "0" / "false" 才关（main.go:1013）
	"tgbot_enabled":                         false,                                          // tgbot/manager.go:63
	"tgbot_url":                             "",                                             // tgbot/manager.go:72，bot 启动后由系统写
	"tgbot_webapp_dev_preview":              false,                                          // tgbot/manager.go:71
	"master_https_recovery_enabled":         false,                                          // handler/system_settings.go:147
	"master_recovery_failure_minutes":       int64(5),                                       // parsePositiveInt(raw, 5) handler/system_settings.go:151
	"master_recovery_startup_grace_minutes": int64(10),                                      // handler/system_settings.go:152
	"external_https":                        false,                                          // handler/system_settings.go:311-313
	"probe_cdn_regions_endpoint":            "https://lf3-ips.zstaticcdn.com/nodes_data.js", // handler/probe_cdn_proxy.go:39（公开的 IP 归属数据源，不是 mmwx 自己的域名）

	// 日常运维·品牌与外观（handler/branding.go:73-79；handler/system_settings.go:1369）。
	"branding_site_title":  "",
	"branding_brand_title": "",
	"branding_logo_url":    "",
	"branding_logo_ext":    "",
	"login_wallpaper":      "",
	// mmwx 默认 pixel（handler/system_settings.go:1290）；Satchel 先做扁平主题（附录 B 第 19 行），默认 flat。
	"default_theme": "flat",

	// 日常运维·采集、文案与通知参数。
	"dashboard_refresh_interval_ms": int64(5000), // handler/system_settings.go:935
	// defaultRedeemTemplate handler/system_settings.go:838-844；末段的产品名换成「面板」。
	"redeem_copy_template":            "使用教程\n打开这个机器人 {机器人地址}\n点左下角我的面板，然后输入兑换码注册\n{兑换码}\n\n如果需要自定义出站落地，需要登录面板\n{主控域名}",
	"notify_daily_traffic_template":   "",         // 空等于渲染时用内置模板（handler/notify_daily_template.go:17-31），M4 沿用
	"notify_server_tolerance_seconds": int64(120), // storage/traffic.go:10797-10812

	// 日常运维·订阅保护（securityDefaults handler/security_settings.go:58-61）。
	"sub_rate_enabled":              true,
	"sub_rate_limit":                int64(60),
	"sub_rate_window_minutes":       int64(1),
	"block_unknown_subscription_ua": false,

	// 日常运维·探针与伪装页（handler/system_settings.go:412-512 的读侧）。
	// probe_internal_enabled / probe_external_enabled 在 mmwx 里靠旧 key 兼容推导（:421-424），Satchel 默认 false，兼容逻辑归 M9 导入。
	"probe_disguise_enabled":               false,
	"probe_internal_enabled":               false,
	"probe_external_enabled":               false,
	"probe_external_access_only":           false,
	"probe_external_token_sha256":          "",
	"probe_disguise_title":                 "",
	"probe_disguise_theme":                 "follow", // :427-432，非法名当 follow
	"probe_disguise_logo":                  "",
	"probe_disguise_server_ids":            json.RawMessage(`[]`),
	"probe_disguise_show_name":             false, // 读 == "1"
	"probe_disguise_metric_cpu":            false,
	"probe_disguise_metric_mem":            false,
	"probe_disguise_metric_disk":           false,
	"probe_disguise_metric_ping":           false,
	"probe_disguise_metric_traffic":        true, // 读 != "0"：没设过就是开（:449-451, 500）
	"probe_disguise_metric_speed":          true, // 读 != "0"（:501）
	"probe_disguise_show_expiry":           false,
	"probe_disguise_show_price":            false,
	"probe_disguise_show_globe":            false,
	"probe_disguise_show_daily_trend":      true,  // 读 != "0"（:505）
	"probe_disguise_show_traffic_hotspots": true,  // 读 != "0"（:506）
	"probe_disguise_show_traffic_7d":       true,  // 读 != "0"（:507）
	"probe_disguise_show_resource_heatmap": true,  // 读 != "0"（:508）
	"probe_disguise_show_traffic_quota":    true,  // 读 != "0"（:509）
	"probe_disguise_show_renewal_timeline": true,  // 读 != "0"（:510）
	"probe_disguise_show_health_score":     false, // 读 == "1"（:511）
	"probe_disguise_show_return_route":     false, // 读 == "1"（:512）
	"probe_disguise_ping_targets":          json.RawMessage(`[]`),
	"probe_disguise_ping_targets_override": json.RawMessage(`{}`),
	// 代码里的默认是 60000（handler/system_settings.go:477-480）；:386 的常量注释写 5000 与代码不一致，取代码。
	"probe_disguise_ping_interval_ms": int64(60000),
	// DefaultProbeQualityAlertConfig handler/probe_quality_alert.go:33-38。
	"probe_quality_alert_config": json.RawMessage(`{"enabled":false,"jitter_threshold_ms":80,"loss_threshold_pct":20,"window_minutes":5,"min_samples":5,"trigger_consecutive":2,"recover_consecutive":2,"cooldown_minutes":30}`),

	// 日常运维·公告（defaultAnnounceConfig handler/announcement.go:57-66；handler/announcement.go:136-143）。
	"announcement_config": json.RawMessage(`{"types":{` +
		`"node_blocked":{"enabled":true,"title":"节点异常","template":"⚠️ 节点【{node}】疑似被墙,暂时无法连接,请先切换其他节点,我们正在处理。","via_bot":true,"via_miniapp":true},` +
		`"node_recovered":{"enabled":true,"title":"节点恢复","template":"✅ 节点【{node}】已恢复,可正常使用。","via_bot":true,"via_miniapp":true},` +
		`"maintenance":{"enabled":true,"title":"系统维护","template":"🛠 系统将于 {time} 维护,期间可能短暂不可用,敬请谅解。","via_bot":true,"via_miniapp":true},` +
		`"sub_update":{"enabled":true,"title":"订阅更新","template":"🔄 节点有更新,请重新拉取订阅以获取最新节点。","via_bot":true,"via_miniapp":true},` +
		`"general":{"enabled":true,"title":"公告","template":"","via_bot":true,"via_miniapp":true}}}`),
	"announce_probe_tester_ids": json.RawMessage(`[]`),

	// 日常运维·Reality 域名清单（handler/reality_domain_inventory.go:125-136）。
	"reality_domains":         json.RawMessage(`[]`),
	"reality_domains_blocked": json.RawMessage(`[]`),

	// 日常运维·用户权限与配额（handler/user_permissions.go:36-37, 72-107）。
	// user_quota_routed_outbound 与 user_routed_outbound_daily_limit 的 0 在 mmwx 里是「用默认」而不是「不限」，读侧回 2 / 5；这里默认值直接给 2 / 5。
	"user_perm_pages":                  json.RawMessage(`[]`),
	"user_quota_subscribe":             int64(0),
	"user_quota_template":              int64(0),
	"user_quota_routed_outbound":       int64(2),
	"user_quota_override":              int64(0),
	"user_routed_outbound_enabled":     false,
	"user_routed_outbound_daily_limit": int64(5),

	// 日常运维·规则模板对用户的可见性（handler/rule_templates.go:83-99）。
	"user_hidden_rule_templates":        json.RawMessage(`[]`),
	"user_visible_owned_rule_templates": json.RawMessage(`[]`),

	// 只读展示：Satchel 常开、不可关（第 06 章）；Load 里恒置 true，这里的值只是让目录完整。
	"require_encryption": true,

	// 运行态（主控自己写，设置写接口拒绝）。primary_admin_username 在 mmwx 里空时回填最早的用户名（storage/nodes.go:1328-1344），M3 再定。
	"master_https_recovery_pending":         false,
	"master_https_recovery_reason":          "",
	"master_cert_pending":                   "",
	"master_force_public_http":              false,
	"primary_admin_username":                "",
	"pending_outbound_address_replacements": json.RawMessage(`{}`),
	"probe_quality_alert_states":            json.RawMessage(`{}`),
}
