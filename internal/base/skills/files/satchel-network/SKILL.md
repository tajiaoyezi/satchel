---
name: satchel-network
description: "管理节点网络探测（端口扫描、站点 TLS/Reality 校验、最多 200 域名 16 并发延迟批量探测）、纯 Go WARP WireGuard 凭据与出站/防送中快捷路由规划，以及返程路由（kind ReturnRoute）的查询与测路长任务。"
---

# satchel-network

目标：覆盖节点侧三项网络探测工具、纯 Go WARP WireGuard 完整闭环以及一等配置类资源 `ReturnRoute` 的查询与测路任务。全部命令只对管理员开放，普通用户会话或普通用户令牌一律返回 `forbidden`；所有 `Outbound`、`RoutingRule` 与 `ReturnRoute` 写操作一律经 `satchel plan` 与 `satchel apply` 落地。

## 网络探测三项工具

这三条命令均为管理口动作命令（不改库，绝不暴露在只读轮询口 `pull_port`），探测目标不可达或证书失败会返回结构化 `ok: false` 而不是 RPC 报错：

- **节点端口扫描**：`satchel server scan <名字> --host 127.0.0.1 --ports 443 --ports 8000-8010 --timeout 1s --concurrency 32`（单次去重后最多 256 个端口，并发 1..64，超时 100ms..5s；省略 `--ports` 时扫描常用端口 `22,80,443,8080,8443`）。
- **远程站点校验**：`satchel website validate <名字> --domain www.example.com --port 443 --sni www.example.com --timeout 10s`（从节点发起真实 TCP + TLS 握手，返回延迟、TLS 版本、ALPN、是否 HTTP/2、证书主题/签发者/到期剩余天数、HTTP 状态码与 `reality_suitable` 综合判定）。
- **域名延迟批量探测**：`satchel server domain-latency <名字> --domains a.example.com --domains b.example.com --port 443 --mode tls --timeout 5s --concurrency 16`（硬上限：去重后最多 200 个域名、最多 16 并发；支持 `--mode tls` 或 `--mode tcp`，按输入顺序返回结果并附按延迟升序的 `fastest` 推荐清单）。

## 纯 Go WARP WireGuard 闭环与防送中快捷路由

节点侧采用纯 Go Cloudflare WARP WireGuard 设备注册与 `<dataDir>/warp.json`（权限 `0600`）凭据存储，不依赖第三方 `warp-cli` 守护进程，裸机与 Docker 容器通用：

- **查状态**：`satchel server warp status <名字>`（读类，绝不返回私钥、access token 或 license 原文；同时汇总主控 `servers.warp_installed` 与库内 `is_warp: true` 出站及防送中路由状态）。
- **注册安装**：`satchel server warp install <名字> --mtu 1280`（长任务，向 Cloudflare WARP API 注册设备并原子落盘，成功后更新 `servers.warp_installed=true` 且不抬升 `resourceVersion`；可选 `--license-key <密钥>` 顺带升级 WARP+、`--endpoint <地址>`、`--api-base <地址>`、`--reinstall` 强制重注册）。
- **授权升级或离线导入**：
  - 绑定 WARP+ license：`satchel server warp authorize <名字> --license-key <密钥>`（需已完成设备注册，敏感参数审计打码）；
  - 离线导入已有 WireGuard 凭据：`satchel server warp authorize <名字> --private-key <私钥> --address-v4 172.16.0.2/32 --reserved 12,34,56`（可选 `--address-v6`、`--peer-public-key`、`--endpoint`、`--mtu`）。
- **移除节点 WARP 凭据**：`satchel server warp remove <名字> --confirm <名字>`（删除类危险操作；原子删除节点 `warp.json`、将 `warp_installed` 置为 `false` 并列出同机尚未清理的活跃 `is_warp` 出站供按需经 `plan` / `apply` 删除）。
- **规划 WARP 出站与防止送中路由**：`satchel server warp outbound-plan <名字> --tag warp --anti-china --sort-order 40`（从在线节点拉取凭据生成符合 `sing-box` 顶层 `endpoints[]` 规范的 `wireguard` `Outbound`（`is_warp: true`）；加 `--anti-china` 时在同一份待确认 `Plan` 中附带复用内置 `warp-google-meta` 预设的防送中路由规则，可选 `--inbound-tag <tag>` 限定单入站）。拿到 `plan_id` 后执行 `satchel apply --plan <计划号>` 原子落库并下发生效。

## 返程路由（kind ReturnRoute）查询与测路长任务

`ReturnRoute` 是一等配置类资源（自然键 `(server_id, carrier)`，标准码 `CT`、`CU`、`CM`、`EDU` 自动转大写）：

- **配置增改删恢复与回滚**：一律通过 `satchel plan --spec <JSON>`、`satchel apply --plan <计划号>` 与 `satchel snapshot rollback --id <快照号>` 执行（纯主控配置，不触发节点 `kernel.json` 或 `nginx` 下发，离线节点亦可预配）。
- **分页列表与详情**：`satchel return-route list --server-name <名字>`（可按 `--carrier CT`、`--enabled`、`--include-deleted` 过滤）与 `satchel return-route get <id>`。
- **启动节点返程测路长任务**：`satchel return-route test <名字>`（关联服务器长任务，默认测该服务器全部已启用线路；也可加 `--carrier CT` 仅测指定单条线路）。节点执行追踪并识别 `CN2 GIA`、`CN2 GT`、`AS9929`、`AS4837`、`CMIN2`、`CMI`、`163`、`CERNET`、`Unknown`，主控仅回写 `return_routes` 的 `status` 六列，绝不抬升 `resourceVersion`。
- 如需排查代理出站/路由与更多预设见 `satchel-proxy`；常规站点建站与扫盘见 `satchel-websites`；基础节点运维见 `satchel-servers`。
