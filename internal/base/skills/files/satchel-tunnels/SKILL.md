---
name: satchel-tunnels
description: "部署与切换服务器 443 偷取模式（tunnel 与 fallback）、编排任意门单跳端口转发、复用已有 direct 入站按域名/IP 分流以及 N 台服务器首尾相接链式隧道时用：零新增表，全部写操作一律经 plan/apply 原子落地，严守同机 443 防抢占与 sing-box v1.14.4 规范。"
---

# satchel-tunnels

目标：管理服务器的 443 偷自己部署（`tunnel` / `fallback` / `off`）、单跳与复用任意门端口转发以及 N 台服务器链式隧道编排。本域全部 9 条命令只对管理员开放（普通用户会话与普通用户令牌一律返回 `forbidden`），零新增数据库表，所有写操作均只生成待确认 `Plan`，必须由管理员执行 `satchel apply --plan <计划号>` 原子提交与下发，并天然支持 `satchel snapshot rollback --apply-id <apply_id>` 整单回滚。

## 偷取模式（不使用内核回落）

两种模式均不依赖内核 fallback，且本地伪装站统一由主控渲染至固定文件 `/etc/nginx/servers/satchel-steal.conf`（监听 `127.0.0.1:8001 ssl; http2 on;` 并自动合并同机活跃 `ws` 入站，**绝不启用 `proxy_protocol`**，因 `sing-box@v1.14.4` 已移除 `direct` 出站 `proxy_protocol` 且 Reality 握手直通原始 TCP/TLS）：

- 查看状态与配置预览：`satchel server steal status <名字>`（纯读主控库，展示 `same_host_as_master`、`use_443`、`steal_mode`、`domain`、`tunnel-in` 与路由、推荐空闲端口 `next_free_port` 以及偷自己 nginx 配置预览）。
- 规划部署、模式切换与关闭：
  - `tunnel` 模式（443 `direct` 入站 `tunnel-in` 按 SNI 分流至 `127.0.0.1:8001` 与内部回环 Reality 端口，且 TCP 443 可与 `hysteria2`/`tuic` UDP 443 共存）：`satchel server steal plan <名字> --mode tunnel --domain decoy.example.com --site-type decoy --site-value landing`（可选 `--internal-port 18443`、`--inbound-tag <tag>`；若 `tunnel-in` 曾被软删除会自动按原 ID 规划 `restore` 无重名冲突）。
  - `fallback` 模式（非 `ws` Reality 入站直接监听对外 443，`handshake` 指向 `127.0.0.1:8001`，同时软删除 `tunnel-in`）：`satchel server steal plan <名字> --mode fallback --domain decoy.example.com`。
  - 关闭偷自己：`satchel server steal plan <名字> --mode off`（将 `use_443` 置为 `false`、清理 `tunnel-in` 及其路由，`apply` 时先推内核释放 443 再原子删除 `satchel-steal.conf`）。
- 启用 `use_443=true` 时，禁止与同机已存在的同名 `Website` 发生域名冲突（返 `conflict`）；同机活跃 `ws` 入站保持在 `127.0.0.1` 不被重映射，自动合并进 `satchel-steal.conf` 实现偷自己与 WSS 共存。
- 执行 `satchel apply --plan <计划号>` 时：开启或维持 `use_443=true` 会**先同步 nginx 确保 `127.0.0.1:8001` 就绪再推 `kernel.json`**；关闭 `use_443=false` 会**先推 `kernel.json` 释放 443 再移除 `satchel-steal.conf`**。

## 任意门单跳转发与复用分流

底层统一建模为 `sing-box@v1.14.4` 原生 `direct` 入站加路由规则动作 `override_address` / `override_port`（指向 `outbound: "direct"`，支持 TCP 与 UDP；双协议 `tcp,udp` 在 `settings` 中省略 `network` 字段，绝不写非法的 `"tcp,udp"` 字符串）：

