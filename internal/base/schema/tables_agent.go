package schema

// 12 张 agent-native 表：Satchel 新增、mmwx 没有对应物的表（附录 A.4、第 05 章、第 10 章、第 13 章 M0）。

// 列定义的简写：分档不写默认 spec，公共列按名字自动归 meta。
func col(name string, t Type) Column { return Column{Name: name, Type: t} }

func (c Column) null() Column             { c.Nullable = true; return c }
func (c Column) def(d string) Column      { c.Default = d; return c }
func (c Column) cls(k Class) Column       { c.Class = k; return c }
func (c Column) enum(v ...string) Column  { c.Enum = v; return c }
func (c Column) check(expr string) Column { c.Check = expr; return c }
func (c Column) masked() Column           { c.Masked = true; return c }
func (c Column) immutable() Column        { c.Immutable = true; return c }

// naturalKey 是 kind 自然键（用户起的名字）的唯一索引：撞上它报 name_taken。
// job_id、delivery_id 这类系统生成的幂等 id 不是名字，撞上报 conflict。
func naturalKey(name string, columns ...string) Index {
	return Index{Name: name, Columns: columns, Unique: true, NaturalKey: true}
}

// withMeta 在前面加 id、后面加 created_at 与 updated_at。
func withMeta(cols ...Column) []Column {
	out := []Column{col("id", TypeSerial)}
	out = append(out, cols...)
	return append(out,
		col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
		col("updated_at", TypeTime).def("CURRENT_TIMESTAMP"),
	)
}

func fk(column, refTable, onDelete string) ForeignKey {
	return ForeignKey{Columns: []string{column}, RefTable: refTable, RefColumns: []string{"id"}, OnDelete: onDelete}
}

// AutomationActions 是第 05 章的自动化动作白名单：停用用户、断开连接、临时限速、订阅摘挂节点、发通知、写待办。
var AutomationActions = []string{"disable_user", "disconnect", "throttle", "detach_node", "notify", "write_task"}

