// Package cli 是投影层的命令行：把 satchel 子命令解析成对 service 的调用并编码输出。
// 投影层只做解析、调用、编码，不含业务。
//
// MCP 的 satchel_run 也用 NewRootCommand 解析命令数组，CLI 与 MCP 共用同一个解析器。
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

// options 是全局选项，由根命令的 persistent flag 与环境变量决定，各子命令共用。
type options struct {
	// json 为 true 时输出 JSON，否则输出给人看的文本。
	json bool
}

// NewRootCommand 组装 satchel 根命令。每次调用返回独立的一份，可安全地并发 Execute。
func NewRootCommand() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:          "satchel",
		Short:        "Satchel（百宝袋）：多服务器代理管理系统的主控与命令行",
		SilenceUsage: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			// 环境变量 SATCHEL_OUTPUT=json 与 --json 等价；显式给了 flag 就以 flag 为准。
			if !cmd.Flags().Changed("json") && os.Getenv("SATCHEL_OUTPUT") == "json" {
				opts.json = true
			}
		},
	}
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "以 JSON 输出（等价于环境变量 SATCHEL_OUTPUT=json）")
	root.AddCommand(newVersionCommand(opts))
	return root
}
