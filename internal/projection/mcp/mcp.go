// Package mcp 是投影层的 MCP 服务：只暴露 satchel_run 与 satchel_explain 两个工具，命令数组交给 CLI 的同一个解析器、
// 用主控进程内的执行链执行，不经 shell（master-mcp）。身份只来自那次 HTTP 请求的认证结果（authn 放进 ctx），
// 命令数组里的 --token / --server 一律拒绝。
package mcp

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satchel/satchel/internal/base/buildinfo"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/cli"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Path 是 MCP 服务挂载的路径。
const Path = "/mcp"

type runInput struct {
	Args []string `json:"args" jsonschema:"要执行的 satchel 命令数组，如 [\"audit\",\"list\",\"--limit\",\"10\"]；不经 shell，不要带 satchel 本身"`
	// Confirm 收任意 JSON 值：不是字符串时视为没给（master-identity-and-authz「confirm 是字符串」），由执行链报 confirm_required。
	Confirm any `json:"confirm,omitempty" jsonschema:"危险操作的确认字符串：删除、重启、权限、节点执行、主控自身类填对象名，批量类填本次受影响数量"`
}

type explainInput struct {
	Target string `json:"target,omitempty" jsonschema:"命令路径（如 \"audit list\"）或 kind 名（如 Task）；不给则列出全部命令与 kind"`
}

// NewHandler 建 /mcp 的 Streamable HTTP 处理器。opts 是主控进程内装配好的 CLI 选项：Remote 返回执行链本身、ServerSide 为 true。
func NewHandler(opts cli.Options) http.Handler {
	server := sdk.NewServer(&sdk.Implementation{Name: "satchel", Version: buildinfo.Version}, &sdk.ServerOptions{
		Instructions: "Satchel 主控的 MCP 接口。satchel_run 跑一条 satchel 命令（与 CLI 同构，输出恒为 JSON），satchel_explain 看命令与 kind 的说明；不预载工具定义，需要时先 explain。",
	})
	sdk.AddTool(server, &sdk.Tool{
		Name:        "satchel_run",
		Description: "跑一条 satchel 命令（参数是命令数组，不经 shell），返回该命令 --json 的输出；失败时返回 code / reason / state / next 四字段错误。",
	}, runTool(opts))
	sdk.AddTool(server, &sdk.Tool{
		Name:        "satchel_explain",
		Description: "解释一条命令（参数、类别、权限范围、REST 映射）或一个 kind（字段与分档）；不带 target 列出全部。",
	}, explainTool(opts.Table))
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

func runTool(opts cli.Options) sdk.ToolHandlerFor[runInput, any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in runInput) (*sdk.CallToolResult, any, error) {
		if err := precheck(opts, in.Args); err != nil {
			return errorResult(err), nil, nil
		}
		// --json 与 confirm 要放在 -- 之前：-- 之后的一切都是位置参数。
		head, tail := cli.SplitDashDash(stripJSONFlags(in.Args))
		args := append([]string{}, head...)
		if confirm, ok := in.Confirm.(string); ok && confirm != "" {
			args = append(args, "--confirm", confirm)
		}
		args = append(append(args, "--json"), tail...)
		var stdout, stderr bytes.Buffer
		code := cli.ExecuteContext(ctx, opts, args, &stdout, &stderr)
		if code != v1.ExitOK {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.TrimSpace(stderr.String())}}, IsError: true}, nil, nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.TrimSpace(stdout.String())}}}, nil, nil
	}
}

// explainTool 与 REST 的 explain 同一条口径：要有身份（TCP 上的 MCP 除协议握手外没有无身份的内容）。
func explainTool(t *command.Table) sdk.ToolHandlerFor[explainInput, any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in explainInput) (*sdk.CallToolResult, any, error) {
		if v1.IdentityFrom(ctx).IsAnonymous() {
			return errorResult(v1.New(v1.CodeUnauthenticated, "没有身份：请经主控本机的 unix socket 连接，或带上令牌")), nil, nil
		}
		result, err := command.Explain(t, in.Target)
		if err != nil {
			return errorResult(err), nil, nil
		}
		out, err := v1.MarshalOutput(result)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.TrimSpace(string(out))}}}, nil, nil
	}
}

