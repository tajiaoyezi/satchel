package schema

// 订阅节点簇（附录 A.3「Node 照抄」）：nodes 升为 kind，node_reachability 照抄为附属表。
// 订阅一族归配置类；「订阅摘挂节点」这个动作写 Node 上的 detached，与用户自己的 enabled 分开。

func nodeTables() []Table {
	return []Table{
		{
			Name: "nodes", Kind: "Node", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("username", TypeText),
				// 从 Server 入站同步来的节点记它属于哪台 Server（权威链接，改名不受影响）；
				// original_server 照抄 mmwx，是同步时记下的服务器名，只作展示与导入兼容，不再作 join 键。
				col("server_id", TypeInt).null(),
				// raw_url 是节点 URI（UUID / 密码在里面），parsed_config 与 clash_config 是同一份凭据的另两种写法，
				// 按第 05 章功能②「节点配置里的私钥、PSK 与 UUID」打码；node_name 是展示用的，不打。
				col("raw_url", TypeText).masked(),
				col("node_name", TypeText),
				col("protocol", TypeText),
				col("parsed_config", TypeJSON).masked(),
				col("clash_config", TypeText).masked(),
				col("enabled", TypeBool).def("FALSE").defaultTrue(),
				col("tag", TypeText).def("'手动输入'"),
				col("tags", TypeJSON).def("'[]'"),
				// 以下是 mmwx 的中转、链式代理、路由出站那组列，照抄。
				col("original_server", TypeText).null(),
				col("original_domain", TypeText).null(),
				col("inbound_tag", TypeText).null(),
				col("chain_proxy_node_id", TypeInt).null(),
				col("relay_group_name", TypeText).null(),
				col("relay_group_node_ids", TypeJSON).def("'[]'"),
				col("node_type", TypeText).def("'physical'"),
				col("parent_node_id", TypeInt).null(),
				col("routed_outbound_tag", TypeText).null(),
				col("routed_outbound_json", TypeJSON).null().masked(), // 完整的出站对象，含凭据，与 user_outbounds.outbound_json 同一类数据
				col("routed_rule_marktag", TypeText).null(),
				col("routed_admin_email", TypeText).null(),
				col("routed_admin_credential", TypeText).null().masked(),
				col("routed_owner", TypeText).def("'shared'"),
				col("relay_orig_server", TypeText).null(),
				col("relay_orig_port", TypeInt).def("0"),
				col("ip_family", TypeText).def("''"),
				// 动作专属：订阅摘挂节点（第 05 章白名单动作），渲染时与 enabled 一起排除。
				col("detached", TypeBool).def("FALSE").cls(ClassAction),
				col("detach_reason", TypeText).null().cls(ClassAction),
			),
			Indexes: []Index{
				{Name: "nodes_username_idx", Columns: []string{"username"}},
				{Name: "nodes_enabled_idx", Columns: []string{"enabled"}},
				{Name: "nodes_parent_idx", Columns: []string{"parent_node_id"}},
				{Name: "nodes_protocol_idx", Columns: []string{"protocol"}},
				{Name: "nodes_tag_idx", Columns: []string{"tag"}},
				{Name: "nodes_type_idx", Columns: []string{"node_type"}},
			},
			ForeignKeys: []ForeignKey{fkUser("username"), fk("server_id", "servers", "SET NULL")},
		},
		{
			// 可达性：内置任务写，一节点一行。
			Name: "node_reachability", GoName: "NodeReachability", Origin: OriginMMWX,
			Columns: []Column{
				col("node_id", TypeInt),
				col("reachable", TypeBool).def("FALSE").defaultTrue(),
				col("consecutive_fail", TypeInt).def("0"),
				col("since", TypeTime).def("CURRENT_TIMESTAMP"),
				col("announced_blocked", TypeBool).def("FALSE"),
			},
			PrimaryKey:  []string{"node_id"},
			ForeignKeys: []ForeignKey{fk("node_id", "nodes", "CASCADE")},
		},
	}
}
