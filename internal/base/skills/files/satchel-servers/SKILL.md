---
name: satchel-servers
description: "加服务器（节点）、取安装命令、看节点是否在线时用：安装命令要 https 主控地址，token 原文要请人取。"
---

# satchel-servers

目标：在主控上登记一台新服务器，拿到在节点上运行的一键安装命令，交给人去节点上装；装好以后确认它连上了主控。

## 加服务器

- 加一台：`satchel server add <名字>`。它属于配置类，令牌要有日常运维权限（`operate`）；名字不能和已有的重复（重复时是 `name_taken`）。
- 这一版连接模式只有 websocket：`--mode` 省略就是 websocket，写别的值会被拒（`bad_request`）。
- 服务器建了以后暂时改不了，也删不了（要等配置类的 plan/apply）：不要为了试一试随手建，名字想好再建。
- 结果是这台服务器的完整对象，`status.status` 是 `pending`；里面的 token 是打码的，安装命令要另外取。

## 取安装命令

- `satchel server install-command <名字>`。前提是主控地址（`master_url`）已设且是 https，否则返回 `conflict`。
- 主控地址没设时：这一步只能由人做：请用户在主控本机、用 root 或运行主控的用户、在没有设 `SATCHEL_TOKEN` 与 `SATCHEL_SERVER`、也没用 `satchel login` 存过登录文件的终端里（不是 AI runtime 的终端）执行 `sudo satchel settings master-url set --url https://panel.example.com --resource-version <N> --verify-user <管理员账号>`（版本号先用 `satchel settings show` 看），它会当场要这个管理员的密码（开了两步验证还要验证码）；Docker 部署把开头的 `sudo satchel` 换成 `docker compose exec satchel satchel`。也可以请人用管理员账号登录网页，在「系统设置」页上改。
- 命令里的 token 原文要密钥读取：令牌没开密钥读取时，命令里 `--token` 后面是打码标记，这样的命令装不上。这时请人在主控本机执行 `sudo satchel server install-command <名字>` 取完整的命令。
- 拿到的命令由人在节点上用 root 运行（命令里没有 `sudo`，不是 root 时先 `sudo -i` 切到 root）；AI 不经主控去节点上执行它，也不要把带 token 原文的命令贴进对话。
- 结果里的 `master_key_fingerprint` 是主控公钥的指纹，节点上的安装脚本会拿它核对命令里的公钥，对不上就停。

## 看服务器与在线状态

- 列出：`satchel server list`。每台有 id、名字、连接模式、状态、最近心跳时间与来源 IP。
- 查看一台：`satchel server get <名字>`。输出里的 token 一律打码，本机管理员也一样。
- 状态的含义：`pending` 是还没连上过；`connected` 是在线；`offline` 是连接断开，或者心跳超过离线阈值（默认 90 秒）没来。
- 节点的心跳间隔取系统设置的 `heartbeat_interval`，但最多是离线阈值的三分之一（默认阈值下最多 30 秒），设得更长也按这个上限下发，在线的节点不会因此被标成离线；改了以后节点下次连上时生效。
- 刚运行完安装命令，一般几秒到一分钟内变成 `connected`。一直是 `pending` 时，请人在节点上看 satchel-agent 服务的日志（`journalctl -u satchel-agent` 或 `/var/log/satchel-agent.log`）：常见原因是节点连不上主控地址、主控公钥或 token 不对。

## 注意

- token 等同于这台节点的身份，不要出现在对话、日志或命令行的回显里。
- 主控地址、门与安装命令里的 token 原文都是人来处理的事，AI 只告诉人去哪里做。