func agentNativeTables() []Table {
	return []Table{
		{
			Name: "tasks", Kind: "Task", KindClass: KindAction,
			Columns: withMeta(
				col("title", TypeText),
				col("body", TypeText).null(),
				col("source", TypeText).enum("system", "admin"),
				col("dedup_key", TypeText).def("''"),
				col("alert_id", TypeInt).null(),
				col("evidence_id", TypeInt).null(),
				col("status", TypeText).def("'open'").enum("open", "claimed", "done", "cancelled").cls(ClassAction),
				col("claimed_by", TypeInt).null().cls(ClassAction),
				col("lease_until", TypeTime).null().cls(ClassAction),
				col("cancel_reason", TypeText).null().cls(ClassStatus),
				col("expires_at", TypeTime).null().cls(ClassStatus),
			),
			Indexes: []Index{
				{Name: "tasks_dedup_key_active", Columns: []string{"dedup_key"}, Unique: true,
					Where: "status IN ('open', 'claimed') AND dedup_key <> ''"},
			},
			ForeignKeys: []ForeignKey{
				fk("alert_id", "alerts", "SET NULL"),
				fk("evidence_id", "evidence_packages", "SET NULL"),
				fk("claimed_by", "api_tokens", "SET NULL"),
			},
		},
		{
			Name: "alerts", Kind: "Alert", KindClass: KindAction,
			Columns: withMeta(
				col("category", TypeText),
				col("level", TypeText),
				col("object_kind", TypeText),
				col("object_id", TypeText),
				col("dedup_key", TypeText).check("dedup_key <> ''"),
				col("conditions", TypeJSON).def("'[]'").cls(ClassStatus),
				col("evidence_id", TypeInt).null().cls(ClassStatus),
				col("status", TypeText).def("'open'").enum("open", "claimed", "recovered", "resolved").cls(ClassStatus),
				col("resolve_reason", TypeText).null().cls(ClassStatus),
				col("occurrence_count", TypeInt).def("1").cls(ClassStatus),
				col("first_seen_at", TypeTime).def("CURRENT_TIMESTAMP").cls(ClassStatus),
				col("last_seen_at", TypeTime).def("CURRENT_TIMESTAMP").cls(ClassStatus),
			),
			Indexes: []Index{
				{Name: "alerts_dedup_key_active", Columns: []string{"dedup_key"}, Unique: true, Where: "status <> 'resolved'"},
			},
			ForeignKeys: []ForeignKey{fk("evidence_id", "evidence_packages", "SET NULL")},
		},
		{
			Name: "evidence_packages", Kind: "EvidencePackage", KindClass: KindSystem,
			Columns: withMeta(
				col("category", TypeText),
				col("server_id", TypeInt).null(),
				col("collected_at", TypeTime).def("CURRENT_TIMESTAMP").cls(ClassStatus),
				col("items", TypeJSON).def("'{}'").cls(ClassStatus),
				col("collect_error", TypeText).null().cls(ClassStatus),
				col("size_bytes", TypeInt).def("0").cls(ClassStatus),
				col("expires_at", TypeTime).null().cls(ClassStatus),
			),
			ForeignKeys: []ForeignKey{fk("server_id", "servers", "SET NULL")},
		},
		{
			Name: "automation_rules", Kind: "AutomationRule", KindClass: KindConfig,
			Columns: withMeta(
				col("name", TypeText),
				col("trigger", TypeText),
				col("condition", TypeJSON).def("'{}'"),
				col("action", TypeText).enum(AutomationActions...),
				col("max_targets", TypeInt).null(),
				col("deadline_minutes", TypeInt).null(),
				col("enabled", TypeBool).def("FALSE"),
				col("proposed_by", TypeText).cls(ClassAction),
				col("status", TypeText).def("'pending'").enum("pending", "approved").cls(ClassHuman),
				col("approved_by", TypeText).null().cls(ClassHuman),
				col("approved_at", TypeTime).null().cls(ClassHuman),
			),
			Indexes: []Index{naturalKey("automation_rules_name_key", "name")},
		},
		{
			Name: "audit_logs", Kind: "AuditLog", KindClass: KindSystem, AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("actor", TypeText),
				col("actor_kind", TypeText),
				col("token_id", TypeInt).null(),
				col("command", TypeText),
				col("args_digest", TypeText),
				col("full_command", TypeText).null(),
				col("output", TypeText).null(),
				col("plan_id", TypeInt).null(),
				col("result", TypeText),
			},
		},
		{
			Name: "config_snapshots", Kind: "ConfigSnapshot", KindClass: KindSystem, AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("object_kind", TypeText),
				col("object_id", TypeInt),
				col("object_version", TypeInt),
				col("content", TypeJSON),
				col("content_hash", TypeText),
				col("source", TypeText),
				col("status", TypeText),
				col("apply_id", TypeText).null(),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
		},
		{
			Name: "notify_channels", Kind: "NotifyChannel", KindClass: KindConfig,
			Columns: withMeta(
				col("name", TypeText),
				col("type", TypeText).enum("telegram", "webhook"),
				col("events", TypeJSON).def("'[]'"),
				col("enabled", TypeBool).def("FALSE"),
				col("target", TypeText).def("''").cls(ClassMasterSelf),
				col("secret", TypeText).def("''").cls(ClassMasterSelf).masked(),
				col("last_delivered_at", TypeTime).null().cls(ClassStatus),
				col("last_error", TypeText).null().cls(ClassStatus),
			),
			Indexes: []Index{naturalKey("notify_channels_name_key", "name")},
		},
		{
			Name: "notify_deliveries", Kind: "NotifyDelivery", KindClass: KindSystem,
			Columns: withMeta(
				col("delivery_id", TypeText),
				col("channel_id", TypeInt),
				col("type", TypeText).enum("alerts", "tasks"),
				col("payload", TypeJSON),
				col("attempts", TypeInt).def("0").cls(ClassStatus),
				col("status", TypeText).def("'pending'").enum("pending", "sent", "failed", "void").cls(ClassStatus),
				col("last_error", TypeText).null().cls(ClassStatus),
				col("last_attempt_at", TypeTime).null().cls(ClassStatus),
			),
			Indexes:     []Index{{Name: "notify_deliveries_delivery_id_key", Columns: []string{"delivery_id"}, Unique: true}},
			ForeignKeys: []ForeignKey{fk("channel_id", "notify_channels", "")},
		},
		{
			Name: "alert_deliveries", GoName: "AlertDelivery",
			Columns: []Column{
				col("alert_id", TypeInt),
				col("delivery_id", TypeInt),
			},
			PrimaryKey: []string{"alert_id", "delivery_id"},
			ForeignKeys: []ForeignKey{
				fk("alert_id", "alerts", "CASCADE"),
				fk("delivery_id", "notify_deliveries", "CASCADE"),
			},
		},
		{
			Name: "jobs", Kind: "Job", KindClass: KindAction,
			Columns: withMeta(
				col("job_id", TypeText),
				col("kind", TypeText),
				col("server_id", TypeInt).null(),
				col("args", TypeJSON).def("'{}'"),
				col("status", TypeText).def("'queued'").enum("queued", "running", "done", "failed", "unknown").cls(ClassStatus),
				col("started_at", TypeTime).null().cls(ClassStatus),
				col("finished_at", TypeTime).null().cls(ClassStatus),
				col("exit_code", TypeInt).null().cls(ClassStatus),
				col("output", TypeText).null().cls(ClassStatus),
				col("output_truncated", TypeBool).def("FALSE").cls(ClassStatus),
			),
			Indexes:     []Index{{Name: "jobs_job_id_key", Columns: []string{"job_id"}, Unique: true}},
			ForeignKeys: []ForeignKey{fk("server_id", "servers", "SET NULL")},
		},
		{
			Name: "batch_op_counters", GoName: "BatchOpCounter", AppendOnly: true,
			Columns: []Column{
				col("id", TypeSerial),
				col("token_id", TypeInt).null(),
				col("actor", TypeText),
				col("at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("objects", TypeInt),
				col("command", TypeText),
			},
		},
		{
			Name: "plans", Kind: "Plan", KindClass: KindAction,
			Columns: withMeta(
				col("objects", TypeJSON),
				col("diff", TypeJSON),
				col("affected_count", TypeInt),
				col("share_identity", TypeText).null(),
				col("share_scope", TypeJSON).null(),
				col("expires_at", TypeTime),
				col("status", TypeText).def("'pending'").enum("pending", "applied", "expired", "rejected").cls(ClassStatus),
				col("apply_id", TypeText).null().cls(ClassStatus),
			),
		},
	}
}
