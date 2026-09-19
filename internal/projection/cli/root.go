// Package cli 是投影层的命令行：子命令树由命令表构造（master-command-table「三个投影从表构造」），
// 每条命令的 RunE 只做解析 → Invocation → Runner → 渲染。本地命令（version、db、__verify、serve）在本进程里跑；
// 经主控的命令默认经数据目录的 unix socket 发 REST 请求（master-cli「经主控的命令走本机连接」）。
//
// MCP 的 satchel_run 用同一棵树解析命令数组（换一个进程内的 Runner），CLI 与 MCP 因此共用解析器。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Renderer 把一条命令的结果渲染成给人看的文本。
type Renderer func(w io.Writer, result any) error

// Options 是构造子命令树的依赖，由 cmd/satchel 装配、测试自己拼。
type Options struct {
	// Table 是命令表，通常是 command.Catalog()。
	Table *command.Table
	// Local 是本地命令的处理函数：version、db *、__verify 由 DefaultOptions 内置，serve 由 cmd/satchel 注入。
	Local command.Bindings
	// Remote 按数据目录构造经主控命令的执行器；nil 时用连本机 socket 的客户端。MCP 传进程内的执行链。
	Remote func(dataDir string) command.Runner
	// Renderers 是按命令名的文本渲染；没登记的用通用渲染。
	Renderers map[string]Renderer
}

// DefaultOptions 是 satchel 二进制的默认装配（不含 serve，它要 cmd/satchel 才能装）。
func DefaultOptions() Options {
	opts := Options{Table: command.Catalog(), Local: command.Bindings{}, Renderers: map[string]Renderer{}}
	registerBuiltins(&opts)
	return opts
}

// options 是一次执行的全局状态：由根命令的 persistent flag 与环境变量决定，各子命令共用。
type options struct {
	json      bool
	dataDir   string
	preRunRan bool
}

type dataDirKey struct{}

// DataDir 取本次执行解析出的数据目录（--data-dir、SATCHEL_DATA_DIR、默认值），本地命令的处理函数用它。
func DataDir(ctx context.Context) string {
	if dir, ok := ctx.Value(dataDirKey{}).(string); ok {
		return dir
	}
	return db.DataDir("")
}

// NewRoot 从命令表组装 satchel 根命令。每次调用返回独立的一份，可安全地并发 Execute。
// 错误不由 cobra 打印，交给 Execute 按四字段渲染。
func NewRoot(opts Options) *cobra.Command {
	root, _ := newRoot(opts)
	return root
}

func newRoot(opts Options) (*cobra.Command, *options) {
	o := &options{}
	root := &cobra.Command{
		Use:           "satchel",
		Short:         "Satchel（百宝袋）：多服务器代理管理系统的主控与命令行",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			o.preRunRan = true
			// 环境变量 SATCHEL_OUTPUT=json 与 --json 等价；显式给了 flag 就以 flag 为准。
			if !cmd.Flags().Changed("json") && os.Getenv("SATCHEL_OUTPUT") == "json" {
				o.json = true
			}
			o.dataDir = db.DataDir(o.dataDir)
			cmd.SetContext(context.WithValue(cmd.Context(), dataDirKey{}, o.dataDir))
		},
	}
	root.PersistentFlags().BoolVar(&o.json, "json", false, "以 JSON 输出（等价于环境变量 SATCHEL_OUTPUT=json）")
	root.PersistentFlags().StringVar(&o.dataDir, "data-dir", "", "数据目录（默认取环境变量 "+db.EnvDataDir+"，再默认 "+db.DefaultDataDir+"）")
	buildTree(root, opts, o)
	return root, o
}

// Execute 跑一次 satchel 命令：解析 args、执行、把失败按四字段输出到 stderr，返回进程退出码。
func Execute(opts Options, args []string, stdout, stderr io.Writer) int {
	return ExecuteContext(context.Background(), opts, args, stdout, stderr)
}

// ExecuteContext 同 Execute，但带 ctx：MCP 把带身份的请求 ctx 传进来。
func ExecuteContext(ctx context.Context, opts Options, args []string, stdout, stderr io.Writer) int {
	root, o := newRoot(opts)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := checkDuplicateFlags(root, opts.Table, args)
	if err == nil {
		err = root.ExecuteContext(ctx)
	}
	if err == nil {
		return v1.ExitOK
	}
	e := normalizeError(err, o.preRunRan)
	// 用法错误时 PersistentPreRun 没跑，--json 只能从原始参数与环境变量里看。
	asJSON := o.json || (!o.preRunRan && wantsJSON(args))
	writeError(stderr, e, asJSON)
	return v1.ExitCodeOf(e)
}

// Resolve 用命令表构造的树找出 args 指向的命令（不执行）。找不到、或指向的是分组节点时返回 false。
// MCP 在执行前用它做拒绝清单检查。
func Resolve(opts Options, args []string) (*command.Command, bool) {
	root := NewRoot(opts)
	found, _, err := root.Find(args)
	if err != nil || found == nil {
		return nil, false
	}
	name, ok := found.Annotations[annotationName]
	if !ok {
		return nil, false
	}
	return opts.Table.Lookup(name)
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

// usageError 是我们自己产生的用法错误（多余或缺少的位置参数、重复的 flag），文案直接给人看。
func usageError(format string, args ...any) *v1.Error {
	return v1.Newf(v1.CodeUsage, "用法错误："+format, args...).WithNext("运行 satchel --help 查看用法")
}

// checkDuplicateFlags 在解析前预扫原始参数：同一个标量 flag 出现两次是用法错误（pflag 只会让后者覆盖前者）。
// 只认长名（表里的 flag 一律没有短名）：--name、--name=value、--name value 三种写法；-- 之后的片段算位置参数。
func checkDuplicateFlags(root *cobra.Command, t *command.Table, args []string) error {
	var multi map[string]bool
	if found, _, err := root.Find(args); err == nil && found != nil {
		if name, ok := found.Annotations[annotationName]; ok {
			if c, ok := t.Lookup(name); ok {
				multi = map[string]bool{}
				for _, f := range c.Flags {
					if f.Type == command.TypeStrings {
						multi[f.Name] = true
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "--") || len(a) == 2 {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if multi[name] {
			continue
		}
		if seen[name] {
			return usageError("参数 --%s 重复", name)
		}
		seen[name] = true
	}
	return nil
}
