package schema

// 安全与运维簇（附录 A.2「安全与运维 改造」）：四张记录表照抄，announcements 升为 kind，
// system_settings 与 system_config 合成 SystemSettings 单例（第 07 章主控设置类：一个逻辑对象、一个 resourceVersion）。
// mmwx 的列集合来自 internal/storage/logs_tables.go 与 traffic.go。

func opsTables() []Table {
	return []Table{
		{
			// 安全事件（登录失败、封禁……），只追加。
			Name: "security_events", GoName: "SecurityEvent", Origin: OriginMMWX, AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("ip", TypeText),
				col("kind", TypeText),
				col("path", TypeText).def("''"),
				col("username", TypeText).def("''"),
				col("detail", TypeText).def("''"),
				col("actor", TypeText).def("''"),
			},
			Indexes: []Index{
				{Name: "security_events_at_idx", Columns: []string{"at"}},
				{Name: "security_events_ip_idx", Columns: []string{"ip"}},
				{Name: "security_events_kind_at_idx", Columns: []string{"kind", "at"}},
			},
		},
		{
			// IP 封禁，一 IP 一行；封禁与解封是 M1 的动作，不是 kind。
			Name: "ip_bans", GoName: "IPBan", Origin: OriginMMWX,
			Columns: []Column{
				col("ip", TypeText),
				col("reason", TypeText).def("''"),
				col("banned_at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("expires_at", TypeTime).null(),
				col("permanent", TypeBool).def("FALSE"),
				col("fail_count", TypeInt).def("0"),
				col("released_at", TypeTime).null(),
				col("actor", TypeText).def("''"),
			},
			PrimaryKey: []string{"ip"},
			Indexes:    []Index{{Name: "ip_bans_active_idx", Columns: []string{"released_at", "expires_at"}}},
		},
		{
			// 定时任务运行记录：mmwx 先插一行 running，跑完原地改 duration_ms / status / detail，所以不是 append-only。
			Name: "task_runs", GoName: "TaskRun", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("task_name", TypeText),
				col("started_at", TypeTime),
				col("duration_ms", TypeInt).def("0"),
				col("status", TypeText),
				col("detail", TypeText).def("''"),
			},
			Indexes: []Index{
				{Name: "task_runs_name_started_idx", Columns: []string{"task_name", "started_at"}},
				{Name: "task_runs_started_idx", Columns: []string{"started_at"}},
			},
		},
		{
			// 测速结果，只追加；node_id 指订阅节点，历史记录不加外键。
			Name: "speed_test_results", GoName: "SpeedTestResult", Origin: OriginMMWX, AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("node_id", TypeInt),
				col("node_name", TypeText).def("''"),
				col("source", TypeText).def("'master_local'"),
				col("down_mbps", TypeFloat).def("0"),
				col("latency_ms", TypeInt).def("-1"),
				col("test_bytes", TypeInt).def("0"),
				col("status", TypeText).def("'ok'"),
				col("error", TypeText).def("''"),
				col("tested_by", TypeText).def("''"),
				col("egress_ip", TypeText).def("''"),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{{Name: "speed_test_results_node_idx", Columns: []string{"node_id"}}},
		},
		{
			// 公告（含「被墙 / 恢复」公告关联的节点与 TG 投递字段）；node_id 为 NULL 表示不关联（取代 mmwx 的 0）。
			Name: "announcements", Kind: "Announcement", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("type", TypeText).def("'general'"),
				col("title", TypeText).def("''"),
				col("body", TypeText).def("''"),
				col("node_id", TypeInt).null(),
				col("via_bot", TypeBool).def("FALSE").defaultTrue(),
				col("via_miniapp", TypeBool).def("FALSE").defaultTrue(),
				col("expires_at", TypeTime).null(),
				col("bot_delivered_at", TypeTime).null().cls(ClassStatus),
			),
			ForeignKeys: []ForeignKey{fk("node_id", "nodes", "SET NULL")},
		},
		{
			// SystemSettings 单例的 kind 表：单行、主键固定为 1（默认值也是 1，不填 id 就落到这一行）、只有 resource_version（Add 不补 deleted_at）。
			// 列按第 10 章三档：telegram_bot_token（TG 机器人 token）与 silent_mode、silent_mode_timeout（第 05 章七组「门」里的静默模式）
			// 属七组（人类专属），telegram_chat_id 是通知渠道目标（主控自身类），其余是日常运维（spec）。单例行不在 0001 里预置，M1 的设置服务启动时补。
			Name: "system_config", Kind: "SystemSettings", KindClass: KindMasterSettings, Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeInt).def("1").check("id = 1"),
				col("proxy_groups_source_url", TypeText).def("''"),
				col("client_compatibility_mode", TypeBool).def("FALSE"),
				col("enable_short_link", TypeBool).def("FALSE").defaultTrue(),
				col("enable_sub_info_nodes", TypeBool).def("FALSE"),
				col("sub_info_v2ray_only", TypeBool).def("FALSE"),
				col("sub_info_expire_prefix", TypeText).def("'📅过期时间'"),
				col("sub_info_traffic_prefix", TypeText).def("'⌛剩余流量'"),
				col("speed_collect_interval", TypeInt).def("3"),
				col("traffic_collect_interval", TypeInt).def("60"),
				col("traffic_check_interval", TypeInt).def("120"),
				col("heartbeat_interval", TypeInt).def("30"),
				col("agent_log_enabled", TypeBool).def("FALSE"),
				col("notify_enabled", TypeBool).def("FALSE"),
				col("telegram_bot_token", TypeText).def("''").cls(ClassHuman).masked(),
				col("telegram_chat_id", TypeText).def("''").cls(ClassMasterSelf),
				col("notify_login", TypeBool).def("FALSE"),
				col("notify_subscribe_fetch", TypeBool).def("FALSE"),
				col("notify_daily_traffic", TypeBool).def("FALSE"),
				col("notify_server_offline", TypeBool).def("FALSE"),
				col("notify_server_online", TypeBool).def("FALSE"),
				col("notify_traffic_threshold", TypeBool).def("FALSE"),
				col("notify_daily_traffic_time", TypeText).def("'08:00'"),
				col("notify_traffic_threshold_percent", TypeInt).def("80"),
				col("notify_traffic_threshold_80", TypeBool).def("FALSE"),
				col("notify_over_limit", TypeBool).def("FALSE"),
				col("notify_package_expiring", TypeBool).def("FALSE"),
				col("notify_package_expiring_days", TypeInt).def("3"),
				col("notify_package_expired", TypeBool).def("FALSE"),
				col("notify_user_registered", TypeBool).def("FALSE"),
				col("notify_telegram_bound", TypeBool).def("FALSE"),
				col("notify_cert_result", TypeBool).def("FALSE"),
				col("notify_agent_long_offline", TypeBool).def("FALSE"),
				col("notify_agent_long_offline_minutes", TypeInt).def("30"),
				col("notify_device_limit_exceeded", TypeBool).def("FALSE"),
				col("notify_server_renewal", TypeBool).def("FALSE"),
				col("notify_ip_ban", TypeBool).def("FALSE"),
				col("enable_override_scripts", TypeBool).def("FALSE"),
				col("subscription_output_format", TypeText).def("'yaml'"),
				col("silent_mode", TypeBool).def("FALSE").cls(ClassHuman),
				col("silent_mode_timeout", TypeInt).def("15").cls(ClassHuman),
				col("enable_miaomiaowu_features", TypeBool).def("FALSE").defaultTrue(),
				col("default_template_filename", TypeText).def("''"),
				col("default_surge_template_filename", TypeText).def("''"),
				col("node_name_multiplier_prefix_enabled", TypeBool).def("FALSE"),
				col("node_name_multiplier_left", TypeText).def("'「'"),
				col("node_name_multiplier_right", TypeText).def("'」'"),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("updated_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"id"},
			// 键值表里的 key 目录（tables_settings.go）：与列一起构成 SystemSettings 这个逻辑对象。
			Settings: settingsKeys(),
		},
		{
			// SystemSettings 单例的 key-value 底表：mmwx 以 key 存的设置项与运行态标记。
			// 每个 key 的分档、类型、打码与是不是运行态由 system_config 表上的 Settings 目录给出（tables_settings.go）。
			Name: "system_settings", GoName: "SystemSettingEntry", Origin: OriginMMWX,
			Columns: []Column{
				col("key", TypeText),
				col("value", TypeText),
				col("updated_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"key"},
		},
	}
}
