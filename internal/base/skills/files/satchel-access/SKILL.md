---
name: satchel-access
description: "管令牌、AI 接入、账号与 IP 封禁时用：看令牌与谁在连，签发吊销令牌与接入 runtime 要请人做。"
---

# satchel-access

目标：看清有哪些令牌、谁在连、各自有什么权限；签发、改权限、吊销令牌，接入 AI runtime，改账号的安全设置，封禁与解封 IP，这些都讲清步骤交给人做。

## 看令牌与谁在连

- 自己这把令牌的权限：`satchel whoami` 的 `scopes` 与 `danger`。
- 列出令牌：`satchel token list`。每把有 id、名字、预设、`state`（active、revoked、expired 之一）与最后使用时间（按分钟记）。普通用户只看得到自己的；管理员可以只看某个签发者的：`satchel token list --owner <用户名>`。
- 看谁在连（绑了 runtime 标签的令牌，按最后使用时间倒序）：CLI 上用 `satchel mcp status`；MCP 上做不了，改用 `satchel token list`。
- 权限范围：`read`（恒有）、`operate`、六个危险类（delete、restart、permission、batch、exec、master）与密钥读取 `secrets`。预设由它推出：readonly 只读，ops 能操作、危险类全关，full 能操作、六类全开。
- 令牌每次使用时，生效的权限是它的权限范围与签发者当下角色的交集：签发者被降为普通用户后危险类与密钥读取随之失效，签发者停用后令牌无效。普通用户只能签 `read` 与 `operate`。

## 签发、改权限、吊销：要请人做

这三条是人类专属，令牌不能签令牌，也不能改或吊销令牌。

- 签发：这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel token create --name <名字> --preset ops --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。
  - 新令牌默认只读、不过期；`--expires-in 720h` 设过期时间，`--secrets` 打开密钥读取。
  - 令牌明文只在这次输出里出现一次，请人自己保存好，不要贴进对话。
- 改权限：这一步只能由人做：请用户在同样的终端里执行 `sudo satchel token update <id> --danger master --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署同样换成 `docker compose exec satchel satchel`。
  - `--danger` 把危险类设成恰好这几个（隐含 `operate`），原来开着、还要保留的要一起写上；`--preset` 按预设整体重设；`--name`、`--expires-in` 改名字与过期时间。改完立刻生效，令牌字符串不变。
- 吊销：这一步只能由人做：请用户在同样的终端里执行 `sudo satchel token revoke <id> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署同样换成 `docker compose exec satchel satchel`。吊销立刻生效、不能恢复。
- 要开哪一类、给谁开，先把理由告诉人，由人决定。

## 接入 AI runtime：要请人做

`satchel mcp init` 把一个 runtime（claude-code、codex 或 hermes）接上主控：签一把令牌（或用已有的），写进它的 MCP 配置与环境变量，并把 skills 装进它的 skills 目录。

- 它不套上面那句话：由 runtime 所属的系统用户、在 runtime 所在的机器上执行，不加 `sudo`（`sudo` 会写进 root 的目录）。
- 这个用户能连上主控的 unix socket（就在主控本机，并且是 root 或运行主控的用户）时，直接签发，终端里会要管理员的密码：

  ```
  satchel mcp init --runtime claude-code --verify-user <管理员账号>
  ```

- 连不上 socket 时（runtime 在别的机器上，或者主控以 root 或别的用户运行），分两步：
  1. 先照上一节的写法，请人在主控本机签一把令牌，例如 `sudo satchel token create --name codex@laptop --preset ops --runtime codex@laptop --verify-user <管理员账号>`。
  2. 再由 runtime 的用户执行下面这条，令牌从终端读：

     ```
     satchel mcp init --runtime codex --use-token --url https://panel.example.com
     ```

- 新签的令牌默认预设 ops，`--preset` 可改；名字与 runtime 标签默认 `<runtime>@<主机名>`，`--name` 可改。`--print` 只打印要加的配置与命令，不写任何文件（输出里有令牌明文）。
- 改完要重启 runtime，MCP 配置与 skills 才生效。验证：`claude mcp get satchel`、`codex mcp get satchel --json`、`hermes mcp test satchel`。
- 升级 satchel 之后更新 skills：由同一个用户执行 `satchel mcp init --runtime claude-code --skills-only`，再重启 runtime。`--skills-only` 只和 `--runtime` 一起用：不连主控、不签令牌、不改配置。
- skills 目录里的 `satchel-*` 归 Satchel 管，手改的内容会被下次 `mcp init` 覆盖。
- `satchel mcp stdio` 是 runtime 用的垫片（Claude Code 以本地进程方式连主控），不要手动执行。

## 账号

- 看自己账号的状态（角色、两步验证、剩余恢复码、活动会话数）：`satchel account show`。令牌身份看到的是签发者的账号；本机管理员不是账号，会得到 `bad_request`。剩余恢复码不足两枚时有 `recovery_codes_low` 提示，转告人。
- 改密码、两步验证与恢复码这几条只能由账号本人登录之后做：`satchel account set-password`、`satchel account totp setup`、`satchel account totp confirm --code <验证码>`、`satchel account totp disable`、`satchel account recovery-codes regenerate`。
  - 令牌做不了（`human_required`）；本机管理员不是账号，主控本机的终端也做不了。
  - 网页随 m1-11 交付；在那之前只能用账号的登录会话调 REST。请人自己操作。
- 管理员忘了密码见 `satchel-troubleshoot`。

## IP 封禁

- 列出生效中的封禁：`satchel security bans list`（只对管理员开放）。被封的 IP 带令牌的请求一律被拒，网页会话与登录照常。
- 封禁与解封是人类专属：
  - 封禁：这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel security ban <ip> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。默认按 `brute_force_block_minutes` 到期，加 `--permanent` 永久封禁。
  - 解封：这一步只能由人做：请用户在同样的终端里执行 `sudo satchel security unban <ip> --verify-user <管理员账号>`，它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署同样换成 `docker compose exec satchel satchel`。
- 安全事件里的封禁记录见 `satchel-troubleshoot`。

## 注意

- 令牌明文、密码与验证码不要出现在对话、日志或命令行里。
- 发现自己的令牌权限过大或可能泄露，告诉人，建议吊销后重签。
