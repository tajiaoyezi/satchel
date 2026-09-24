package cli

import (
	"context"
	"errors"
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
		if f.Type == command.TypePassword {
			continue // 密码只从终端读，不作为命令行参数存在（给了就是未知 flag → usage）
		}
		values[f.Name] = registerFlag(leaf.Flags(), f)
	}
	var confirm string
	var page command.Page
	if c.Danger != "" {
		leaf.Flags().StringVar(&confirm, "confirm", "", "危险操作的确认字符串："+confirmHint(c))
	}
	var verifyCode, verifyUser string
	if c.HumanOnly {
		// 当场验证：密码只从终端读；验证码可以作参数也可以终端输入；verify-user 是本机管理员要指明的管理员账号。
		leaf.Flags().StringVar(&verifyCode, command.VerifyCodeFlag, "", "当场验证的第二因素：验证器当前的码或一枚恢复码（不给会在终端里问）")
		leaf.Flags().StringVar(&verifyUser, command.VerifyUserFlag, "", "当场验证要验的管理员账号（本机管理员必填；登录的用户只能验自己）")
	}
	if c.List {
		leaf.Flags().IntVar(&page.Limit, "limit", command.DefaultLimit, fmt.Sprintf("每页条数（1 到 %d）", command.MaxLimit))
		leaf.Flags().StringVar(&page.Cursor, "cursor", "", "上一页返回的 nextCursor")
	}
	leaf.RunE = func(cmd *cobra.Command, args []string) error {
		inv := &command.Invocation{Path: c.Path, Args: args, Flags: map[string]any{}, Confirm: confirm}
		for _, f := range c.Flags {
			if !cmd.Flags().Changed(f.Name) {
				continue
			}
			if f.Type == command.TypeObject {
				// object 类型在命令行上是可重复的 字段=值，这里拼成对象（缺等号、重复字段是 usage）。
				pairs, _ := deref(values[f.Name]).([]string)
				obj, err := command.ObjectFromPairs(f.Name, pairs)
				if err != nil {
					return err
				}
				inv.Flags[f.Name] = obj
				continue
			}
			inv.Flags[f.Name] = deref(values[f.Name])
		}
		if c.List {
			if cmd.Flags().Changed("limit") && (page.Limit < 1 || page.Limit > command.MaxLimit) {
				return v1.Newf(v1.CodeBadRequest, "limit 必须在 1 到 %d 之间，得到 %d", command.MaxLimit, page.Limit)
			}
			p := page
			inv.Page = &p
		}
		runner, err := runnerFor(cmd.Context(), c, opts, o)
		if err != nil {
			return err
		}
		if !opts.ServerSide && c.Class != command.ClassLocal {
			// 密码类 flag 从终端读两遍（新密钥），人类专属命令再读当场验证的密码与可选的验证码。
			if err := readPasswords(c, inv, opts); err != nil {
				return err
			}
			if c.HumanOnly {
				if err := readVerification(c, inv, verifyCode, verifyUser, opts); err != nil {
					return err
				}
			}
		}
		result, err := execute(cmd.Context(), c, inv, opts, runner)
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

// runnerFor 决定一条经主控的命令交给谁执行：有 Remote 执行器（MCP 的进程内执行链、测试）用它，否则按本次的连法
// （flag、环境变量、登录文件）建客户端。本地命令与本机作答的离线命令返回 nil。连法在读终端之前解析，配错了不必先输密码。
func runnerFor(ctx context.Context, c *command.Command, opts Options, o *options) (command.Runner, error) {
	if c.Class == command.ClassLocal || (c.Offline && !opts.ServerSide) {
		return nil, nil
	}
	if opts.Remote != nil {
		return opts.Remote(o.dataDir), nil
	}
	conn, err := Connect(ctx)
	if err != nil {
		return nil, err
	}
	return NewClient(opts.Table, conn), nil
}

// execute 决定一条命令在哪里跑：本地命令调 Local 里的处理函数（除了会连主控的几条，显式给了 --server / --token 是用法错误），
// 离线命令本地作答，其余交给 runner。
func execute(ctx context.Context, c *command.Command, inv *command.Invocation, opts Options, runner command.Runner) (any, error) {
	switch {
	case c.Class == command.ClassLocal:
		if f := connFlagsOf(ctx); (f.serverSet || f.tokenSet) && !connectingLocal[c.Name()] {
			return nil, usageError("%s 只在本机跑、不连主控，不接受 --server 与 --token", c.Name())
		}
		h, ok := opts.Local[c.Name()]
		if !ok {
			return nil, v1.Newf(v1.CodeInternal, "本地命令 %s 没有装配处理函数", c.Name())
		}
		return h(ctx, inv)
	case c.Offline && !opts.ServerSide:
		return command.Explain(opts.Table, inv.Arg(0))
	}
	return runner.Run(ctx, inv)
}

// readPasswords 让 password 类型的 flag 从终端读两遍并比对；没有终端直接拒绝（人类专属命令是 human_required，其它 bad_request）。
func readPasswords(c *command.Command, inv *command.Invocation, opts Options) error {
	for _, f := range c.Flags {
		if f.Type != command.TypePassword {
			continue
		}
		first, err := prompt(opts, f.Description)
		if err != nil {
			return noTerminal(c, err)
		}
		second, err := prompt(opts, "再输入一次确认")
		if err != nil {
			return noTerminal(c, err)
		}
		if first != second {
			return v1.Newf(v1.CodeBadRequest, "两次输入的 %s 不一致", f.Name)
		}
		inv.Flags[f.Name] = first
	}
	return nil
}

// readVerification 读当场验证的值：密码只从终端读；验证码没作参数给就问一次（没开两步验证直接回车）。
func readVerification(c *command.Command, inv *command.Invocation, code, user string, opts Options) error {
	pw, err := prompt(opts, "当场验证：请输入你的密码")
	if err != nil {
		return noTerminal(c, err)
	}
	if code == "" {
		code, err = prompt(opts, "两步验证码或恢复码（没开两步验证直接回车）")
		if err != nil {
			return noTerminal(c, err)
		}
	}
	inv.Verify = &command.Verification{Password: pw, Code: code, User: user}
	return nil
}

func prompt(opts Options, label string) (string, error) {
	if opts.Prompt == nil {
		return terminalPrompt(label)
	}
	return opts.Prompt(label)
}

// noTerminal 把「没有终端」翻成四字段错误：人类专属命令是 human_required（退出码 9），其它 bad_request。
func noTerminal(c *command.Command, err error) error {
	if !errors.Is(err, ErrNoTerminal) {
		return v1.Wrap(v1.CodeInternal, "从终端读取失败", err)
	}
	if c.HumanOnly {
		return v1.Wrap(v1.CodeHumanRequired, c.Name()+" 是只有人能做的操作，要在终端里当场验证身份；当前没有终端", err).
			WithNext("在有终端的会话里执行，或在网页上操作")
	}
	return v1.Wrap(v1.CodeBadRequest, c.Name()+" 要从终端读密码，当前没有终端", err).
		WithNext("在有终端的会话里执行，或在网页上操作")
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
	case command.TypeObject:
		p := new([]string)
		fs.StringArrayVar(p, f.Name, nil, f.Description+"（可重复，写成 字段=值）")
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
