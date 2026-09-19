package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// annotationName 把命令表里的名字挂在 cobra 命令上，Find 之后能反查表。
const annotationName = "satchel.command"

// buildTree 按命令表建 cobra 树：多段路径的中间段是分组节点（只做分组，不带子命令运行是用法错误）。
func buildTree(root *cobra.Command, opts Options, o *options) {
	groups := map[string]*cobra.Command{}
	for _, c := range opts.Table.All() {
		parent := root
		for i := 0; i < len(c.Path)-1; i++ {
			key := joinPath(c.Path[:i+1])
			g, ok := groups[key]
			if !ok {
				seg := c.Path[i]
				g = &cobra.Command{
					Use:   seg,
					Short: opts.Table.GroupSummary(key),
					RunE: func(cmd *cobra.Command, args []string) error {
						if len(args) > 0 {
							return usageError("没有子命令 %s", args[0])
						}
						return usageError("%s 需要一个子命令", cmd.CommandPath())
					},
				}
				groups[key] = g
				parent.AddCommand(g)
			}
			parent = g
		}
		parent.AddCommand(newLeaf(c, opts, o))
	}
}

func joinPath(segs []string) string {
	out := ""
	for i, s := range segs {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

// newLeaf 建一条命令：flag 按表登记，RunE 统一为解析 → Invocation → Runner → 渲染。
func newLeaf(c *command.Command, opts Options, o *options) *cobra.Command {
	use := c.Path[len(c.Path)-1]
	for _, a := range c.Args {
		if a.Optional {
			use += " [" + a.Name + "]"
		} else {
			use += " <" + a.Name + ">"
		}
	}
	values := map[string]any{}
	leaf := &cobra.Command{
		Use:         use,
		Short:       c.Summary,
		Hidden:      c.Hidden,
		Annotations: map[string]string{annotationName: c.Name()},
		Args:        argsValidator(c),
	}
	for _, f := range c.Flags {
		values[f.Name] = registerFlag(leaf.Flags(), f)
	}
	var confirm string
	var page command.Page
	if c.Danger != "" {
		leaf.Flags().StringVar(&confirm, "confirm", "", "危险操作的确认字符串："+confirmHint(c))
	}
	if c.List {
		leaf.Flags().IntVar(&page.Limit, "limit", command.DefaultLimit, fmt.Sprintf("每页条数（1 到 %d）", command.MaxLimit))
		leaf.Flags().StringVar(&page.Cursor, "cursor", "", "上一页返回的 nextCursor")
	}
	leaf.RunE = func(cmd *cobra.Command, args []string) error {
		inv := &command.Invocation{Path: c.Path, Args: args, Flags: map[string]any{}, Confirm: confirm}
		for _, f := range c.Flags {
			if cmd.Flags().Changed(f.Name) {
				inv.Flags[f.Name] = deref(values[f.Name])
			}
		}
		if c.List {
			if cmd.Flags().Changed("limit") && (page.Limit < 1 || page.Limit > command.MaxLimit) {
				return v1.Newf(v1.CodeBadRequest, "limit 必须在 1 到 %d 之间，得到 %d", command.MaxLimit, page.Limit)
			}
			p := page
			inv.Page = &p
		}
		result, err := execute(cmd.Context(), c, inv, opts, o)
		if err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), c, result, opts, o)
	}
	return leaf
}

func confirmHint(c *command.Command) string {
	if c.Confirm != nil && c.Confirm.Kind == command.ConfirmObject {
		return "填 " + c.Confirm.Arg + " 的值"
	}
	return "填本次受影响的数量"
}

// execute 决定一条命令在哪里跑：本地命令调 Local 里的处理函数，离线命令本地作答，其余交给 Remote 执行器。
func execute(ctx context.Context, c *command.Command, inv *command.Invocation, opts Options, o *options) (any, error) {
	switch {
	case c.Class == command.ClassLocal:
		h, ok := opts.Local[c.Name()]
		if !ok {
			return nil, v1.Newf(v1.CodeInternal, "本地命令 %s 没有装配处理函数", c.Name())
		}
		return h(ctx, inv)
	case c.Offline && !opts.ServerSide:
		return command.Explain(opts.Table, inv.Arg(0))
	}
	var runner command.Runner
	if opts.Remote != nil {
		runner = opts.Remote(o.dataDir)
	} else {
		runner = NewClient(opts.Table, socketPath(o.dataDir))
	}
	return runner.Run(ctx, inv)
}

// argsValidator 按表校验位置参数个数，文案点名缺的或多余的那个。
func argsValidator(c *command.Command) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > len(c.Args) {
			return usageError("多余的参数 %s", args[len(c.Args)])
		}
		if len(args) < c.RequiredArgs() {
			return usageError("缺少位置参数 %s", c.Args[len(args)].Name)
		}
		return nil
	}
}

// registerFlag 按类型登记一个 flag，返回值的指针（deref 在 RunE 里取回）。表里的 flag 一律没有短名。
func registerFlag(fs *pflag.FlagSet, f command.Flag) any {
	switch f.Type {
	case command.TypeInt:
		p := new(int)
		if f.Default != "" {
			fmt.Sscan(f.Default, p)
		}
		fs.IntVar(p, f.Name, *p, f.Description)
		return p
	case command.TypeBool:
		p := new(bool)
		fs.BoolVar(p, f.Name, f.Default == "true", f.Description)
		return p
	case command.TypeDuration:
		p := new(time.Duration)
		if f.Default != "" {
			*p, _ = time.ParseDuration(f.Default)
		}
		fs.DurationVar(p, f.Name, *p, f.Description)
		return p
	case command.TypeStrings:
		p := new([]string)
		fs.StringArrayVar(p, f.Name, nil, f.Description+"（可重复）")
		return p
	default: // string、file
		p := new(string)
		fs.StringVar(p, f.Name, f.Default, f.Description)
		return p
	}
}

func deref(p any) any {
	switch v := p.(type) {
	case *int:
		return *v
	case *bool:
		return *v
	case *time.Duration:
		return *v
	case *[]string:
		return *v
	case *string:
		return *v
	}
	return p
}
