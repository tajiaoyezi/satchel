package schema

// 用户簇（附录 A.3「User 改造」）：users 升为 kind，六张附属表照抄；api_tokens 由 user_api_tokens 改造（附录 A.4）。
// mmwx 的列集合来自 internal/storage/traffic.go 的 CREATE TABLE 与后加的 ALTER TABLE。

// fkTo 是引用非 id 列的外键（指向 users 的外键都用 username，照抄 mmwx 的连表方式）。
func fkTo(column, refTable, refColumn, onDelete string) ForeignKey {
	return ForeignKey{Columns: []string{column}, RefTable: refTable, RefColumns: []string{refColumn}, OnDelete: onDelete}
}

// fkUser 指向 users.username：用户物理删除时级联删，改名时级联改（第 05 章七组的管理员改名、附录 B 116 的用户改名）。
func fkUser(column string) ForeignKey {
	f := fkTo(column, "users", "username", "CASCADE")
	f.OnUpdate = "CASCADE"
	return f
}

func userTables() []Table {
	return []Table{
		{
			Name: "users", Kind: "User", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText).immutable(),
				col("email", TypeText).null(),
				col("nickname", TypeText).null(),
				col("avatar_url", TypeText).null(),
				col("role", TypeText).def("'user'").enum("admin", "user"),
				col("remark", TypeText).null(),
				// 五个个人覆盖项：可空或不含 key 表示继承套餐，0 是显式的「不限」（第 10 章）。
				col("traffic_limit_override", TypeInt).null(),
				col("speed_limit_override", TypeFloat).null(),
				col("device_limit_override", TypeInt).null(),
				col("node_speed_limit_overrides", TypeJSON).def("'{}'"),
				col("node_device_limit_overrides", TypeJSON).def("'{}'"),
				col("tg_notify_enabled", TypeBool).def("FALSE"),
				// 动作专属：停用 / 启用用户，绑定 / 解绑 Telegram。
				col("is_active", TypeBool).def("FALSE").defaultTrue().cls(ClassAction),
				col("telegram_id", TypeInt).null().cls(ClassAction),
				col("telegram_username", TypeText).def("''").cls(ClassAction),
				col("telegram_bound_at", TypeTime).null().cls(ClassAction),
				// 人类专属（第 05 章七组「管理员账号」）：密码、两步验证。
				col("password_hash", TypeText).cls(ClassHuman).masked(),
				col("totp_secret", TypeText).def("''").cls(ClassHuman).masked(),
				col("totp_enabled", TypeBool).def("FALSE").cls(ClassHuman),
				col("recovery_codes", TypeJSON).def("'[]'").cls(ClassHuman).masked(),
				// status：enforcer 与周期任务写的标记。
				col("is_over_limit", TypeBool).def("FALSE").cls(ClassStatus),
				col("over_limit_enforced", TypeBool).def("FALSE").cls(ClassStatus),
				col("disabled_access_enforced", TypeBool).def("FALSE").cls(ClassStatus),
				col("traffic_warned_80", TypeBool).def("FALSE").cls(ClassStatus),
				col("last_reset_at", TypeTime).null().cls(ClassStatus),
				col("last_package_id", TypeInt).null().cls(ClassStatus),
				col("last_package_end_date", TypeTime).null().cls(ClassStatus),
			),
			Indexes: []Index{
				naturalKey("users_username_key", "username"),
				{Name: "users_email_idx", Columns: []string{"email"}},
				{Name: "users_telegram_id_key", Columns: []string{"telegram_id"}, Unique: true, Where: "telegram_id IS NOT NULL"},
			},
		},
		{
			// 用户的订阅令牌，一人一行；token 出现在订阅链接里，按第 05 章打码。
			Name: "user_tokens", GoName: "UserToken", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				col("token", TypeText).masked(),
				col("user_short_code", TypeText).def("''"),
				col("custom_user_short_code", TypeText).def("''"),
				col("updated_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"username"},
			Indexes: []Index{
				// 两个短码是订阅短链 /x/<code> 的查找键，非空时唯一（照抄 mmwx 的部分唯一索引）。
				{Name: "user_tokens_user_short_code_key", Columns: []string{"user_short_code"}, Unique: true, Where: "user_short_code <> ''"},
				{Name: "user_tokens_custom_user_short_code_key", Columns: []string{"custom_user_short_code"}, Unique: true, Where: "custom_user_short_code <> ''"},
			},
			ForeignKeys: []ForeignKey{fkUser("username")},
		},
		{
			// 登录会话：mmwx 存明文 token，第 10 章要求只存哈希。
			Name: "sessions", GoName: "Session", Origin: OriginMMWX,
			Columns: []Column{
				col("token_hash", TypeText),
				col("username", TypeText),
				col("expires_at", TypeTime),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"token_hash"},
			Indexes: []Index{
				{Name: "sessions_username_idx", Columns: []string{"username"}},
				{Name: "sessions_expires_at_idx", Columns: []string{"expires_at"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username")},
		},
		{
			// 订阅生成的用户偏好，一人一行，全部照抄。
			Name: "user_settings", GoName: "UserSettings", Origin: OriginMMWX,
			Columns: []Column{
				col("username", TypeText),
				col("force_sync_external", TypeBool).def("FALSE"),
				col("match_rule", TypeText).def("'node_name'"),
				col("cache_expire_minutes", TypeInt).def("0"),
				col("sync_traffic", TypeBool).def("FALSE"),
				col("sync_scope", TypeText).def("'saved_only'"),
				col("keep_node_name", TypeBool).def("FALSE").defaultTrue(),
				col("custom_rules_enabled", TypeBool).def("FALSE"),
				col("enable_short_link", TypeBool).def("FALSE"),
				col("use_new_template_system", TypeBool).def("FALSE").defaultTrue(),
				col("enable_proxy_provider", TypeBool).def("FALSE"),
				col("node_name_filter", TypeText).def("'剩余|流量|到期|订阅|时间|重置'"),
				col("node_order", TypeJSON).def("'[]'"),
				col("default_template_filename", TypeText).def("''"),
				col("append_sub_info", TypeBool).def("FALSE"),
				col("debug_enabled", TypeBool).def("FALSE"),
				col("debug_log_path", TypeText).def("''"),
				col("debug_started_at", TypeTime).null(),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("updated_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey:  []string{"username"},
			ForeignKeys: []ForeignKey{fkUser("username")},
		},
		{
			// 子账号（中转场景），照抄；凭据打码。
			Name: "user_subaccounts", GoName: "UserSubaccount", Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText),
				col("routed_node_id", TypeInt),
				col("email", TypeText),
				col("credential_json", TypeJSON).masked(),
				col("is_active", TypeBool).def("FALSE").defaultTrue(),
			),
			Indexes: []Index{
				{Name: "user_subaccounts_node_user_key", Columns: []string{"routed_node_id", "username"}, Unique: true},
				{Name: "user_subaccounts_node_email_key", Columns: []string{"routed_node_id", "email"}, Unique: true},
				{Name: "user_subaccounts_email_idx", Columns: []string{"email"}},
				{Name: "user_subaccounts_username_idx", Columns: []string{"username"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username")},
		},
		{
			// 用户自行添加的出站 / 私有节点，照抄；出站配置含凭据，打码。
			Name: "user_outbounds", GoName: "UserOutbound", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("username", TypeText),
				col("server_id", TypeInt),
				col("inbound_tag", TypeText),
				col("outbound_tag", TypeText),
				col("outbound_json", TypeJSON).masked(),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			ForeignKeys: []ForeignKey{fkUser("username"), fk("server_id", "servers", "CASCADE")},
		},
		{
			// 用户路由出站的操作记录，配合每日次数限额；只追加。
			Name: "user_routed_outbound_actions", GoName: "UserRoutedOutboundAction", Origin: OriginMMWX, AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("username", TypeText),
				col("action", TypeText),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{{Name: "user_routed_outbound_actions_user_idx", Columns: []string{"username", "created_at"}}},
		},
		{
			// 带权限范围的 API 令牌（第 05 章功能②）。签发、改权限、吊销都是七组人类专属操作，
			// 所以可写列全归人类专属，spec 为空；令牌哈希只用来校验，不出现在任何输出里。
			Name: "api_tokens", Kind: "ApiToken", KindClass: KindAction, Origin: OriginMMWX,
			Columns: withMeta(
				col("owner", TypeText).cls(ClassHuman),
				col("name", TypeText).def("''").cls(ClassHuman),
				col("token_hash", TypeText).cls(ClassHuman).masked(),
				col("scopes", TypeJSON).def("'{}'").cls(ClassHuman),
				col("preset", TypeText).def("'readonly'").enum("readonly", "ops", "full").cls(ClassHuman),
				col("expires_at", TypeTime).null().cls(ClassHuman),
				col("runtime", TypeText).null().cls(ClassHuman),
				col("revoked", TypeBool).def("FALSE").cls(ClassHuman),
				col("revoked_at", TypeTime).null().cls(ClassHuman),
				col("last_used_at", TypeTime).null().cls(ClassStatus),
			),
			Indexes: []Index{
				{Name: "api_tokens_token_hash_key", Columns: []string{"token_hash"}, Unique: true},
				{Name: "api_tokens_owner_idx", Columns: []string{"owner"}},
			},
			ForeignKeys: []ForeignKey{fkUser("owner")},
		},
	}
}
