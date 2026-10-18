package main

import (
	"context"
	"fmt"
	"io"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/cli"
	"github.com/satchel/satchel/internal/projection/mcp"
	"github.com/satchel/satchel/internal/projection/web"
)

// options 是 satchel 二进制的完整装配：默认的 CLI 选项加上只有装配根才能装的 serve 与 mcp stdio
// （mcp 投影依赖 cli 投影，垫片的处理函数只能在这里登记）。
func options() cli.Options {
	opts := cli.DefaultOptions()
	opts.Local["serve"] = serveCommand
	opts.Renderers["serve"] = func(w io.Writer, _ any) error {
		_, err := fmt.Fprintln(w, "主控已停止")
		return err
	}
	// stdout 只走 MCP 协议：处理函数返回 nil 结果，CLI 什么都不渲染；启动失败的四字段错误照常写 stderr。
	opts.Local["mcp stdio"] = func(ctx context.Context, _ *command.Invocation) (any, error) {
		conn, err := cli.Connect(ctx)
		if err != nil {
			return nil, err
		}
		return nil, mcp.Stdio(ctx, conn, &sdk.StdioTransport{})
	}
	return opts
}

func main() {
	// 错误提示里的网页入口按编进二进制的页面清单来（master-web）：在执行任何命令之前设置一次。
	if m, err := web.Embedded(); err == nil {
		command.SetWebEntries(m.Entries())
	}
	os.Exit(cli.Execute(options(), os.Args[1:], os.Stdout, os.Stderr))
}
