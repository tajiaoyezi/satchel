package schema

// 证书与 DNS 簇（附录 A.3「证书与 DNS 簇照抄」）：certificates 升为 kind Certificate（第 07 章配置类清单），
// dns_providers 升为 kind DnsProvider。mmwx 用 0 表示「没有」的两个引用列改成可空外键。

func certificateTables() []Table {
	return []Table{
		{
			// DNS 服务商凭据，DDNS 与 DNS-01 验证共用；credentials 是 JSON，按第 05 章打码。
			Name: "dns_providers", Kind: "DnsProvider", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("name", TypeText),
				col("provider_type", TypeText),
				col("credentials", TypeJSON).masked(),
			),
			Indexes: []Index{naturalKey("dns_providers_name_key", "name")},
		},
		{
			// ACME 证书。spec 是申请参数与证书材料（第 07 章：Certimate 上传与自动续期都是改 spec、抬版本），
			// status 是 ACME 流程写的结果。server_id 为 NULL 表示在主控本机验证（取代 mmwx 的 remote_server_id = 0）；
			// 物理删服务器时 RESTRICT：证书材料不能随服务器消失，也不能靠 SET NULL 悄悄变成「主控本机」（会撞 domain 的本机唯一键）。
			Name: "certificates", Kind: "Certificate", KindClass: KindConfig, Origin: OriginMMWX,
			Columns: withMeta(
				col("domain", TypeText),
				col("email", TypeText),
				col("provider", TypeText).def("'letsencrypt'"),
				col("challenge_mode", TypeText).def("'standalone'").enum("standalone", "webroot", "dns", "manual"),
				col("webroot_path", TypeText).null(),
				col("server_id", TypeInt).null(),
				col("dns_provider_id", TypeInt).null(),
				col("cert_pem", TypeText).null(),
				col("key_pem", TypeText).null().masked(),
				col("auto_renew", TypeBool).def("FALSE").defaultTrue(),
				col("deploy_target", TypeText).def("'none'"),
				col("deploy_cert_path", TypeText).null(),
				col("deploy_key_path", TypeText).null(),
				col("auto_deploy", TypeBool).def("FALSE"),
				col("status", TypeText).def("'pending'").enum("pending", "valid", "expired", "failed").cls(ClassStatus),
				col("expiry_date", TypeTime).null().cls(ClassStatus),
				col("issue_date", TypeTime).null().cls(ClassStatus),
				col("message", TypeText).null().cls(ClassStatus),
				col("cert_path", TypeText).null().cls(ClassStatus),
				col("key_path", TypeText).null().cls(ClassStatus),
			),
			Indexes: []Index{
				// mmwx 的 UNIQUE(domain, remote_server_id)：两库的唯一索引都把 NULL 当作互不相等，所以拆成两个部分唯一索引。
				{Name: "certificates_domain_server_key", Columns: []string{"domain", "server_id"}, Unique: true, Where: "server_id IS NOT NULL", NaturalKey: true},
				{Name: "certificates_domain_local_key", Columns: []string{"domain"}, Unique: true, Where: "server_id IS NULL", NaturalKey: true},
				{Name: "certificates_domain_idx", Columns: []string{"domain"}},
				{Name: "certificates_status_idx", Columns: []string{"status"}},
				{Name: "certificates_server_id_idx", Columns: []string{"server_id"}},
				{Name: "certificates_expiry_date_idx", Columns: []string{"expiry_date"}},
			},
			ForeignKeys: []ForeignKey{fkServer("RESTRICT"), fk("dns_provider_id", "dns_providers", "SET NULL")},
		},
	}
}
