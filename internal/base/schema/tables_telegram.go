package schema

// Telegram 簇（附录 A.2「Telegram 全家 照抄」）与外部测速员：五张附属表，都不是 kind。
// mmwx 的列集合来自 internal/storage/traffic.go。

func telegramTables() []Table {
	return []Table{
		{
			// TG 机器人操作审计，只追加。
			Name: "tg_audit", GoName: "TgAudit", Origin: OriginMMWX, AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("tg_id", TypeInt).null(),
				col("username", TypeText).def("''"),
				col("action", TypeText),
				col("detail", TypeText).def("''"),
				col("at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{
				{Name: "tg_audit_tg_id_idx", Columns: []string{"tg_id"}},
				{Name: "tg_audit_username_idx", Columns: []string{"username"}},
				{Name: "tg_audit_at_idx", Columns: []string{"at"}},
			},
		},
		{
			// 邀请码 / 兑换码，code 主键；package_id 是记录，不加外键。
			Name: "invite_codes", GoName: "InviteCode", Origin: OriginMMWX,
			Columns: []Column{
				col("code", TypeText),
				col("kind", TypeText).enum("new", "bind"),
				col("bind_username", TypeText).def("''"),
				col("created_by", TypeText),
				col("package_id", TypeInt).null(),
				col("max_uses", TypeInt).def("1"),
				col("used_count", TypeInt).def("0"),
				col("expires_at", TypeTime).null(),
				col("revoked", TypeBool).def("FALSE"),
				col("remark", TypeText).def("''"),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("duration_months", TypeInt).def("0"),
			},
			PrimaryKey: []string{"code"},
			Indexes: []Index{
				{Name: "invite_codes_created_by_idx", Columns: []string{"created_by"}},
				{Name: "invite_codes_kind_idx", Columns: []string{"kind"}},
			},
		},
		{
			// 邀请码使用记录，只追加。
			Name: "invite_code_uses", GoName: "InviteCodeUse", Origin: OriginMMWX, AppendOnly: true,
			Columns: []Column{
				col("code", TypeText),
				col("username", TypeText),
				col("tg_id", TypeInt).null(),
				col("used_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey: []string{"code", "username"},
			Indexes:    []Index{{Name: "invite_code_uses_username_idx", Columns: []string{"username"}}},
		},
		{
			// 续费申请（用户发起、管理员审批）。request_token 是审批链接里的令牌、passphrase 是用户提交的口令，都打码。
			// 申请属于用户，随用户物理删除一起删（留着会让同名新用户撞上「待审每人一条」的部分唯一索引）。
			// 新增 assignment_id：Satchel 一个用户多份套餐分配，申请要指明续哪一份；分配被物理删除后申请留着、目标置空，审批流按失效处理。
			Name: "renewal_requests", GoName: "RenewalRequest", Origin: OriginMMWX,
			Columns: withMeta(
				col("request_token", TypeText).masked(),
				col("username", TypeText),
				col("telegram_id", TypeInt).def("0"),
				col("package_id", TypeInt),
				col("assignment_id", TypeInt).null(),
				col("package_name", TypeText).def("''"),
				col("previous_end_date", TypeTime).null(),
				col("renew_days", TypeInt),
				col("passphrase", TypeText).masked(),
				col("source", TypeText).def("'web'"),
				col("status", TypeText).def("'pending'"),
				col("reviewed_by", TypeInt).def("0"),
				col("reviewed_at", TypeTime).null(),
				col("new_end_date", TypeTime).null(),
				col("error_message", TypeText).def("''"),
			),
			Indexes: []Index{
				{Name: "renewal_requests_request_token_key", Columns: []string{"request_token"}, Unique: true},
				{Name: "renewal_requests_user_created_idx", Columns: []string{"username", "created_at"}},
				// 同一用户同时只能有一条待审的申请，照抄 mmwx 的部分唯一索引。
				{Name: "renewal_requests_pending_user_key", Columns: []string{"username"}, Unique: true, Where: "status IN ('pending', 'processing')"},
			},
			ForeignKeys: []ForeignKey{fkUser("username"), fk("assignment_id", "package_assignments", "SET NULL")},
		},
		{
			// 外部测速员：token_hash 是配对令牌，打码；caps 是逗号分隔文本，last_seen 与 version 由测速端上报。
			Name: "speed_testers", GoName: "SpeedTester", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("name", TypeText).def("''"),
				col("token_hash", TypeText).masked(),
				col("created_by", TypeText).def("''"),
				col("last_seen", TypeTime).null(),
				col("caps", TypeText).def("''"),
				col("version", TypeText).def("''"),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			Indexes: []Index{{Name: "speed_testers_token_hash_key", Columns: []string{"token_hash"}, Unique: true}},
		},
	}
}
