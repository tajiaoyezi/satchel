---
name: satchel-proxy
description: "管理 sing-box 代理入站（Inbound）、出站（Outbound）、路由规则（RoutingRule）、密钥生成、无状态建站向导、出站实时探活、中转落地规划、路由预设与多机批量规划时用：所有配置变更一律走 plan / apply。"
---

# satchel-proxy

目标：在 `sing-box@v1.14.4` 规范下查询、设计、生成并批量规划服务器的代理入站、出站与路由规则，并在在线节点上实测出站可达性。本组全部命令只对管理员开放，普通用户调用一律返回 `forbidden`。所有 `Inbound`、`Outbound` 与 `RoutingRule` 的新增、修改与删除都必须经 `satchel plan` 与 `satchel apply` 两阶段配置事务提交，不存在绕过 `plan` / `apply` 的直接写命令。

## 入站查询、协议矩阵、示例与密钥

- 列出入站：`satchel inbound list`（可加 `--server-name <服务器>`、`--protocol <协议>`、`--include-deleted`）。无 `secrets` 权限的令牌看到的 `tls` 与 `settings` 会自动打码为 `***`。
- 查看单条入站：`satchel inbound get <id>`。结果额外附带 `derived.reality_public_key`（由 Reality 私钥实时经 X25519 派生，绝不写入节点的 `kernel.json`，防止 `sing-box` 因未知字段报错）。
- 查看支持矩阵：`satchel inbound protocols`。返回 `sing-box@v1.14.4` 支持的 15 种原生入站协议、合法的传输层与安全层组合、被废弃禁用的字段，以及并行协议线说明（例如仅支持原生 sing-box 协议，明确标注不含非原生协议）。
- 查看配置示例：`satchel inbound examples`（可加 `--protocol vless`、`--transport tcp`、`--security reality` 筛选），直接给出纯 Go 内嵌的标准配置范本。
- 生成加密随机凭据：`satchel inbound keygen --type all`（`--type` 可选 `all`、`uuid`、`x25519`、`short-id`、`password`、`ss2022` 或 `wireguard`；`ss2022` 可配 `--cipher 2022-blake3-aes-128-gcm`）。全在主控本地用加密随机源纯 Go 生成，不调用外部二进制。

## 无状态入站向导与多机批量规划

- 单机入站向导：`satchel inbound build --server-name <服务器> --protocol vless --port 443 --security reality --sni www.microsoft.com`。它是只读命令，不落库；会校验协议、传输与安全组合，自动补全默认流控、UUID、Reality 密钥对与 `short_id`，直接输出可直接喂给 `satchel plan` 的 `Inbound` 变更对象。常用可选参数还有 `--tag`、`--listen`、`--transport`、`--handshake`、`--path`、`--service-name`、`--cipher`、`--flow`、`--cert-path`、`--key-path`、`--user-name`、`--password`、`--uuid`、`--sort-order`。
- 多机批量规划入站：`satchel inbound batch plan --servers hk-1 --servers hk-2 --protocol vless --port 443 --security reality --sni www.microsoft.com`（也可用 `--all` 覆盖全库服务器，或传 `--spec <JSON>` 直接提供模板）。它为每台目标服务器各自生成独立密钥并产出一份待确认的 `Plan`（同时在审计记录中写明 `plan_id`），确认 diff 与所需危险类后再执行 `satchel apply --plan <plan_id>` 原子落库并下发到节点。
- 查看入站批次记录：`satchel inbound batch list`（可按 `--batch-id <批次>` 或 `--server-name <服务器>` 过滤）。只有 `apply` 真正成功提交后才会原子写入批次记录。

## 出站查询、实时探活、负载均衡、落地规划与批量规划

