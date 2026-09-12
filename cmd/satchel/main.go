// satchel 的装配根：new 各层、注入依赖、起服务。
// 现在只有 CLI 根命令；业务不在这里写。
package main

import (
	"os"

	"github.com/satchel/satchel/internal/projection/cli"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		// cobra 已把错误打到 stderr，这里只按第 05 章的退出码表定退出码。
		os.Exit(v1.ExitCodeOf(err))
	}
}
