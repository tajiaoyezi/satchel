# Satchel（百宝袋）

Agent-first 的多服务器代理管理系统。主控、CLI 与 MCP 在这一个仓库、一个二进制 `satchel` 里；节点守护是 [satchel-agent](https://github.com/satchel/satchel-agent)。

**状态：M0 骨架阶段，还没有可用功能。**

## 构建

```sh
go build ./cmd/satchel
./satchel version
```

## 数据库

主控默认用 SQLite（数据目录下的 `satchel.db`），可选 PostgreSQL（数据目录下的 `database.json` 写 `driver: postgres`，或用环境变量 `SATCHEL_DATABASE_*` 覆盖）。数据目录由 `--data-dir` 或环境变量 `SATCHEL_DATA_DIR` 指定，默认 `/var/lib/satchel`。

```sh
./satchel db migrate --data-dir ./data   # 执行迁移，然后比对库结构与注册表
./satchel db status --data-dir ./data    # 查看迁移状态与结构比对结果（只读，不建目录、不建库）
./satchel db unlock --data-dir ./data    # 清除上一次迁移被中断后残留的迁移锁
```

每条命令都支持 `--json`，输出是带 `apiVersion` 的 JSON 对象；失败时 stderr 是 `code`、`reason`、`state`、`next` 四字段（`--json` 时是 JSON 对象），用法错误退出码 2，其它按第 05 章的退出码表。

### 结构漂移提示

`db migrate` 跑完会把库里的实际表结构（列、类型、可空、默认值、CHECK、主键、唯一、索引、外键）与注册表逐项比对，发现差异时以错误码 `schema_mismatch`、退出码 1 结束，并列出每一处差异；库里已有表却没有迁移记账（`satchel_migrations` 表不存在）时不动库，同样报 `schema_mismatch` 并说明。`db status` 在全部迁移已应用、或迁移记账缺失时做同样的比对并把结果列在输出里（`--json` 里是 `schema` 一节：`checked`、`consistent`、`diff`、`extraTables`；没比对时 `consistent` 是 null）。出现这些提示说明库是按旧的 `0001_init` 建的，而注册表已经变了。处置按下面「开发期删库重建」；首个正式发布之后改为写新的迁移文件。库里有、注册表里没有的表（例如共用一个 PostgreSQL 库时别的应用的表）只在 `extraTables` 里列出，不算错误。

迁移过程被打断（进程被杀、断电）会在锁表里留下一把锁，之后 `db migrate` 报 `conflict` 并提示 `db unlock`；确认没有别的迁移在跑之后执行它即可。

### 本地起 PostgreSQL

存储层的测试在 SQLite 与 PostgreSQL 各跑一遍。本地没配 `SATCHEL_TEST_PG_DSN` 时 PostgreSQL 那一遍会跳过并提示；CI 设了 `SATCHEL_TEST_REQUIRE_PG=1`，缺库直接失败。

```sh
docker run -d --name satchel-pg -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16-alpine
export SATCHEL_TEST_PG_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
go test ./...
```

每个测试在自己的随机 schema 里跑，结束后删掉，测试之间不共享表。

### 重新生成

表结构的唯一来源是 `internal/base/schema` 的注册表，按功能簇分文件：`tables_agent.go`（Satchel 新增的 12 张 agent-native 表）、`tables_users.go`、`tables_packages.go`、`tables_servers.go`、`tables_nodes.go`（mmwx 的核心 kind 五簇）。现在共 37 张表、21 个 kind；每张 kind 表有整数 id、resource_version 与 deleted_at，每一列标了分档（spec、status、动作专属、人类专属、主控自身类）与是否打码。用户的订阅令牌、会话、订阅设置、凭据表、批量追踪、可达性这些附属表不是 kind，保留 mmwx 的主键形状，没有版本与软删除列。改了注册表之后重新生成，并把生成物一起提交：

```sh
go generate ./internal/base/schema/
```

生成物有四份：两套迁移 SQL（`internal/base/db/migrations/{sqlite,postgres}/0001_init.tx.up.sql`）、bun 模型（`internal/base/model/zz_generated.go`）、kind 的 Spec / Status 结构体与字段清单（`pkg/api/v1/zz_generated_kinds.go`）。`go test` 会比对生成物与注册表，不一致即失败；CI 另外跑一遍 `go generate` 再看 `git diff`。

### 开发期删库重建

首个正式发布之前，`0001_init` 由注册表重新生成，不堆 0002、0003。这意味着注册表一变，开发机上已经迁移过的库就与新的 0001 对不上了，直接删掉重建：

```sh
rm -f ./data/satchel.db ./data/satchel.db-wal ./data/satchel.db-shm   # SQLite
psql "$SATCHEL_TEST_PG_DSN" -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'  # PostgreSQL
./satchel db migrate --data-dir ./data
```

开发期没有需要保留的数据。打第一个正式 tag 时冻结 `0001_init`，之后改表只能新增序号更大的迁移文件。

## 代码分层

六层，依赖只向下：契约（`pkg/api/v1`、`internal/command`）→ 投影（`internal/projection`）→ 横切（`internal/middleware`）→ 业务（`internal/service`）→ 仓储（`internal/core`）→ 基础设施（`internal/base`）。每个目录的 `doc.go` 写了该层的职责；引用规则由 `internal/layering_test.go` 钉住，违反即 `go test` 失败。

## 许可证

GPL-3.0，见 [LICENSE](LICENSE)。