- 查看所有散装任意门隧道：`satchel tunnel list`（可按 `--server-name <名字>`、`--kind inbound` 或 `--kind multiplex` 过滤；直接读主控库离线可见，自动排除偷自己 `tunnel-in` 与链式隧道跳）。
- 规划单跳端口转发：
  - 固定目标：`satchel tunnel plan --server-name <入口机> --listen-port 20001 --network tcp,udp --target-address 198.51.100.1 --target-port 443`（省略 `--listen-port` 时自动在服务器端口范围内分配空闲端口）。
  - 转发已有服务器入站：`satchel tunnel plan --server-name <入口机> --target-server <目标机> --target-inbound <入站tag>`（自动解析目标机可达地址与端口并按目标入站协议推导默认网络类型，自动拦截同机同端口自环）。
- 复用已有 `direct` 入站按域名/IP 分流（不另开监听端口）：`satchel tunnel multiplex plan --server-name <入口机> --inbound-tag <tag> --domain app.example.com --target-address 198.51.100.2 --target-port 8443`（至少提供 `--domain`、`--domain-suffix`、`--ip-cidr`、`--source-ip-cidr` 之一，目标同样支持 `--target-server` + `--target-inbound`；省略 `--sort-order` 时自动排在现有兜底规则之前，渲染器自动前置 `{"action":"sniff"}`）。
- 规划清理隧道（命中 `delete` 危险类）：
  - 删除整条单跳隧道及其全部绑定规则：`satchel tunnel delete-plan --server-name <入口机> --tag <tag>`（禁止删除 `tunnel-in` 与链式跳）。
  - 仅删除单条复用分流规则（保留共享 `direct` 入站与兜底转发）：`satchel tunnel delete-plan --rule-id <规则id>`。

## N 台服务器链式隧道

面向 $N \ge 2$ 台互不重复服务器首尾相接端口转发，统一按 `tunnel-<label>-h<i>` 命名并打包进**单一原子 `Plan`**（共 $2N$ 个对象）：

- 查看全部链式隧道：`satchel tunnel chain list`（按 `label` 字典序分页聚合，展示跳数、入口地址端口、最终目标与 `complete` 完整性校验结果）。
- 编排新链式隧道：`satchel tunnel chain plan --label hk-jp-us --servers hk-1 --servers jp-1 --servers us-1 --entry-port 25000 --target-address 198.51.100.99 --target-port 443`（`--servers` 为字符串数组，按入口到出口顺序逐项传入，CLI 上通过重复传 `--servers` 指定，REST/MCP 传字符串数组，不按逗号拆分；下游各跳必须具备可达地址；末跳同样支持 `--target-server` + `--target-inbound`；自动按跳分配不冲突端口并校验同名链占坑）。拿到 `plan_id` 后执行 `satchel apply --plan <计划号>` 一次性落库并串行推送各节点；如需整链撤销可执行 `satchel snapshot rollback --apply-id <apply_id>`。
- 规划整链清理：`satchel tunnel chain delete-plan --label hk-jp-us`（一次性收集全部服务器上的链跳入站与绑定规则放入单份 `Plan`，命中 `delete` 危险类，多对象自动按批量阈值要求确认）。

## 同机 443 守卫与安全边界

- **主控同机 443 防抢占**：当服务器 `same_host_as_master=true`（或 `pending` 态非回环域名与 `https://` 主控地址域名一致）时，主控在 `server steal plan`、`tunnel plan`、`tunnel chain plan`、`inbound build`、`plan` 与 `apply` 全链路拒绝开启 `use_443=true` 或启用 `443` 端口入站（返回 `conflict`），并在 `config.apply` 透传 `protect_master_443: true`。
- **节点双层防护**：节点 `config.apply` 收到 `protect_master_443` 或检测到本地 `/etc/nginx/stream_servers/master_https_443.conf` / `# satchel-master-443` 标记时立即拒绝含 443 端口入站的配置；节点启动前还会运行 `ProtectMasterHTTPSPort443` 自愈，若磁盘 `kernel.json` 意外残留 443 入站，先按 `0600` 备份为 `.before-master-https-protect` 再仅剥离 443 入站及其关联路由规则。
- 如需查看或构建常规代理入站/出站/路由规则，见 `satchel-proxy`；如需管理独立 `Website` 站点，见 `satchel-websites`。
