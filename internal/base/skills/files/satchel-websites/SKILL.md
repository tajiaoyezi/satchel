---
name: satchel-websites
description: "管理节点上的 nginx 站点（kind Website）：查站点、扫盘区分已纳管与未纳管文件、查模板目录、用无状态向导生成静态/普通反代/WSS反代/伪装站配置并经 plan/apply 声明式落盘，严守未纳管零覆盖与同机七组/443 双闸门。"
---

# satchel-websites

目标：围绕配置类一等资源 `Website` 管理节点上的 nginx 站点。权威配置保存在主控数据库的 `spec`，节点扫盘结果与落盘路径只写进 `status`；所有站点的新增、修改、软删除与恢复一律经 `satchel plan` 与 `satchel apply` 原子执行，不提供绕过事务的裸写命令。全部命令只对管理员开放，普通用户会话或普通用户令牌一律返回 `forbidden`。

## 查询站点与模板目录

- 分页列出站点：`satchel website list`（可按 `--server-name <名字>`、`--type static`、`--include-deleted` 过滤）。每条返回完整 `Website` 对象，含 `spec`（`server_id`、`domain`、`type`、`target`、`certificate_id`、`use_443`）与 `status`（`scanned`、`managed`、`conf_path`）。
- 查看单条站点：`satchel website get <id>`。
- 查看内置模板与安全规则：`satchel website templates`，列出静态站（`static`）、普通反代（`proxy`）、WSS 域名反代（`wss-proxy`）与伪装站内置模板（`landing`、`blog`、`docs`、`empty`），以及配置防注入与保护规则。

## 目录扫描（未纳管站点零覆盖）

- 扫盘：`satchel website scan <名字>`（动作类，经管理通道调用节点 `nginx.websites.scan` 扫描节点 nginx HTTP 站点目录）。
- 已纳管站点：凡在主控 `websites` 表有对应 `(server_id, domain)` 期望规格的站点，仅回写 `status.scanned`、`status.managed` 与 `status.conf_path`，绝不抬升 `resourceVersion`。
- 未纳管站点：磁盘上存在但主控没有对应 `Website` 规格的手工配置（不带 `# satchel-managed:` 首行标记）只列在返回结果的 `unmanaged` 清单中，绝不自动写入 `spec`，后续 `satchel apply` 亦绝不覆盖或删除这些未纳管文件。

## 无状态站点向导与声明式落盘

- 构造校验过的 `Website` 规格与预览配置：
  - 静态站：`satchel website build --server-name <名字> --domain site.example.com --type static`
  - 普通反代：`satchel website build --server-name <名字> --domain api.example.com --type proxy --target http://127.0.0.1:8080`
  - WSS 域名反代：`satchel website build --server-name <名字> --domain ws.example.com --type proxy --target http://127.0.0.1:10000 --wss-path /ws`
  - 伪装站：`satchel website build --server-name <名字> --domain decoy.example.com --type decoy --target landing`（也可用 `blog`、`docs`、`empty` 或 `https://...` 镜像上游；远程节点如需 HTTPS 可加 `--use-443` 与 `--certificate-id 1`）
- `website build` 不写库也不碰节点，严格拦截域名与 `target` 中的 nginx 配置注入字符（空格、分号、花括号、引号、`$`、`..` 与控制字符），输出 `spec`、`preview_conf` 与可直接提交给 `satchel plan --spec <JSON>` 的 `plan_op`。
- 落盘与回滚：把 `plan_op`（或 `update` / `delete` / `restore` 操作）提交给 `satchel plan --spec <JSON>` 生成计划并核对 `preview_conf` 与 `requires_human`，再经 `satchel apply --plan <计划号>` 写入 `config_snapshots` 并由节点原子落盘、执行 `nginx -t` 校验与重载（失败自动回滚全部受影响 `.conf` 并返回 `rolled_back: true`）。历史修改可用 `satchel snapshot rollback --id <快照号>` 生成回滚计划。

## 同机双闸门与安全边界

- **同机 443 防抢占**：主控同机节点（`same_host_as_master=true`）严禁在 `Website` 中开启 `use_443=true` 直接抢占主控 443 端口；`website build`、`plan`、`apply` 与节点侧 `nginx.websites.apply` 均会拦截并返回 `conflict`。
- **同机七组动态人类验证**：远程节点上的 `Website` 允许合规 API 令牌（含 MCP）直接执行 `satchel apply`；但当目标服务器实时 `same_host_as_master=true` 时，`plan` 会标出 `requires_human: true`，`apply` 会在写库与推节点前动态触发七组当场人类验证——API 令牌一律返回 `human_required`，需请管理员在主控本机终端（带 `--verify-user <管理员账号>`）或登录网页完成当场验密。
- 节点的 `nginx` 安装/卸载、服务启停与 stream 端口巡检清理见 `satchel-servers`。
