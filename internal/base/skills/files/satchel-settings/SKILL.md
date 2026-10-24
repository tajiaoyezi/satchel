---
name: satchel-settings
description: "看与改系统设置时用：日常运维档的设置、logo 与登录壁纸的图片上传、写前快照与回滚；主控地址与门这两组要请人改。"
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

## logo 与登录壁纸的图片

`branding_logo_url` 与 `login_wallpaper` 可以填外部图片的地址，也可以先把图片传到主控上：

1. 上传：

   ```
   satchel settings image upload --file ./logo.png
   ```

   - 只收 PNG、JPEG、GIF、WebP 与 ICO，最大 5 MiB；主控按文件内容判断类型，不收 SVG。
   - 输出里的 `url` 是图片的地址（`/public/uploads/` 开头）。同样的图片传两次，地址相同。
   - 它只放文件，不改设置。它是上传类，MCP 上做不了；这时请人在网页的「系统设置」页点 logo 或登录壁纸旁边的「上传」。
2. 再把地址写进字段，`<N>` 是当前的版本号：

   ```
   satchel settings set --set branding_logo_url=/public/uploads/<文件名> --resource-version <N>
   ```

3. `public/uploads/` 最多留 20 个文件，多的从最旧的删；当前设置与写前快照里用着的不删，所以回滚之后旧图片还在。

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

这两组是人类专属，令牌一律 `human_required`，写操作也不存快照。两组也可以请人用管理员账号登录网页，在「系统设置」页上改，当场验证在网页上输入。

- 主控地址与订阅域名：这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel settings master-url set --url https://panel.example.com --resource-version <N> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。订阅域名用 `--subscription-url`；值是干净的 `https://` 或 `http://` 地址（只有协议、域名与可选的端口），空串表示清掉。
- 门（关闭公网访问、静默模式、登录限流与封禁的参数、Turnstile、反代登记）：这一步只能由人做：请用户在同样的终端里执行 `sudo satchel settings gates set --set silent_mode=true --resource-version <N> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署同样换成 `docker compose exec satchel satchel`。写完下一个请求就按新值判定。
- 打开之前提醒人：关闭公网访问打开后，经 TCP 的网页、远程 CLI 与 MCP 都可能马上进不去；静默模式锁定时，网页与远程 CLI（走面板 API）进不去，MCP 照常。两种情况下主控本机的 CLI 都不受影响。被挡在外面时见 `satchel-troubleshoot`。

## Turnstile 的测试配置

门这一组的 `turnstile_site_key` 与 `turnstile_secret_key` 填好之后，可以核对它们配得对不对：`satchel settings turnstile test --response <token>`。

- `<token>` 是网页上 Turnstile 组件给出的一次性 token，AI 拿不到；通常请人在网页的「系统设置」页点「测试配置」，由页面完成这一步。
- 输出里 `success` 是核对通过没有，`error_codes` 是 Cloudflare 给的原因，`hostname` 是签发 token 的站点。
- 两个 key 没都填是 `conflict`；连不上 Cloudflare 是 `unavailable`。它只核对，不改设置。

## 注意

- 更新 CDN 的开关是主控自身类危险，见 `satchel-upgrade`。
- 改之前把要改的字段、旧值与新值告诉人；改完用 `satchel settings show` 核对。
