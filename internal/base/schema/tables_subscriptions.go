package schema

// 订阅与模板簇（附录 A.3「订阅与模板簇照抄」）：订阅一族归配置类（已拍板），七张升为 kind，四张附属表照抄。
// mmwx 的列集合来自 internal/storage/traffic.go 的 CREATE TABLE、ensure*Column 与单独的 CREATE INDEX。

func subscriptionTables() []Table {
	return []Table{
		{
			// 订阅文件：create / import / upload / package 四种；url 是导入的上游订阅链接（upload、package 为空），按第 05 章打码。
			Name: "subscribe_files", Kind: "SubscribeFile", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("name", TypeText),
				col("description", TypeText).null(),
				col("url", TypeText).masked(),
				col("type", TypeText).enum("create", "import", "upload", "package"),
				col("filename", TypeText),
				col("expire_at", TypeTime).null(),
				col("file_short_code", TypeText).def("''"),
				col("custom_short_code", TypeText).def("''"),
				col("auto_sync_custom_rules", TypeBool).def("FALSE"),
				col("template_filename", TypeText).def("''"),
				col("selected_custom_rule_ids", TypeJSON).def("'[]'"),
				col("selected_override_script_ids", TypeJSON).def("'[]'"),
				col("selected_tags", TypeJSON).def("'[]'"),
				col("selected_node_ids", TypeJSON).def("'[]'"),
				col("stats_server_ids", TypeText).def("''"), // 逗号分隔的 servers.id，照抄
				col("traffic_limit", TypeFloat).null(),
				col("sort_order", TypeInt).def("0"),
				col("raw_output", TypeBool).def("FALSE"),
				col("created_by", TypeText).def("''"),
			),
			Indexes: []Index{
				naturalKey("subscribe_files_name_key", "name"),
				{Name: "subscribe_files_type_idx", Columns: []string{"type"}},
				// 两个短码是订阅短链的查找键，非空时唯一；跨列同一命名空间由 M3 写路径检查（m0-04 design 第 14 条）。
				{Name: "subscribe_files_file_short_code_key", Columns: []string{"file_short_code"}, Unique: true, Where: "file_short_code <> ''"},
				{Name: "subscribe_files_custom_short_code_key", Columns: []string{"custom_short_code"}, Unique: true, Where: "custom_short_code <> ''"},
			},
		},
		{
			// 用户 ↔ 订阅文件，照抄。
			Name: "user_subscriptions", GoName: "UserSubscription", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				col("subscription_id", TypeInt),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"username", "subscription_id"},
			Indexes: []Index{
				{Name: "user_subscriptions_username_idx", Columns: []string{"username"}},
				{Name: "user_subscriptions_subscription_id_idx", Columns: []string{"subscription_id"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username"), fk("subscription_id", "subscribe_files", "CASCADE")},
		},
		{
			// 订阅链接：名称、类型、规则文件、按钮。
			Name: "subscription_links", Kind: "SubscriptionLink", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("name", TypeText),
				col("type", TypeText).def("''"),
				col("description", TypeText).null(),
				col("rule_filename", TypeText),
				col("buttons", TypeJSON).def("'[]'"),
				col("short_url", TypeText).def("''"),
			),
			Indexes: []Index{
				naturalKey("subscription_links_name_key", "name"),
				{Name: "subscription_links_short_url_key", Columns: []string{"short_url"}, Unique: true, Where: "short_url <> ''"},
			},
		},
		{
			// clash / surge 模板。
			Name: "templates", Kind: "Template", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("name", TypeText),
				col("category", TypeText).def("'clash'").enum("clash", "surge"),
				col("template_url", TypeText).def("''"),
				col("rule_source", TypeText).def("''"),
				col("use_proxy", TypeBool).def("FALSE"),
				col("enable_include_all", TypeBool).def("FALSE"),
				col("created_by", TypeText).def("''"),
			),
			Indexes: []Index{
				naturalKey("templates_name_key", "name"),
				{Name: "templates_category_idx", Columns: []string{"category"}},
			},
		},
		{
			// 自定义规则：dns / rules / rule-providers × replace / prepend / append；自然键是 name 加 type。
			Name: "custom_rules", Kind: "CustomRule", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("name", TypeText),
				col("type", TypeText).enum("dns", "rules", "rule-providers"),
				col("mode", TypeText).enum("replace", "prepend", "append"),
				col("content", TypeText),
				col("enabled", TypeBool).def("FALSE"),
				col("created_by", TypeText).def("''"),
			),
			Indexes: []Index{
				naturalKey("custom_rules_name_type_key", "name", "type"),
				{Name: "custom_rules_type_idx", Columns: []string{"type"}},
				{Name: "custom_rules_enabled_idx", Columns: []string{"enabled"}},
			},
		},
		{
			// 规则应用到订阅文件的记录，照抄。
			Name: "custom_rule_applications", GoName: "CustomRuleApplication", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("subscribe_file_id", TypeInt),
				col("custom_rule_id", TypeInt),
				col("rule_type", TypeText),
				col("rule_mode", TypeText),
				col("applied_content", TypeText),
				col("content_hash", TypeText),
				col("applied_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{
				{Name: "custom_rule_applications_file_rule_type_key", Columns: []string{"subscribe_file_id", "custom_rule_id", "rule_type"}, Unique: true},
				{Name: "custom_rule_applications_file_idx", Columns: []string{"subscribe_file_id"}},
				{Name: "custom_rule_applications_rule_idx", Columns: []string{"custom_rule_id"}},
			},
			ForeignKeys: []ForeignKey{fk("subscribe_file_id", "subscribe_files", "CASCADE"), fk("custom_rule_id", "custom_rules", "CASCADE")},
		},
		{
			// 用户保存的路由规则快捷项，照抄；(username, rule_json) 唯一（jsonb 有默认 B 树运算符类）。
			Name: "routing_rule_presets", GoName: "RoutingRulePreset", Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText),
				col("name", TypeText),
				col("rule_json", TypeJSON),
			),
			Indexes: []Index{
				{Name: "routing_rule_presets_username_rule_key", Columns: []string{"username", "rule_json"}, Unique: true},
				{Name: "routing_rule_presets_username_updated_idx", Columns: []string{"username", "updated_at"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username")},
		},
		{
			// 规则文件的版本历史（mmwx 自带的快照先例），只追加。
			Name: "rule_versions", GoName: "RuleVersion", Origin: OriginMMWX, AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("filename", TypeText),
				col("version", TypeInt),
				col("content", TypeText),
				col("created_by", TypeText),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{{Name: "rule_versions_filename_version_key", Columns: []string{"filename", "version"}, Unique: true}},
		},
		{
			// 外部订阅聚合：url 是上游订阅链接，打码；从上游拿回来的用量与到期归 status。
			Name: "external_subscriptions", Kind: "ExternalSubscription", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText),
				col("name", TypeText),
				col("url", TypeText).masked(),
				col("user_agent", TypeText).def("'clash-meta/2.4.0'"),
				col("traffic_mode", TypeText).def("'both'"),
				col("node_count", TypeInt).def("0").cls(ClassStatus),
				col("last_sync_at", TypeTime).null().cls(ClassStatus),
				col("upload", TypeInt).def("0").cls(ClassStatus),
				col("download", TypeInt).def("0").cls(ClassStatus),
				col("total", TypeInt).def("0").cls(ClassStatus),
				col("expire", TypeTime).null().cls(ClassStatus),
			),
			Indexes: []Index{
				// 同一用户不能重复添加同一个链接；不是自然键，撞上报 conflict。
				{Name: "external_subscriptions_username_url_key", Columns: []string{"username", "url"}, Unique: true},
				{Name: "external_subscriptions_username_idx", Columns: []string{"username"}},
				{Name: "external_subscriptions_url_idx", Columns: []string{"url"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username")},
		},
		{
			// 代理集合（proxy provider）输出参数，属某条外部订阅。mmwx 带默认值的列这里都 NOT NULL（mmwx 从不写 NULL）。
			Name: "proxy_provider_configs", Kind: "ProxyProviderConfig", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText),
				col("external_subscription_id", TypeInt),
				col("name", TypeText),
				col("type", TypeText).def("'http'"),
				col("interval", TypeInt).def("3600"),
				col("proxy", TypeText).def("'DIRECT'"),
				col("size_limit", TypeInt).def("0"),
				col("header", TypeText).null(), // YAML 片段
				col("health_check_enabled", TypeBool).def("FALSE"),
				col("health_check_url", TypeText).def("'https://www.gstatic.com/generate_204'"),
				col("health_check_interval", TypeInt).def("300"),
				col("health_check_timeout", TypeInt).def("5000"),
				col("health_check_lazy", TypeBool).def("FALSE"),
				col("health_check_expected_status", TypeInt).def("204"),
				col("filter", TypeText).null(),
				col("exclude_filter", TypeText).null(),
				col("exclude_type", TypeText).null(),
				col("geo_ip_filter", TypeText).null(),
				col("override", TypeText).null(), // YAML 片段
				col("process_mode", TypeText).def("'client'"),
			),
			Indexes: []Index{
				{Name: "proxy_provider_configs_username_idx", Columns: []string{"username"}},
				{Name: "proxy_provider_configs_external_subscription_id_idx", Columns: []string{"external_subscription_id"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username"), fk("external_subscription_id", "external_subscriptions", "CASCADE")},
		},
		{
			// JS 覆写脚本（脚本引擎也进 v1）。
			Name: "override_scripts", Kind: "OverrideScript", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText),
				col("name", TypeText),
				col("hook", TypeText),
				col("content", TypeText),
				col("enabled", TypeBool).def("FALSE"),
				col("sort_order", TypeInt).def("0"),
			),
			Indexes: []Index{
				{Name: "override_scripts_username_idx", Columns: []string{"username"}},
				{Name: "override_scripts_hook_idx", Columns: []string{"hook"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username")},
		},
	}
}