func errorResult(err error) *sdk.CallToolResult {
	raw, _ := v1.AsError(err).MarshalJSON()
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(raw)}}, IsError: true}
}

// precheck 在任何东西被打开或执行之前按拒绝清单检查命令数组（清单全部从命令表推出，见 master-command-table「拒绝清单只从表推出」）。
// 只看 -- 之前的片段（-- 之后一律是位置参数）：帮助 flag、客户端专用 flag（--token、--server、--data-dir）、文件路径类参数、
// 客户端专用命令（login、mcp）、解析不到命令表里的命令、本地命令、人类专属命令。
func precheck(opts cli.Options, args []string) error {
	head, _ := cli.SplitDashDash(args)
	cmd, resolved := cli.Resolve(opts, head)
	var words []string
	for _, a := range head {
		name, isFlag := flagName(a)
		if !isFlag {
			words = append(words, a)
			continue
		}
		if name == "help" || name == "h" {
			return v1.New(v1.CodeBadRequest, "satchel_run 不提供帮助文本：看说明用 satchel_explain").
				WithNext("调用 satchel_explain，target 填命令路径或 kind 名")
		}
		for _, banned := range command.ClientOnlyFlags {
			if name == banned {
				return v1.Newf(v1.CodeBadRequest, "命令数组里不能带 --%s：它只属于 CLI 客户端，MCP 调用的身份与数据目录固定来自主控这一端", name)
			}
		}
		if isFilePathFlag(name, cmd) {
			return v1.Newf(v1.CodeBadRequest, "参数 %s 是文件路径：文件只在 CLI 本地读取、内联后发给主控，主控不读服务端路径", a)
		}
	}
	if len(words) > 0 {
		for _, banned := range command.ClientOnlyCommands {
			if words[0] == banned {
				return v1.Newf(v1.CodeBadRequest, "%s 只在 CLI 客户端里有，MCP 上不可用", words[0])
			}
		}
	}
	if !resolved {
		if len(words) == 0 {
			return v1.New(v1.CodeUsage, "用法错误：缺少命令").WithNext("调用 satchel_explain 查看全部命令")
		}
		return v1.Newf(v1.CodeUsage, "用法错误：没有命令 %s", strings.Join(words, " ")).WithNext("调用 satchel_explain 查看全部命令")
	}
	if cmd.Class == command.ClassLocal {
		return v1.Newf(v1.CodeBadRequest, "%s 是本地命令，只在主控本机的 CLI 里有，不经主控、MCP 上不可用", cmd.Name())
	}
	if cmd.Anonymous {
		return v1.Newf(v1.CodeBadRequest, "%s 是初始化向导的命令，只给人在网页或主控本机的 CLI 上用，MCP 上不可用", cmd.Name())
	}
	if cmd.HumanOnly {
		return v1.Newf(v1.CodeHumanRequired, "%s 是只有人能做的操作，MCP 上不可用", cmd.Name()).
			WithNext("在网页或主控本机的 CLI 上由管理员本人执行")
	}
	return nil
}

// flagName 认 --name、--name=value、-x、-x=value 四种写法（短名只有帮助与文件参数那几个要拦）。
func flagName(a string) (string, bool) {
	switch {
	case strings.HasPrefix(a, "--") && len(a) > 2:
		name, _, _ := strings.Cut(a[2:], "=")
		return name, true
	case strings.HasPrefix(a, "-") && len(a) >= 2 && a != "--":
		name, _, _ := strings.Cut(a[1:], "=")
		return name, true
	}
	return "", false
}

func isFilePathFlag(name string, cmd *command.Command) bool {
	for _, f := range command.FilePathFlags {
		if name == f {
			return true
		}
	}
	if cmd != nil {
		for _, f := range cmd.FileFlags() {
			if name == f {
				return true
			}
		}
	}
	return false
}

// stripJSONFlags 去掉 -- 之前的 --json / --json=true / --json=false：satchel_run 恒为 JSON，这些 flag 被接受但没有效果。
func stripJSONFlags(args []string) []string {
	head, tail := cli.SplitDashDash(args)
	out := make([]string, 0, len(args))
	for _, a := range head {
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			continue
		}
		out = append(out, a)
	}
	return append(out, tail...)
}
