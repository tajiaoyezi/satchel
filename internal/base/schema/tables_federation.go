package schema

// 联邦簇（附录 A.2「联邦（分享服务器）照抄」）：四张附属表，都不是 kind。M8 按第 05 章六类授权重做分享范围时再定 kind。
// mmwx 的列集合来自 internal/storage/federated_servers.go、shared_servers.go、rule_template_owners.go。

func federationTables() []Table {
	return []Table{
		{
			// 消费方：本地那台 servers 行代表拥有方分享来的服务器；share_token 是向拥有方认证的凭据，打码。
			Name: "federated_servers", GoName: "FederatedServer", Origin: OriginMMWX,
			Columns: []Column{
				col("server_id", TypeInt),
				col("owner_url", TypeText),
				col("share_token", TypeText).masked(),
				col("prefix", TypeText).def("''"),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey:  []string{"server_id"},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			// 拥有方：一条分享一行；allow_manage_xray 改名 allow_manage_inbounds（Satchel 没有 xray，语义是允许消费方管理入站）。
			Name: "shared_servers", GoName: "SharedServer", Origin: OriginMMWX,
			Columns: []Column{
				col("id", TypeSerial),
				col("server_id", TypeInt),
				col("token_hash", TypeText).masked(),
				col("label", TypeText).def("''"),
				col("allow_manage_inbounds", TypeBool).def("FALSE"),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
				col("revoked_at", TypeTime).null(),
			},
			Indexes:     []Index{{Name: "shared_servers_token_hash_key", Columns: []string{"token_hash"}, Unique: true}},
			ForeignKeys: []ForeignKey{fkServer("CASCADE")},
		},
		{
			// 分享范围内的入站。mmwx 没有主键、只有 UNIQUE(share_id, inbound_tag)，这里把它作主键；
			// inbound_tag 不做指向 inbounds 的复合外键，否则分享中的入站改不了 tag。
			Name: "shared_server_inbounds", GoName: "SharedServerInbound", Origin: OriginMMWX,
			Columns: []Column{
				col("share_id", TypeInt),
				col("server_id", TypeInt),
				col("inbound_tag", TypeText),
				col("created_at", TypeTime).def("CURRENT_TIMESTAMP"),
			},
			PrimaryKey:  []string{"share_id", "inbound_tag"},
			ForeignKeys: []ForeignKey{fk("share_id", "shared_servers", "CASCADE"), fkServer("CASCADE")},
		},
		{
			// 规则模板文件的归属，照抄。
			Name: "rule_template_owners", GoName: "RuleTemplateOwner", Origin: OriginMMWX,
			Columns: []Column{
				col("filename", TypeText),
				col("created_by", TypeText).def("''"),
			},
			PrimaryKey: []string{"filename"},
		},
	}
}
