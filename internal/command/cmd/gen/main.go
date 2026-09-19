// gen 把命令表生成「命令 × scope 对照表」docs/commands.md。由 go generate ./internal/command/ 调用，
// 工作目录是 internal/command，输出写到模块根的 docs/。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/satchel/satchel/internal/command"
)

func main() {
	out := filepath.Join("..", "..", filepath.FromSlash(command.DocsPath))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, command.GenerateDocs(command.Catalog()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("已生成 " + command.DocsPath)
}
