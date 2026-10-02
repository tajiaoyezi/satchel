---
name: satchel-upgrade
description: "升级主控时用：检查更新、升到所选渠道的最新版、更新 CDN 的开关、升级失败时的自动回退。"
---

# satchel-upgrade

目标：确认有没有新版本、这台主控能不能自升级，经人同意后升级，并跟到结局。

这一份里的命令都只对管理员开放。

## 检查更新

```
satchel update check
```

- 输出里有当前版本 `current_version`、所选渠道的最新版本 `latest_version`、`has_update`、发布说明 `notes`、版本信息从哪来 `source`（更新 CDN 或 GitHub），以及这台主控能不能自升级 `can_apply`（不能时 `reason` 说明原因）。
- 默认看 stable 渠道；看预发布版用 `satchel update check --channel prerelease`。
- 它要先从更新 CDN 或 GitHub 取到最新版本，两边都取不到时整条返回 `unavailable`，这时也看不到当前版本；稍后再试，或请人检查主控的出网。
- 不能自升级的情况：Docker 部署、直接 `go build` 的开发版、不是 Linux。

## 升级

升级之前把发布说明、目标版本与下面的风险讲给人，得到同意再做。

1. 令牌要单独打开主控自身类危险（`master`）。`whoami` 的 `danger` 里没有 `master` 时会被拒绝（`forbidden`），请人给令牌开这一类，见 `satchel-access`。
2. 执行，`--confirm` 填版本号；版本号必须等于 `update check` 看到的 `latest_version`，不能降级、不能跳到中间版本：

   ```
   satchel update apply 0.1.1 --confirm 0.1.1
   ```

   升预发布版时两条命令都带 `--channel prerelease`，例如 `satchel update apply 0.2.0-rc.1 --channel prerelease --confirm 0.2.0-rc.1`。
3. 它是长任务：下载并验签新版本 → 暂停写入 → 生成一份 `before-upgrade-<时间>.zip` 备份 → 替换二进制 → 原地重启 → 健康检查。CLI 默认跟到结束（主控重启的那几秒里接着等，最多 2 分钟）；MCP 上最多等 60 秒，之后用 `satchel job get <job_id>` 跟到 `done` 或 `failed`。
4. 升级期间别的命令返回 `unavailable`，只能查 job；照错误里的 `next` 等待。
5. 结束后用 `satchel update check` 确认 `current_version` 已是新版本。

## 升级失败

- 新版本启动失败、健康检查不过、或者连续 3 次没能启动时，主控自动回退：放回旧二进制，并用升级前的备份换回旧库。旧版本起来后 job 写成 `failed` 并带上原因，用 `satchel job get <job_id>` 看。
- 回退换回了旧库，所以恢复码全部换新：开了两步验证的账号各有一批新的，明文在主控数据目录的 `recovery-codes/` 下。把这一点与失败原因一起告诉人。
- 下载、验签或试跑新版本失败时什么都不动，job 是 `failed`，主控照常运行。
- job 一直不结束、主控也起不来时，自动回退可能没有发生（新版本一启动就崩溃）。不要自己猜着修，交给人按 satchel 仓库 README「升级主控与更新 CDN」一节的手工回退步骤处理。

## Docker 部署

容器里不能替换二进制，`update apply` 会被拒绝。请人在 compose 文件所在的目录（一键脚本是 `/opt/satchel`）换镜像：

1. 一键脚本写的 `.env` 里，`SATCHEL_IMAGE` 固定了安装时的版本（如 `ghcr.io/tajiaoyezi/satchel:0.1.0`）：先把它的 tag 改成要升到的版本号（`update check` 看到的 `latest_version`）。tag 是 `latest` 的不用改。
2. 再执行 `docker compose pull`，然后 `docker compose up -d`。
3. 容器起来之后用 `satchel update check` 确认 `current_version` 已是新版本；只做第 2 步、没改固定的 tag 时，跑的还是旧版本。

换之前建议先 `satchel backup create`，因为这条路没有自动的升级前备份与回退。

## 更新 CDN 的开关

- 检查更新与下载先问更新 CDN，再问 GitHub。CDN 有没有用上看 `update check` 输出里的 `cdn`。
- 关掉之后直接走 GitHub。它也是主控自身类危险，`--confirm` 填与参数相同的 true 或 false；`<N>` 是 `satchel settings show` 里的 `metadata.resourceVersion`：

  ```
  satchel settings update-cdn set false --resource-version <N> --confirm false
  ```

- 版本对不上是 `version_conflict`：重新 `satchel settings show` 读一遍版本再改。

## 注意

- 升级会让主控重启几秒，期间只能查 job；挑人同意的时间做。
- 升级前的备份只在升级标记还在时受保护，之后按 7 份轮换；要长期保留，请人用 `satchel-backup` 里的下载把它取走。
