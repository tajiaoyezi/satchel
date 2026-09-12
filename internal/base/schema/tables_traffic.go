package schema

// 流量账本簇（附录 A.3「流量账本簇照抄」）：21 张表一个字段都不动——列、主键、唯一约束、索引、外键都按 mmwx。
// 类型只做机械映射：INTEGER → TypeInt、REAL → TypeFloat、TIMESTAMP → TypeTime；date 列是 'YYYY-MM-DD' 字符串，
// mmwx 按字符串比较与分组，保持文本。都不是 kind，也不是 append-only（mmwx 全部用 upsert）。
// mmwx 的列集合来自 internal/storage/traffic.go 与 traffic_daily_ledger.go。

func trafficTables() []Table {
	updated := col("updated_at", TypeTime).def("CURRENT_TIMESTAMP")
	created := col("created_at", TypeTime).def("CURRENT_TIMESTAMP")
	date := col("date", TypeText)
	return []Table{
		// ---- 实时账 3 张 + 周期结转 1 张 ----
		{
			Name: "node_traffic", GoName: "NodeTraffic", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				col("tag", TypeText),
				col("type", TypeText).enum("inbound", "outbound"),
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				col("total_uplink", TypeInt).def("0"),
				col("total_downlink", TypeInt).def("0"),
				col("last_uplink", TypeInt).def("0"),
				col("last_downlink", TypeInt).def("0"),
				updated,
			},
			Indexes: []Index{
				{Name: "node_traffic_server_tag_type_key", Columns: []string{"server_id", "tag", "type"}, Unique: true},
				{Name: "node_traffic_server_id_idx", Columns: []string{"server_id"}},
				{Name: "node_traffic_tag_idx", Columns: []string{"tag"}},
				{Name: "node_traffic_type_idx", Columns: []string{"type"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "user_traffic", GoName: "UserTraffic", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				col("username", TypeText),
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				col("total_uplink", TypeInt).def("0"),
				col("total_downlink", TypeInt).def("0"),
				col("last_uplink", TypeInt).def("0"),
				col("last_downlink", TypeInt).def("0"),
				col("cycle_start", TypeTime).def("CURRENT_TIMESTAMP"),
				updated,
			},
			Indexes: []Index{
				{Name: "user_traffic_server_username_key", Columns: []string{"server_id", "username"}, Unique: true},
				{Name: "user_traffic_server_id_idx", Columns: []string{"server_id"}},
				{Name: "user_traffic_username_idx", Columns: []string{"username"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "user_email_traffic", GoName: "UserEmailTraffic", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				col("email", TypeText),
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				col("total_uplink", TypeInt).def("0"),
				col("total_downlink", TypeInt).def("0"),
				col("last_uplink", TypeInt).def("0"),
				col("last_downlink", TypeInt).def("0"),
				col("cycle_base_uplink", TypeInt).def("0"),
				col("cycle_base_downlink", TypeInt).def("0"),
				col("weighted_uplink", TypeFloat).def("0"),
				col("weighted_downlink", TypeFloat).def("0"),
				col("cycle_base_weighted_uplink", TypeFloat).def("0"),
				col("cycle_base_weighted_downlink", TypeFloat).def("0"),
				col("attributed_username", TypeText).def("''"),
				col("cycle_start", TypeTime).def("CURRENT_TIMESTAMP"),
				updated,
			},
			Indexes: []Index{
				{Name: "user_email_traffic_server_email_key", Columns: []string{"server_id", "email"}, Unique: true},
				{Name: "user_email_traffic_server_id_idx", Columns: []string{"server_id"}},
				{Name: "user_email_traffic_email_idx", Columns: []string{"email"}},
				{Name: "user_email_traffic_attributed_username_idx", Columns: []string{"attributed_username"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "user_traffic_cycle_carry", GoName: "UserTrafficCycleCarry", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				col("weighted_uplink", TypeFloat).def("0"),
				col("weighted_downlink", TypeFloat).def("0"),
				updated,
			},
			PrimaryKey: []string{"username"},
		},
		// ---- 快照 5 张 ----
		{
			Name: "node_traffic_snapshots", GoName: "NodeTrafficSnapshot", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				col("tag", TypeText),
				col("type", TypeText).def("'inbound'"),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				created,
			},
			Indexes: []Index{
				{Name: "node_traffic_snapshots_server_tag_type_date_key", Columns: []string{"server_id", "tag", "type", "date"}, Unique: true},
				{Name: "node_traffic_snapshots_date_idx", Columns: []string{"date"}},
			},
		},
		{
			Name: "user_traffic_snapshots", GoName: "UserTrafficSnapshot", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				col("username", TypeText),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				created,
			},
			Indexes: []Index{
				{Name: "user_traffic_snapshots_server_username_date_key", Columns: []string{"server_id", "username", "date"}, Unique: true},
				{Name: "user_traffic_snapshots_date_idx", Columns: []string{"date"}},
			},
		},
		{
			Name: "user_email_traffic_snapshots", GoName: "UserEmailTrafficSnapshot", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				col("email", TypeText),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				created,
			},
			Indexes: []Index{
				{Name: "user_email_traffic_snapshots_server_email_date_key", Columns: []string{"server_id", "email", "date"}, Unique: true},
				{Name: "user_email_traffic_snapshots_date_idx", Columns: []string{"date"}},
			},
		},
		{
			Name: "traffic_snapshots", GoName: "TrafficSnapshot", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				date,
				col("inbound_uplink", TypeInt).def("0"),
				col("inbound_downlink", TypeInt).def("0"),
				col("outbound_uplink", TypeInt).def("0"),
				col("outbound_downlink", TypeInt).def("0"),
				col("user_uplink", TypeInt).def("0"),
				col("user_downlink", TypeInt).def("0"),
				created,
			},
			Indexes: []Index{
				{Name: "traffic_snapshots_server_date_key", Columns: []string{"server_id", "date"}, Unique: true},
				{Name: "traffic_snapshots_server_id_idx", Columns: []string{"server_id"}},
				{Name: "traffic_snapshots_date_idx", Columns: []string{"date"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "server_system_traffic_snapshots", GoName: "ServerSystemTrafficSnapshot", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				date,
				col("rx_cycle", TypeInt).def("0"),
				col("tx_cycle", TypeInt).def("0"),
				created,
			},
			Indexes: []Index{
				{Name: "server_system_traffic_snapshots_server_date_key", Columns: []string{"server_id", "date"}, Unique: true},
				{Name: "server_system_traffic_snapshots_date_idx", Columns: []string{"date"}},
			},
		},
		// ---- 日账 9 张 ----
		{
			Name: "traffic_daily_users", GoName: "TrafficDailyUser", Origin: OriginMMWX,
			Columns: []Column{
				col("server_id", TypeInt),
				col("username", TypeText),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				updated,
			},
			PrimaryKey:  []string{"server_id", "username", "date"},
			Indexes:     []Index{{Name: "traffic_daily_users_date_idx", Columns: []string{"date"}}},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "traffic_daily_users_archived", GoName: "TrafficDailyUserArchived", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				updated,
			},
			PrimaryKey: []string{"username", "date"},
			Indexes:    []Index{{Name: "traffic_daily_users_archived_date_idx", Columns: []string{"date"}}},
		},
		{
			Name: "traffic_daily_user_nodes", GoName: "TrafficDailyUserNode", Origin: OriginMMWX,
			Columns: []Column{
				col("server_id", TypeInt),
				col("node_id", TypeInt),
				col("username", TypeText),
				date,
				col("uplink", TypeFloat).def("0"),
				col("downlink", TypeFloat).def("0"),
				col("weighted_uplink", TypeFloat).def("0"),
				col("weighted_downlink", TypeFloat).def("0"),
				updated,
			},
			PrimaryKey: []string{"server_id", "node_id", "username", "date"},
			Indexes: []Index{
				{Name: "traffic_daily_user_nodes_date_idx", Columns: []string{"date"}},
				{Name: "traffic_daily_user_nodes_user_idx", Columns: []string{"username"}},
				{Name: "traffic_daily_user_nodes_node_idx", Columns: []string{"node_id"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "traffic_daily_user_emails", GoName: "TrafficDailyUserEmail", Origin: OriginMMWX,
			Columns: []Column{
				col("server_id", TypeInt),
				col("email", TypeText),
				col("attributed_username", TypeText).def("''"),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				col("weighted_uplink", TypeFloat).def("0"),
				col("weighted_downlink", TypeFloat).def("0"),
				updated,
			},
			PrimaryKey: []string{"server_id", "email", "attributed_username", "date"},
			Indexes: []Index{
				{Name: "traffic_daily_user_emails_date_idx", Columns: []string{"date"}},
				{Name: "traffic_daily_user_emails_user_idx", Columns: []string{"attributed_username"}},
			},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "traffic_daily_nodes", GoName: "TrafficDailyNode", Origin: OriginMMWX,
			Columns: []Column{
				col("server_id", TypeInt),
				col("tag", TypeText),
				col("type", TypeText),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				updated,
			},
			PrimaryKey: []string{"server_id", "tag", "type", "date"},
			// mmwx 的 idx_traffic_daily_nodes_identity 只是给缺主键的老 PostgreSQL 库补的修复索引，新库主键已覆盖，不录。
			Indexes:     []Index{{Name: "traffic_daily_nodes_date_idx", Columns: []string{"date"}}},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "traffic_daily_system_servers", GoName: "TrafficDailySystemServer", Origin: OriginMMWX,
			Columns: []Column{
				col("server_id", TypeInt),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				updated,
			},
			PrimaryKey:  []string{"server_id", "date"},
			Indexes:     []Index{{Name: "traffic_daily_system_servers_date_idx", Columns: []string{"date"}}},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			Name: "traffic_daily_external_subscriptions", GoName: "TrafficDailyExternalSubscription", Origin: OriginMMWX,
			Columns: []Column{
				col("external_subscription_id", TypeInt),
				date,
				col("uplink", TypeInt).def("0"),
				col("downlink", TypeInt).def("0"),
				updated,
			},
			PrimaryKey:  []string{"external_subscription_id", "date"},
			Indexes:     []Index{{Name: "traffic_daily_external_subscriptions_date_idx", Columns: []string{"date"}}},
			ForeignKeys: []ForeignKey{fk("external_subscription_id", "external_subscriptions", "CASCADE")},
		},
		{
			Name: "traffic_daily_incomplete_dates", GoName: "TrafficDailyIncompleteDate", Origin: OriginMMWX,
			Columns: []Column{
				date,
				col("reason", TypeText),
				created,
			},
			PrimaryKey: []string{"date"},
		},
		{
			Name: "traffic_daily_meta", GoName: "TrafficDailyMeta", Origin: OriginMMWX,
			Columns: []Column{
				col("key", TypeText),
				col("value", TypeText),
				updated,
			},
			PrimaryKey: []string{"key"},
		},
		// ---- 汇总与预警 3 张 ----
		{
			Name: "traffic_records", GoName: "TrafficRecord", Origin: OriginMMWX,
			Columns: []Column{
				date,
				col("total_limit", TypeInt),
				col("total_used", TypeInt),
				col("total_remaining", TypeInt),
				created,
			},
			PrimaryKey: []string{"date"},
		},
		{
			Name: "user_traffic_records", GoName: "UserTrafficRecord", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				date,
				col("total_limit", TypeInt),
				col("total_used", TypeInt),
				col("total_remaining", TypeInt),
				created,
			},
			PrimaryKey: []string{"username", "date"},
		},
		{
			Name: "traffic_threshold_notified", GoName: "TrafficThresholdNotified", Origin: OriginMMWX,
			Columns: []Column{
				col("server_id", TypeInt),
				col("notified_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"server_id"},
		},
	}
}
