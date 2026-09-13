// Package cli 是投影层的命令行：把 satchel 子命令解析成对 service 的调用并编码输出。
// 投影层只做解析、调用、编码，不含业务。
//
// MCP 的 satchel_run 也用 NewRootCommand 解析命令数组，CLI 与 MCP 共用同一个解析器。
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/spf13/cobra"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// options 是全局选项，由根命令的 persistent flag 与环境变量决定，各子命令共用。
type options struct {
	// json 为 true 时输出 JSON，否则输出给人看的文本。
	json bool
	// preRunRan 记录 PersistentPreRun 有没有跑到：cobra 先解析 flag、校验参数、找子命令，都过了才跑它，
	// 所以命令出错时它没跑过就是用法错误。
	preRunRan bool
}

// NewRootCommand 组装 satchel 根命令。每次调用返回独立的一份，可安全地并发 Execute。
// 错误不由 cobra 打印，交给 Execute 按四字段渲染。
func NewRootCommand() *cobra.Command {
	root, _ := newRoot()
	return root
}

func newRoot() (*cobra.Command, *options) {
	opts := &options{}
	root := &cobra.Command{
		Use:           "satchel",
		Short:         "Satchel（百宝袋）：多服务器代理管理系统的主控与命令行",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			opts.preRunRan = true
			// 环境变量 SATCHEL_OUTPUT=json 与 --json 等价；显式给了 flag 就以 flag 为准。
			if !cmd.Flags().Changed("json") && os.Getenv("SATCHEL_OUTPUT") == "json" {
				opts.json = true
			}
		},
	}
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "以 JSON 输出（等价于环境变量 SATCHEL_OUTPUT=json）")
	root.AddCommand(newVersionCommand(opts), newDBCommand(opts))
	return root, opts
}

// Execute 跑一次 satchel 命令：解析 args、执行、把失败按四字段输出到 stderr，返回进程退出码。
// main.go 只调它。
func Execute(args []string, stdout, stderr io.Writer) int {
	root, opts := newRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return v1.ExitOK
	}
	e := normalizeError(err, opts.preRunRan)
	// 用法错误时 PersistentPreRun 没跑，--json 只能从原始参数与环境变量里看。
	asJSON := opts.json || (!opts.preRunRan && wantsJSON(args))
	writeError(stderr, e, asJSON)
	return v1.ExitCodeOf(e)
}

// normalizeError 把任何错误规范成四字段错误：用法错误 → usage（退出码 2，只有它是 2），
// 已是四字段的原样返回，其它包成 internal。
func normalizeError(err error, preRunRan bool) *v1.Error {
	var e *v1.Error
	if errors.As(err, &e) {
		return e
	}
	if !preRunRan {
		return v1.Wrap(v1.CodeUsage, usageReason(err.Error()), err).
			WithNext("运行 satchel --help 查看用法")
	}
	return v1.Wrap(v1.CodeInternal, "命令执行失败", err)
}

// cobra 与 pflag 的用法错误文案是英文且随版本变化，reason 里改成自己的话；原文留在 Unwrap 链里。
var usagePatterns = []struct {
	re     *regexp.Regexp
	format string
}{
	{regexp.MustCompile(`^unknown command "([^"]+)"`), "用法错误：没有子命令 %s"},
	{regexp.MustCompile(`^unknown flag: (\S+)`), "用法错误：没有参数 %s"},
	{regexp.MustCompile(`^unknown shorthand flag: '(.)'`), "用法错误：没有参数 -%s"},
	{regexp.MustCompile(`^flag needs an argument: (\S+)`), "用法错误：参数 %s 缺少值"},
	{regexp.MustCompile(`^invalid argument "([^"]*)" for "([^"]+)"`), "用法错误：参数 %[2]s 的值 %[1]s 不合法"},
	{regexp.MustCompile(`^accepts (\d+) arg\(s\), received (\d+)`), "用法错误：只接受 %s 个参数，给了 %s 个"},
	{regexp.MustCompile(`^required flag\(s\) (.+) not set`), "用法错误：缺少必填参数 %s"},
}

func usageReason(msg string) string {
	for _, p := range usagePatterns {
		if m := p.re.FindStringSubmatch(msg); m != nil {
			args := make([]any, len(m)-1)
			for i, v := range m[1:] {
				args[i] = v
			}
			return fmt.Sprintf(p.format, args...)
		}
	}
	return "用法错误：" + msg
}

func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "--json=true" {
			return true
		}
		if a == "--json=false" {
			return false
		}
	}
	return os.Getenv("SATCHEL_OUTPUT") == "json"
}
