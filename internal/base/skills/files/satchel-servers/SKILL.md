---
name: satchel-servers
description: "加服务器（节点）、取安装命令、看节点是否在线，以及给节点上的凭据临时限速、踢线、看连接与流量时用：安装命令要 https 主控地址，token 原文要请人取。"
---

# satchel-servers

目标：在主控上登记一台新服务器，拿到在节点上运行的一键安装命令，交给人去节点上装；装好以后确认它连上了主控。节点连上以后，经主控给节点上的凭据临时限速、踢线、看连接与流量。

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

## 临时限速、踢线与连接

这几条都经通道作用到一台在线的服务器：服务器不在线时立即返回 `unavailable`，先用 `satchel server get <名字>` 看状态、等它连上。入站名（`--inbound`）是节点 `kernel.json` 里入站的 tag，凭据名（`--credential`）是入站里用户的名字；节点上没有这个入站时返回 `bad_request` 并带节点给的原因。节点的 satchel-agent 比主控旧、不认识这些操作时返回 `conflict`，要请人升级节点的 satchel-agent。

- 设临时限速：`satchel server limit set <名字> --inbound <tag> --credential <凭据> --mbps <值>`，可加 `--expires-in 30m` 让它到时自动撤掉（不给就一直有效）。动作类，要日常运维权限。
- 每份凭据在节点上有三份限速来源：静态（以后由主控按套餐与用户算出）、主控临时、节点规则临时，生效的是已设且大于 0 的那几份里最小的一个。这两条命令只动「主控临时」这一份，后一次设值连同截止时间覆盖前一次。
- 设成 0 与撤掉不是一回事：`--mbps 0` 是这一份显式不限速、仍算设了；撤掉用 `satchel server limit clear <名字> --inbound <tag> --credential <凭据>`，让这一份退出合成，生效值由剩下的来源重新算。结果里有改完之后的三份来源与生效值，以它为准。
- 限速改动 10 秒内作用到已建立的连接上，不需要重连；节点重启后照样生效，主控失联时也按自己的截止时间到期。
- 踢线：`satchel server kick <名字> --credential <凭据> --inbound <tag>` 只踢这个入站上的连接；不给 `--inbound` 时踢这个凭据名在所有入站上的连接。结果里有踢掉的条数。踢线不是封禁，对方随后能重新连上。
- 列连接：`satchel server connections <名字> --inbound <tag> --credential <凭据>`，每条有网络类型（tcp 或 udp）、对端地址、出站与开始时间。

## 看流量

- `satchel server traffic <名字>`：主控收上来的累计，按（入站，出站，凭据）分上行与下行的字节数，另有内核重启次数（`core_boot_count`）、内核是否在跑与最近一次收到快照的时间。它不经通道，服务器断开以后也能看。
- 主控按系统设置 `traffic_collect_interval`（默认 60 秒）向在线的节点拉快照，所以刚产生的流量要等下一次拉取才看得到；节点重启时，上一次拉取之后的那段流量收不到。
- 累计只在主控内存里，主控重启后从零开始；持久化的流量账本、按用户与套餐的统计要到 M4。

## 注意

- token 等同于这台节点的身份，不要出现在对话、日志或命令行的回显里。
- 主控地址、门与安装命令里的 token 原文都是人来处理的事，AI 只告诉人去哪里做。
