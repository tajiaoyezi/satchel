package schema

// 节点服务器簇（附录 A.3「Server 改造」与五个新 kind）：remote_servers 改名 servers 并按字段组拆 spec / status，
// Inbound、Outbound、RoutingRule、Website、ReturnRoute 各建表，batch_inbounds、batch_outbounds 照抄。
// 不搬 xray_servers（外部 xray 实例）与 server_xray_config_snapshots（已被 config_snapshots 泛化）。

func fkServer(onDelete string) ForeignKey { return fk("server_id", "servers", onDelete) }

func serverTables() []Table {
	return []Table{
		{
			Name: "servers", Kind: "Server", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				// spec：身份与连接。
				col("name", TypeText),
				col("ipv6_enabled", TypeBool).def("FALSE").defaultTrue(),
				col("domain", TypeText).def("''"),
				col("domain_v6", TypeText).def("''"),
				col("connection_mode", TypeText).def("'auto'").enum("auto", "websocket", "http", "pull"),
				col("pull_address", TypeText).def("''"),
				col("pull_address_v6", TypeText).def("''"),
				col("pull_port", TypeInt).def("0"),
				col("listen_port", TypeInt).def("0"),
				col("lock_entry_ip", TypeBool).def("FALSE"),
				// spec：伪装与端口。
				col("use_443", TypeBool).def("FALSE"),
				col("steal_mode", TypeText).def("'tunnel'").enum("tunnel", "fallback"),
				col("site_type", TypeText).def("''"),
				col("site_value", TypeText).def("''"),
				col("port_range_min", TypeInt).def("0"),
				col("port_range_max", TypeInt).def("0"),
				// spec：流量计费。traffic_calibration 是人工校准，从 mmwx 的 traffic_used_offset 拆出（附录 A.3）。
				col("traffic_limit", TypeInt).def("0"),
				col("traffic_reset_day", TypeInt).def("0"),
				col("traffic_stats_mode", TypeText).def("'both'").enum("both", "upload", "download", "max"),
				// 口径：core（mmwx 叫 xray）是内核统计，system 是系统网卡。默认 system 照 mmwx 创建路径的行为（列默认虽是 xray，不填就落 system）。
				col("traffic_source", TypeText).def("'system'").enum("core", "system"),
				col("include_in_traffic_stats", TypeBool).def("FALSE").defaultTrue(),
				col("traffic_calibration", TypeInt).def("0"),
				// spec：商务信息。
				col("region", TypeText).def("''"),
				col("region_country", TypeText).def("''"),
				col("region_name", TypeText).def("''"),
				col("region_city", TypeText).def("''"),
				col("renewal_price", TypeFloat).def("0"),
				col("renewal_cycle", TypeText).def("'month'"),
				col("renewal_currency", TypeText).def("'CNY'"),
				col("provider_name", TypeText).def("''"),
				col("provider_url", TypeText).def("''"),
				col("telecom_paid_peer", TypeBool).def("FALSE"),
				col("expires_at", TypeTime).null(),
				// spec：DDNS。记录名单独存，DDNS 不许写主控域名与订阅域名；服务商为 NULL 表示没选（取代 mmwx 的 0）。
				col("ddns_enabled", TypeBool).def("FALSE"),
				col("ddns_provider_id", TypeInt).null(),
				col("ddns_record_name", TypeText).def("''"),
				// spec：内核全局字段组（第 07 章），每台一份。
				col("core_log_level", TypeText).def("'warn'"),
				col("core_dns", TypeJSON).def("'{}'"),
				col("core_stats_enabled", TypeBool).def("FALSE"),
				col("sort_order", TypeInt).def("0"),
				// 动作专属：三把令牌与它们的到期时间，只由轮换 / 重置动作写，打码，不进快照。
				col("token", TypeText).cls(ClassAction).masked(),
				col("agent_token", TypeText).def("''").cls(ClassAction).masked(),
				col("pull_token", TypeText).def("''").cls(ClassAction).masked(),
				col("token_expires_at", TypeTime).null().cls(ClassAction),
				col("agent_token_expires_at", TypeTime).null().cls(ClassAction),
				col("last_token_refresh", TypeTime).null().cls(ClassAction),
				col("last_agent_token_refresh", TypeTime).null().cls(ClassAction),
				// status：运行状态，只由心跳、对账、内置任务写。
				col("status", TypeText).def("'pending'").enum("pending", "connected", "offline").cls(ClassStatus),
				col("last_heartbeat", TypeTime).null().cls(ClassStatus),
				col("ip_address", TypeText).null().cls(ClassStatus),
				col("ip_address_v6", TypeText).def("''").cls(ClassStatus),
				col("boot_time", TypeTime).null().cls(ClassStatus),
				col("boot_count", TypeInt).def("0").cls(ClassStatus),
				col("core_boot_time", TypeTime).null().cls(ClassStatus),
				col("core_boot_count", TypeInt).def("0").cls(ClassStatus),
				col("core_running", TypeBool).def("FALSE").cls(ClassStatus),
				col("core_version", TypeText).def("''").cls(ClassStatus),
				col("current_upload_speed", TypeInt).def("0").cls(ClassStatus),
				col("current_download_speed", TypeInt).def("0").cls(ClassStatus),
				col("speed_updated_at", TypeTime).null().cls(ClassStatus),
				col("offline_since", TypeTime).null().cls(ClassStatus),
				col("offline_notified", TypeBool).def("FALSE").cls(ClassStatus),
				col("warp_installed", TypeBool).def("FALSE").cls(ClassStatus),
				col("same_host_as_master", TypeBool).def("FALSE").cls(ClassStatus),
				col("time_offset_seconds", TypeInt).null().cls(ClassStatus),
				col("push_fail_count", TypeInt).def("0").cls(ClassStatus),
				col("last_push_fail", TypeTime).null().cls(ClassStatus),
				col("fallback_to_pull", TypeBool).def("FALSE").cls(ClassStatus),
				col("fallback_at", TypeTime).null().cls(ClassStatus),
				col("last_pull_at", TypeTime).null().cls(ClassStatus),
				// status：系统网卡口径的累计与基线。
				col("system_rx_cycle", TypeInt).def("0").cls(ClassStatus),
				col("system_tx_cycle", TypeInt).def("0").cls(ClassStatus),
				col("system_last_seen_rx", TypeInt).def("0").cls(ClassStatus),
				col("system_last_seen_tx", TypeInt).def("0").cls(ClassStatus),
				col("system_boot_time_unix", TypeInt).def("0").cls(ClassStatus),
				col("system_traffic_updated_at", TypeTime).null().cls(ClassStatus),
				// status：重置基线由「重置流量」动作与周期结转写，与人工校准分开（附录 A.3）。
				col("traffic_reset_baseline", TypeInt).def("0").cls(ClassStatus),
				col("last_traffic_reset_at", TypeTime).null().cls(ClassStatus),
				// status：DDNS 运行态。
				col("ddns_last_synced_at", TypeTime).null().cls(ClassStatus),
				col("ddns_last_error", TypeText).def("''").cls(ClassStatus),
				col("ddns_pending", TypeBool).def("FALSE").cls(ClassStatus),
				col("provider_updated_at", TypeTime).null().cls(ClassStatus),
				// status：Satchel 新增。轮换与吊销（第 06 章）、已生效配置（第 07 章）。
				col("rotation_pending", TypeBool).def("FALSE").cls(ClassStatus),
				col("last_rotated_at", TypeTime).null().cls(ClassStatus),
				col("revoke_pending", TypeBool).def("FALSE").cls(ClassStatus),
				col("applied_hash", TypeText).def("''").cls(ClassStatus),
				col("applied_generation", TypeInt).def("0").cls(ClassStatus),
				// 导入的首次 apply 资格（第 10 章导入段、第 13 章 M0）：mmwx 导入工具置 true，apply 只在节点回执成功时消费；
				// 失败保留，结果未知时按节点报回的 hash 走。不用「applied_hash 为空」编码它：导入的节点上本来就有旧配置。
				col("first_apply_eligible", TypeBool).def("FALSE").cls(ClassStatus),
			),
			Indexes: []Index{
				naturalKey("servers_name_key", "name"),
				// 认证按令牌查，唯一但不是名字。
				{Name: "servers_token_key", Columns: []string{"token"}, Unique: true},
				{Name: "servers_status_idx", Columns: []string{"status"}},
			},
			ForeignKeys: []ForeignKey{fk("ddns_provider_id", "dns_providers", "SET NULL")},
		},
		{
			// 入站升为一等公民：spec 存主控，由主控渲染成 sing-box 配置下发；用户凭证由 Assignment 渲染注入。
			Name: "inbounds", Kind: "Inbound", KindClass: KindConfig, Origin: OriginSatchel,
			Columns: withMeta(
				col("server_id", TypeInt),
				col("tag", TypeText),
				col("protocol", TypeText),
				col("port", TypeInt),
				col("listen", TypeText).def("''"),
				col("tls", TypeJSON).def("'{}'").masked(), // Reality 私钥、证书 PEM
				col("transport", TypeJSON).def("'{}'"),
				col("settings", TypeJSON).def("'{}'").masked(), // PSK、协议专属密钥
				col("enabled", TypeBool).def("FALSE"),
				col("sort_order", TypeInt).def("0"),
			),
			// 同一服务器下的 tag 是自然键（跨服务器同名合法），撞上报 name_taken 点名两列；metadata.name 是 server_id/tag。
			Indexes:     []Index{naturalKey("inbounds_server_tag_key", "server_id", "tag")},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "outbounds", Kind: "Outbound", KindClass: KindConfig, Origin: OriginSatchel,
			Columns: withMeta(
				col("server_id", TypeInt),
				col("tag", TypeText),
				col("protocol", TypeText),
				col("settings", TypeJSON).def("'{}'").masked(),
				col("is_warp", TypeBool).def("FALSE"),
				col("balancer_group", TypeText).def("''"),
				col("probe", TypeJSON).def("'{}'"),
				col("last_probe_ok", TypeBool).def("FALSE").cls(ClassStatus),
				col("last_probe_latency_ms", TypeInt).null().cls(ClassStatus),
				col("last_probed_at", TypeTime).null().cls(ClassStatus),
			),
			Indexes:     []Index{naturalKey("outbounds_server_tag_key", "server_id", "tag")},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "routing_rules", Kind: "RoutingRule", KindClass: KindConfig, Origin: OriginSatchel,
			Columns: withMeta(
				col("server_id", TypeInt),
				col("sort_order", TypeInt).def("0"),
				col("match", TypeJSON).def("'{}'"),
				col("outbound_tag", TypeText),
				col("enabled", TypeBool).def("FALSE"),
				col("catch_all", TypeBool).def("FALSE"),
			),
			Indexes:     []Index{{Name: "routing_rules_server_order_idx", Columns: []string{"server_id", "sort_order"}}},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			// nginx 站点：权威在 spec，扫盘结果只进 status（附录 A.3 的换实现）。
			Name: "websites", Kind: "Website", KindClass: KindConfig, Origin: OriginSatchel,
			Columns: withMeta(
				col("server_id", TypeInt),
				col("domain", TypeText),
				col("type", TypeText).enum("static", "proxy", "decoy"),
				col("target", TypeText).def("''"),
				col("certificate_id", TypeInt).null(),
				col("use_443", TypeBool).def("FALSE"),
				col("scanned", TypeJSON).def("'{}'").cls(ClassStatus),
				col("managed", TypeBool).def("FALSE").cls(ClassStatus),
				col("conf_path", TypeText).def("''").cls(ClassStatus),
			),
			Indexes:     []Index{naturalKey("websites_server_domain_key", "server_id", "domain")},
			ForeignKeys: []ForeignKey{fkServer("CASCADE"), fk("certificate_id", "certificates", "SET NULL")},
		},
		{
			// 返程路由：由 mmwx 的 server_return_routes 升成 kind，按运营商探测入口线路；探测结果归 status。
			Name: "return_routes", Kind: "ReturnRoute", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("server_id", TypeInt),
				col("carrier", TypeText),
				col("enabled", TypeBool).def("FALSE"),
				col("probe", TypeJSON).def("'{}'"),
				col("route_type", TypeText).def("'Unknown'").cls(ClassStatus),
				col("region", TypeText).def("''").cls(ClassStatus),
				col("entry_ip", TypeText).def("''").cls(ClassStatus),
				col("entry_asn", TypeText).def("''").cls(ClassStatus),
				col("reason", TypeText).def("''").cls(ClassStatus),
				col("tested_at", TypeTime).null().cls(ClassStatus),
			),
			Indexes: []Index{
				naturalKey("return_routes_server_carrier_key", "server_id", "carrier"),
				{Name: "return_routes_tested_at_idx", Columns: []string{"tested_at"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			// 批量操作追踪，照抄。
			Name: "batch_inbounds", GoName: "BatchInbound", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("batch_id", TypeText),
				col("tag", TypeText),
				col("server_id", TypeInt),
				col("protocol", TypeText),
				col("port", TypeInt),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{
				{Name: "batch_inbounds_batch_idx", Columns: []string{"batch_id"}},
				{Name: "batch_inbounds_server_idx", Columns: []string{"server_id"}},
				{Name: "batch_inbounds_tag_idx", Columns: []string{"tag"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "batch_outbounds", GoName: "BatchOutbound", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("batch_id", TypeText),
				col("tag", TypeText),
				col("server_id", TypeInt),
				col("protocol", TypeText),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{
				{Name: "batch_outbounds_batch_idx", Columns: []string{"batch_id"}},
				{Name: "batch_outbounds_server_idx", Columns: []string{"server_id"}},
				{Name: "batch_outbounds_tag_idx", Columns: []string{"tag"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
	}
}