- 列出出站：`satchel outbound list`（可加 `--server-name <服务器>`、`--balancer-group <组名>`、`--include-deleted`）。同台服务器上多个出站填写相同的 `balancer_group` 时，主控渲染 `kernel.json` 会自动合成 `urltest` 或 `selector` 组出站；`wireguard` 类型出站会自动渲染到顶层 `endpoints`。
- 查看单条出站：`satchel outbound get <id>`，含最近一次探活的连通性、延迟与错误信息；无 `secrets` 权限时 `settings` 敏感字段打码。
- 节点实时探活：`satchel outbound probe <id> --url https://www.gstatic.com/generate_204 --timeout 10s`。主控经管理通道通知在线节点通过运行中 `sing-box` 的指定出站直接发起单次 HTTP 探活（仅允许 `http` 与 `https`，超时范围 1 秒到 30 秒），不改磁盘配置也不中断现有连接。探活结果回写到出站 `status` 字段且**不抬升** `resourceVersion`。
- 中转与落地一体化规划：`satchel outbound landing plan --server-name <源服务器> --outbound-tag land-jp --target-server <目标服务器> --target-inbound <目标入站>`（加 `--inbound-tag <入站tag>` 只绑定单个入站，省略时生成整机兜底路由；加 `--detour <前置出站tag>` 串接链式跳转；加 `--balancer-group <组名>` 加入负载均衡组；加 `--sort-order <值>` 指定排序）。若落地端不是托管服务器，可改用 `--protocol socks` 配合 `--settings '{"server":"1.2.3.4","server_port":1080}'` 手工指定。该命令同样只产出待确认 `Plan`，由 `satchel apply --plan <plan_id>` 原子落地出站与对应路由规则。
- 多机批量规划出站：`satchel outbound batch plan --servers hk-1 --servers hk-2 --tag direct-out --protocol direct`（支持 `--all`、`--settings`、`--balancer-group`、`--probe`），同样产出待确认 `Plan`，经 `satchel apply` 原子落库与记录批次。
- 查看出站批次记录：`satchel outbound batch list`（可按 `--batch-id <批次>` 或 `--server-name <服务器>` 过滤）。

## 路由查询、遮蔽告警与快捷预设

- 列出路由规则：`satchel routing list --server-name <服务器>`（可加 `--include-deleted`）。输出按 `sort_order` 升序排列，并附带 `is_inbound_catch_all` 与 `shadowed_by` 告警：如果某条规则对所有流量或某个入站做了无条件兜底，排在它后面且针对同范围的规则会被标出遮蔽提示。
- 查看单条路由规则：`satchel routing get <id>`。注意 `sing-box@v1.14.4` 路由规则匹配凭据用户名用 `auth_user`（禁止写 `user`），且已移除旧版 `geoip` / `geosite`，主控渲染时会自动前置 `sniff` 动作并按规则里的 `rule_set` 引用自动合成去重后的 `route.rule_set`。
- 查看内置快捷预设：`satchel routing preset builtin`，返回 10 条适配 `sing-box@v1.14.4` 的常用路由规则与配套远程 `.srs` `rule_set` 定义（不含任何废弃 `geoip` / `geosite` 键）。
- 列出自定义预设：`satchel routing preset list`（可按 `--owner <账号>` 过滤）。
- 保存自定义预设：`satchel routing preset save --name block-quic --rule '{"protocol":["quic"]}'`（本机管理员可省略 `--owner`，自动回落主管理员账号）。会严格校验合法 JSON 并拒收废弃的 `geoip`、`geosite` 与 `user` 键。
- 删除自定义预设：`satchel routing preset delete <id> --confirm <id>`。属于删除类危险操作（`delete`），必须带 `--confirm <id>` 且令牌具备 `delete` 危险类权限。

## 注意

- `inbound build`、`inbound batch plan`、`outbound batch plan` 与 `outbound landing plan` 均不会直接修改正在运行的节点，必须审阅 `plan` 输出并执行 `satchel apply --plan <plan_id>` 才会落库并原子推送到节点。
- 多机批量变更达到系统设定的批量门控阈值时，`satchel apply` 会动态要求 `batch` 危险类与 `--confirm`，请按 `satchel-basics` 中的配置事务流程确认执行。
- 涉及私钥、密码、UUID 与 `settings` 原文的操作请遵守最小权限原则，不要把 `secrets` 凭据原文贴进无关对话或日志。
