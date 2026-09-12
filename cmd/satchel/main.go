// satchel 的装配根：new 各层、注入依赖、起服务。
// 现在只有 CLI 根命令；业务不在这里写。
package main

import (
	"os"

	"github.com/satchel/satchel/internal/projection/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
