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
	Args    []string `json:"args" jsonschema:"要执行的 satchel 命令数组，如 [\"audit\",\"list\",\"--limit\",\"10\"]；不经 shell，不要带 satchel 本身"`
	Confirm string   `json:"confirm,omitempty" jsonschema:"危险操作的确认字符串：删除、重启、权限、节点执行、主控自身类填对象名，批量类填本次受影响数量"`
}

type explainInput struct {
	Target string `json:"target,omitempty" jsonschema:"命令路径（如 \"audit list\"）或 kind 名（如 Task）；不给则列出全部命令与 kind"`
}

// NewHandler 建 /mcp 的 Streamable HTTP 处理器。opts 是主控进程内装配好的 CLI 选项：Remote 返回执行链本身。
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
		args := stripJSONFlags(in.Args)
		if in.Confirm != "" {
			args = append(args, "--confirm", in.Confirm)
		}
		args = append(args, "--json")
		var stdout, stderr bytes.Buffer
		code := cli.ExecuteContext(ctx, opts, args, &stdout, &stderr)
		if code != v1.ExitOK {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.TrimSpace(stderr.String())}}, IsError: true}, nil, nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.TrimSpace(stdout.String())}}}, nil, nil
	}
}

func explainTool(t *command.Table) sdk.ToolHandlerFor[explainInput, any] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in explainInput) (*sdk.CallToolResult, any, error) {
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

// precheck 在任何东西被打开或执行之前按拒绝清单检查命令数组（清单全部从命令表推出，见 master-command-table「拒绝清单只从表推出」）：
// 客户端专用 flag（--token、--server）、文件路径类参数、本地命令、人类专属命令。
func precheck(opts cli.Options, args []string) error {
	cmd, resolved := cli.Resolve(opts, args)
	for _, a := range args {
		if a == "--" {
			break
		}
		name, ok := flagName(a)
		if !ok {
			continue
		}
		for _, banned := range command.ClientOnlyFlags {
			if name == banned {
				return v1.Newf(v1.CodeBadRequest, "命令数组里不能带 --%s：MCP 调用的身份固定来自这次连接的认证，不能在命令里改", name)
			}
		}
		if isFilePathFlag(name, cmd) {
			return v1.Newf(v1.CodeBadRequest, "参数 %s 是文件路径：文件只在 CLI 本地读取、内联后发给主控，主控不读服务端路径", a)
		}
	}
	if !resolved {
		return nil // 交给 cobra 出 usage
	}
	if cmd.Class == command.ClassLocal {
		return v1.Newf(v1.CodeBadRequest, "%s 是本地命令，只在主控本机的 CLI 里有，不经主控、MCP 上不可用", cmd.Name())
	}
	if cmd.HumanOnly {
		return v1.Newf(v1.CodeHumanRequired, "%s 是只有人能做的操作，MCP 上不可用", cmd.Name()).
			WithNext("在网页或主控本机的 CLI 上由管理员本人执行")
	}
	return nil
}

// flagName 认三种写法：--name、--name=value、-f（短名只有文件参数那一种要拦）。
func flagName(a string) (string, bool) {
	switch {
	case strings.HasPrefix(a, "--") && len(a) > 2:
		name, _, _ := strings.Cut(a[2:], "=")
		return name, true
	case strings.HasPrefix(a, "-") && len(a) == 2:
		return a[1:], true
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

// stripJSONFlags 去掉命令数组里的 --json / --json=true / --json=false：satchel_run 恒为 JSON，这些 flag 被接受但没有效果。
func stripJSONFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			continue
		}
		out = append(out, a)
	}
	return out
}
