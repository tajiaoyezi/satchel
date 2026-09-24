package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satchel/satchel/internal/base/buildinfo"
	"github.com/satchel/satchel/internal/projection/cli"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Stdio 是 satchel mcp stdio 的垫片（master-mcp「stdio 垫片」）：按 conn 连上主控的 /mcp，先调一次 satchel_explain
// 确认身份有效（tools/list 不要身份，发现不了令牌已吊销），再取主控的工具清单，在 transport 上起一个 MCP 服务：
// 每个工具原样登记，调用时用原始参数转发给主控、结果原样交回。垫片自己不判权限。
// 启动失败返回四字段错误（调用方写 stderr、按错误码退出），此时 transport 上没有写过任何东西；transport 关闭（stdin 结束）时返回 nil。
func Stdio(ctx context.Context, conn cli.Connection, transport sdk.Transport) error {
	client := sdk.NewClient(&sdk.Implementation{Name: "satchel-stdio", Version: buildinfo.Version}, nil)
	// 主控的 /mcp 是无状态的、只回 JSON，没有服务端主动发的消息：不开常驻的 SSE 流。
	upstream, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: conn.BaseURL() + Path, HTTPClient: conn.HTTPClient(), DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return unreachable(conn, err)
	}
	defer upstream.Close()
	check, err := upstream.CallTool(ctx, &sdk.CallToolParams{Name: "satchel_explain", Arguments: map[string]any{"target": "whoami"}})
	if err != nil {
		return unreachable(conn, err)
	}
	if check.IsError {
		return resultError(check)
	}
	tools, err := upstream.ListTools(ctx, nil)
	if err != nil {
		return unreachable(conn, err)
	}
	info := upstream.InitializeResult()
	server := sdk.NewServer(info.ServerInfo, &sdk.ServerOptions{Instructions: info.Instructions})
	for _, tool := range tools.Tools {
		name := tool.Name
		server.AddTool(tool, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var args any = map[string]any{}
			if raw := req.Params.Arguments; len(raw) > 0 {
				args = raw
			}
			res, err := upstream.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				// 半路连不上主控：交回一个四字段的工具错误，runtime 能读懂，垫片继续服务。
				return errorResult(unreachable(conn, err)), nil
			}
			return res, nil
		})
	}
	if err := server.Run(ctx, transport); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		return v1.Wrap(v1.CodeInternal, "stdio 上的 MCP 服务异常结束", err)
	}
	return nil
}

// unreachable 把连主控的 MCP 失败包成 unavailable：reason 说明连的是哪个主控，next 按连法给提示（与 CLI 客户端同一套）。
func unreachable(conn cli.Connection, err error) error {
	where := "本机的主控（unix socket " + conn.Socket + "）"
	next := "主控没在运行？在主控本机执行 systemctl status satchel 或 docker compose ps 查看"
	if conn.Server != "" {
		where = "主控 " + conn.Server
		next = "检查主控地址是否正确（--server、" + cli.EnvServer + " 或登录文件）、主控是否在运行、网络与防火墙是否放行"
	}
	return v1.Wrap(v1.CodeUnavailable, "连不上"+where+"的 MCP 接口", err).WithNext(next)
}

// resultError 把主控交回的工具错误（文本是四字段错误的 JSON）还原成错误。
func resultError(res *sdk.CallToolResult) error {
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			text.WriteString(t.Text)
		}
	}
	var e v1.Error
	if err := json.Unmarshal([]byte(text.String()), &e); err != nil || e.Code == "" {
		return v1.Newf(v1.CodeInternal, "主控的 MCP 接口返回了看不懂的错误：%s", text.String())
	}
	return &e
}
