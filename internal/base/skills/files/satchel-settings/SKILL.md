---
name: satchel-settings
description: "看与改系统设置时用：日常运维档的设置、写前快照与回滚；主控地址与门这两组要请人改。"
---

# satchel-settings

目标：读清系统设置的当前值，按人的要求改日常运维档的字段；改错了能回滚；人类专属的两组交给人。

系统设置是一个单例对象（kind `SystemSettings`），整份共用一个版本号。`settings` 下的命令都只对管理员开放。

## 看

1. 读当前的设置：

   ```
   satchel settings show
   ```

   - `spec` 是日常运维档，令牌能改的只有这些字段。
   - `status` 是其余几档：人类专属的两组（主控地址、门）、主控自身类（如 `update_cdn_enabled`）、只读字段与运行态（如 `last_restore`）。
   - `metadata.resourceVersion` 是当前版本号，写的时候要带上。
   - 打码字段（如 `telegram_bot_token`、`turnstile_secret_key`）非空时显示 `***`；令牌打开了密钥读取才给原文。
2. 字段清单、每个字段属于哪一档，用 `satchel explain SystemSettings` 查。

## 改日常运维档

1. 先 `satchel settings show` 读到当前的 `metadata.resourceVersion`。
2. 一次可以改多个字段，每个字段一个 `--set 字段=值`，例如：

   ```
   satchel settings set --set heartbeat_interval=45 --set branding_site_title=Satchel --resource-version 3
   ```

   值里有空格时用引号括起来，例如 `satchel settings set --set "branding_site_title=My Panel" --resource-version 3`。
3. 服务端先整体校验，任一字段不过整单拒绝，不会部分写入：
   - 不在 `spec` 里的字段是 `field_not_applyable`（错误里点名它属于哪一档），不认识的字段是 `unknown_field`。
   - 值超出范围是 `bad_request`，`reason` 写明允许的范围。
4. 版本号对不上是 `version_conflict`（退出码 6）：别人刚改过。重新 `satchel settings show`，在新值的基础上再改；没有跳过比对的写法。
5. 打码字段写回 `***` 表示保持不变，写空串表示清掉。
6. 成功返回写后的整个对象，新的版本号在 `metadata.resourceVersion` 里。

## 快照与回滚

- 每次成功的 `settings set` 与 `settings rollback` 之前都存一份写前快照（只含日常运维档）。按时间倒序列出（不给内容）：

  ```
  satchel settings snapshots list
  ```

- 把一份快照的内容写回，`<id>` 是上面列出的 id，`<N>` 是当前的版本号：

  ```
  satchel settings rollback <id> --resource-version <N>
  ```

  它等于一次 `settings set`：重过字段校验、同样比对版本、同样先存写前快照，所以回滚本身也能再回滚。它不会把版本号倒回去。

## 主控地址与门：要请人改

这两组是人类专属，令牌一律 `human_required`，写操作也不存快照。

- 主控地址与订阅域名：这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel settings master-url set --url https://panel.example.com --resource-version <N> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。订阅域名用 `--subscription-url`；值是干净的 `https://` 或 `http://` 地址（只有协议、域名与可选的端口），空串表示清掉。
- 门（关闭公网访问、静默模式、登录限流与封禁的参数、Turnstile、反代登记）：这一步只能由人做：请用户在同样的终端里执行 `sudo satchel settings gates set --set silent_mode=true --resource-version <N> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署同样换成 `docker compose exec satchel satchel`。写完下一个请求就按新值判定。
- 打开关闭公网访问或静默模式之前提醒人：经 TCP 的网页、远程 CLI 与 MCP 可能马上进不去，主控本机的 CLI 不受影响。被挡在外面时见 `satchel-troubleshoot`。

## 注意

- 更新 CDN 的开关是主控自身类危险，见 `satchel-upgrade`。
- 改之前把要改的字段、旧值与新值告诉人；改完用 `satchel settings show` 核对。
