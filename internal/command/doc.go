// Package command 是契约层的命令表：登记每条命令的名字、参数、类别、
// 所需 scope、是否危险、是否人类专属。cli、rest、mcp 三个投影与
// 「命令 × scope 对照表」由它生成，不手写三遍。
// 本包只能引用 pkg/api/v1。
package command
