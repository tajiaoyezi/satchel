package schema

// 套餐与分配簇（附录 A.3「Package 照抄」「PackageAssignment 照抄」）：两个 kind 加五张附属表。

func packageTables() []Table {
	return []Table{
		{
			// 套餐定义：mmwx 商业化打磨最充分的部分，全部列是 spec。
			Name: "packages", Kind: "Package", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("name", TypeText),
				col("description", TypeText).null(),
				col("traffic_limit_bytes", TypeInt).def("0"),
				col("cycle_days", TypeInt).def("30"),
				col("is_reset", TypeBool).def("FALSE"),
				col("reset_day", TypeInt).def("1"),
				col("nodes", TypeJSON).def("'[]'"),
				col("device_limit", TypeInt).def("0"),
				col("speed_limit_mbps", TypeFloat).def("0"),
				col("auto_speed_limit_json", TypeJSON).def("'[]'"), // mmwx 用空字符串表示没有规则，JSON 列用 []
				col("traffic_mode", TypeText).def("'oneway'").enum("oneway", "twoway"),
				col("template_filename", TypeText).def("''"),
				col("surge_template_filename", TypeText).def("''"),
				col("short_code", TypeText).def("''"),
				// 按节点细化的四个 JSON：不含 key 即继承，0 是显式的「不限」（第 10 章）。
				col("node_multipliers", TypeJSON).def("'{}'"),
				col("node_traffic_limits", TypeJSON).def("'{}'"),
				col("node_speed_limits", TypeJSON).def("'{}'"),
				col("node_device_limits", TypeJSON).def("'{}'"),
				col("node_name_overrides", TypeJSON).def("'{}'"),
				col("node_name_override_enabled", TypeBool).def("FALSE"),
			),
			Indexes: []Index{naturalKey("packages_name_key", "name")},
		},
		{
			// 用户 × 套餐的一次订购（mmwx 的 user_package_assignments）。配额闭环全长在这张表上。
			Name: "package_assignments", Kind: "PackageAssignment", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText),
				col("package_id", TypeInt),
				col("package_start_date", TypeTime).null(),
				col("package_end_date", TypeTime).null(),
				col("is_reset", TypeBool).def("FALSE"),
				col("reset_day", TypeInt).def("1"),
				col("traffic_limit_override", TypeInt).null(),
				col("is_primary", TypeBool).def("FALSE"),
				col("short_code", TypeText),
				// status：预警、停机、周期重置都由系统写。
				col("status", TypeText).def("'active'").cls(ClassStatus),
				col("last_reset_at", TypeTime).null().cls(ClassStatus),
				col("traffic_warned_80", TypeBool).def("FALSE").cls(ClassStatus),
				col("over_limit_enforced", TypeBool).def("FALSE").cls(ClassStatus),
			),
			Indexes: []Index{
				// 短码是订阅链接用的，撞上报 conflict，不是名字。
				{Name: "package_assignments_short_code_key", Columns: []string{"short_code"}, Unique: true},
				{Name: "package_assignments_user_status_idx", Columns: []string{"username", "status"}},
				{Name: "package_assignments_package_status_idx", Columns: []string{"package_id", "status"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username"), fk("package_id", "packages", "CASCADE")},
		},
		{
			// 这次分配在每台服务器每个入站上的凭证：订阅渲染的原料。
			Name: "package_assignment_inbound_configs", GoName: "PackageAssignmentInboundConfig", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("assignment_id", TypeInt),
				col("username", TypeText),
				col("server_id", TypeInt),
				col("inbound_tag", TypeText),
				col("protocol", TypeText),
				col("email", TypeText),
				col("credential_json", TypeJSON).masked(),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{
				{Name: "package_assignment_inbound_configs_key", Columns: []string{"assignment_id", "server_id", "inbound_tag"}, Unique: true},
				{Name: "package_assignment_inbound_configs_server_idx", Columns: []string{"server_id"}},
				{Name: "package_assignment_inbound_configs_username_idx", Columns: []string{"username"}},
			},
			ForeignKeys: []ForeignKey{
				fk("assignment_id", "package_assignments", "CASCADE"),
				fkUser("username"),
				fk("server_id", "servers", "CASCADE"),
			},
		},
		{
			Name: "package_assignment_subaccounts", GoName: "PackageAssignmentSubaccount", Origin: OriginMMWX,
			Columns: withMeta(
				col("assignment_id", TypeInt),
				col("username", TypeText),
				col("routed_node_id", TypeInt),
				col("email", TypeText),
				col("credential_json", TypeJSON).masked(),
				col("is_active", TypeBool).def("FALSE").defaultTrue(),
			),
			Indexes: []Index{
				{Name: "package_assignment_subaccounts_assignment_node_key", Columns: []string{"assignment_id", "routed_node_id"}, Unique: true},
				{Name: "package_assignment_subaccounts_node_email_key", Columns: []string{"routed_node_id", "email"}, Unique: true},
				{Name: "package_assignment_subaccounts_email_idx", Columns: []string{"email"}},
				{Name: "package_assignment_subaccounts_username_idx", Columns: []string{"username"}},
			},
			ForeignKeys: []ForeignKey{fk("assignment_id", "package_assignments", "CASCADE"), fkUser("username")},
		},
		{
			// 按节点流量基线：复合主键，照抄。
			Name: "package_user_node_traffic_baselines", GoName: "PackageUserNodeTrafficBaseline", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				col("package_id", TypeInt),
				col("node_id", TypeInt),
				col("baseline", TypeFloat).def("0"),
				col("updated_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"username", "package_id", "node_id"},
		},
		{
			// 按节点停用凭证的执行痕迹：复合主键，照抄。
			Name: "package_node_traffic_suspensions", GoName: "PackageNodeTrafficSuspension", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				col("package_id", TypeInt),
				col("node_id", TypeInt),
				col("kind", TypeText),
				col("credential_json", TypeJSON).def("'{}'").masked(),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"username", "package_id", "node_id", "kind"},
		},
		{
			// 每用户在每台服务器每个入站的凭据本体。
			Name: "user_inbound_configs", GoName: "UserInboundConfig", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("username", TypeText),
				col("server_id", TypeInt),
				col("inbound_tag", TypeText),
				col("protocol", TypeText),
				col("credential_json", TypeJSON).masked(),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			// 同用户同入站只能有一份凭据：mmwx 靠这条唯一索引让并发绑套餐的 ON CONFLICT DO NOTHING 生效。
			Indexes:     []Index{{Name: "user_inbound_configs_key", Columns: []string{"username", "server_id", "inbound_tag"}, Unique: true}},
			ForeignKeys: []ForeignKey{fkUser("username"), fk("server_id", "servers", "CASCADE")},
		},
	}
}
