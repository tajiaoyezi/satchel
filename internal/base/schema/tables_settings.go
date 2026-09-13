package schema

// SystemSettings 单例在键值附属表 system_settings 里的 key 目录（storage-schema「Settings key catalog」）。
// 每个 key 是一个没有数据库表示的「逻辑列」：只用名字、类型、分档、打码四项，进 kind 清单与 Spec / Status 结构体，
// 不进 DDL 与 bun 模型。归类依据是第 05 章七组与功能②的打码清单、第 07 章主控设置类那一行、第 10 章系统设置段。
// 来源是 mmwx 主控源码里全部 GetSystemSetting / SetSystemSetting / 直接 SQL 的调用点（114 个 key），
// 其中 22 个不搬，见 tables_settings_test.go 的 golden 清单。值的编码：布尔读时接受 1 / 0 / true / false / 空，写时统一 true / false；
// 整数十进制；json 原文。默认值不在这里，随 M1 的设置服务。

func settingsKeys() []Column {
	return []Column{
		// 七组人类专属·主控地址（第 05 章：改主控地址、主控迁移、HTTPS 自愈的恢复地址；订阅域名改它等于把全部用户的订阅链接指到别处，按同一组处理）
		col("master_url", TypeText).cls(ClassHuman),
		col("master_recovery_url", TypeText).cls(ClassHuman),
		col("subscription_url", TypeText).cls(ClassHuman),

		// 七组人类专属·门（第 05 章：关闭公网访问、探针伪装页的隐藏登录入口、暴力破解防护、登录限流、Turnstile 验证码）
		col("master_local_only", TypeBool).cls(ClassHuman),
		col("probe_disguise_block_login", TypeBool).cls(ClassHuman),
		col("brute_force_enabled", TypeBool).cls(ClassHuman),
		col("brute_force_max_failures", TypeInt).cls(ClassHuman),
		col("brute_force_window_minutes", TypeInt).cls(ClassHuman),
		col("brute_force_block_minutes", TypeInt).cls(ClassHuman),
		col("login_rate_max_attempts", TypeInt).cls(ClassHuman),
		col("login_rate_window_minutes", TypeInt).cls(ClassHuman),
		col("login_rate_lock_minutes", TypeInt).cls(ClassHuman),
		col("skip_local_ip", TypeBool).cls(ClassHuman),
		col("turnstile_site_key", TypeText).cls(ClassHuman),
		col("turnstile_secret_key", TypeText).cls(ClassHuman).masked(),

		// 七组人类专属·Telegram（第 05 章：改机器人 token，与 system_config.telegram_bot_token（通知推送用）是两把不同的 token；管理员 TG id 名单决定谁能当 TG 管理员，改它等于给人加权——第 10 章字面上归「其它设置」，按第 05 章七组的目的归这里）
		col("tgbot_token", TypeText).cls(ClassHuman).masked(),
		col("tgbot_admin_ids", TypeJSON).cls(ClassHuman),

		// 主控自身类（第 10 章：更新 CDN、TG 机器人其它设置；HTTPS 自愈参数与「外部已配 HTTPS」标记同属主控自身）
		col("update_cdn_enabled", TypeBool).cls(ClassMasterSelf),
		col("tgbot_enabled", TypeBool).cls(ClassMasterSelf),
		col("tgbot_url", TypeText).cls(ClassMasterSelf),
		col("tgbot_webapp_dev_preview", TypeBool).cls(ClassMasterSelf),
		col("master_https_recovery_enabled", TypeBool).cls(ClassMasterSelf),
		col("master_recovery_failure_minutes", TypeInt).cls(ClassMasterSelf),
		col("master_recovery_startup_grace_minutes", TypeInt).cls(ClassMasterSelf),
		col("external_https", TypeBool).cls(ClassMasterSelf),
		col("probe_cdn_regions_endpoint", TypeText).cls(ClassMasterSelf),

		// 日常运维·品牌与外观（第 10 章：站点标题、logo、登录壁纸、默认主题）
		col("branding_site_title", TypeText),
		col("branding_brand_title", TypeText),
		col("branding_logo_url", TypeText),
		col("branding_logo_ext", TypeText),
		col("login_wallpaper", TypeText),
		col("default_theme", TypeText),

		// 日常运维·采集、文案与通知参数
		col("dashboard_refresh_interval_ms", TypeInt),
		col("redeem_copy_template", TypeText),
		col("notify_daily_traffic_template", TypeText),
		col("notify_server_tolerance_seconds", TypeInt),

		// 日常运维·订阅保护（订阅频率限制、只放行可识别的客户端 UA；不在七组「门」的清单里）
		col("sub_rate_enabled", TypeBool),
		col("sub_rate_limit", TypeInt),
		col("sub_rate_window_minutes", TypeInt),
		col("block_unknown_subscription_ua", TypeBool),

		// 日常运维·探针与伪装页（第 10 章探针伪装页设置；probe_external_token_sha256 是外置探针的访问密钥哈希，第 05 章功能②要打码；许可徽章开关不搬）
		col("probe_disguise_enabled", TypeBool),
		col("probe_internal_enabled", TypeBool),
		col("probe_external_enabled", TypeBool),
		col("probe_external_access_only", TypeBool),
		col("probe_external_token_sha256", TypeText).masked(),
		col("probe_disguise_title", TypeText),
		col("probe_disguise_theme", TypeText),
		col("probe_disguise_logo", TypeText),
		col("probe_disguise_server_ids", TypeJSON),
		col("probe_disguise_show_name", TypeBool),
		col("probe_disguise_metric_cpu", TypeBool),
		col("probe_disguise_metric_mem", TypeBool),
		col("probe_disguise_metric_disk", TypeBool),
		col("probe_disguise_metric_ping", TypeBool),
		col("probe_disguise_metric_traffic", TypeBool),
		col("probe_disguise_metric_speed", TypeBool),
		col("probe_disguise_show_expiry", TypeBool),
		col("probe_disguise_show_price", TypeBool),
		col("probe_disguise_show_globe", TypeBool),
		col("probe_disguise_show_daily_trend", TypeBool),
		col("probe_disguise_show_traffic_hotspots", TypeBool),
		col("probe_disguise_show_traffic_7d", TypeBool),
		col("probe_disguise_show_resource_heatmap", TypeBool),
		col("probe_disguise_show_traffic_quota", TypeBool),
		col("probe_disguise_show_renewal_timeline", TypeBool),
		col("probe_disguise_show_health_score", TypeBool),
		col("probe_disguise_show_return_route", TypeBool),
		col("probe_disguise_ping_targets", TypeJSON),
		col("probe_disguise_ping_targets_override", TypeJSON),
		col("probe_disguise_ping_interval_ms", TypeInt),
		col("probe_quality_alert_config", TypeJSON),

		// 日常运维·公告（公告模板配置与被墙探测用的家用测速端）
		col("announcement_config", TypeJSON),
		col("announce_probe_tester_ids", TypeJSON),

		// 日常运维·Reality 域名清单（第 10 章证书与域名，M6；共享池的三个 key 不搬）
		col("reality_domains", TypeJSON),
		col("reality_domains_blocked", TypeJSON),

		// 日常运维·用户权限与配额（全局策略）
		col("user_perm_pages", TypeJSON),
		col("user_quota_subscribe", TypeInt),
		col("user_quota_template", TypeInt),
		col("user_quota_routed_outbound", TypeInt),
		col("user_quota_override", TypeInt),
		col("user_routed_outbound_enabled", TypeBool),
		col("user_routed_outbound_daily_limit", TypeInt),

		// 日常运维·规则模板对用户的可见性
		col("user_hidden_rule_templates", TypeJSON),
		col("user_visible_owned_rule_templates", TypeJSON),

		// 只读展示（第 07 章：没有写接口；节点通信强制加密在 Satchel 常开、不可关，第 06 章）
		col("require_encryption", TypeBool).cls(ClassReadOnly),

		// 运行态（第 07 章：主控自己算或写的值归 status，设置写接口带上一律拒绝）
		col("master_https_recovery_pending", TypeBool).cls(ClassStatus),
		col("master_https_recovery_reason", TypeText).cls(ClassStatus),
		col("master_cert_pending", TypeText).cls(ClassStatus),
		col("master_force_public_http", TypeBool).cls(ClassStatus),
		col("primary_admin_username", TypeText).cls(ClassStatus),
		col("pending_outbound_address_replacements", TypeJSON).cls(ClassStatus),
		col("probe_quality_alert_states", TypeJSON).cls(ClassStatus),
	}
}
