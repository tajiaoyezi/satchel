// Package command 是契约层的命令表：登记每条命令的名字、参数、类别、危险类、是否人类专属、
// 是否列表命令与 REST 映射（master-command-table）。CLI、REST、MCP 三个投影在启动时从这张表构造自己，
// 「命令 × scope 对照表」docs/commands.md 由它生成（go generate ./internal/command/）。
//
// 本包只放元数据与调用对象的形状（Invocation、Handler、Runner），不持有任何处理函数的实现：
// 绑定发生在 cmd/satchel。本包只能引用 pkg/ 与标准库。
package command

//go:generate go run ./cmd/gen
