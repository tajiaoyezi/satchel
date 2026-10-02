// gen 把命令表生成「命令 × scope 对照表」docs/commands.md，把命令表与 skills 生成「命令 × skills 对照表」docs/skills.md。
// 由 go generate ./internal/command/ 调用，工作目录是 internal/command，输出写到模块根的 docs/。
// skills 的检查不通过时两份都不写。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/satchel/satchel/internal/base/skills"
	"github.com/satchel/satchel/internal/command"
)

func main() {
	skillsDoc, err := skills.Generate(command.Catalog(), skills.Files())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, doc := range []struct {
		path    string
		content []byte
	}{
		{command.DocsPath, command.GenerateDocs(command.Catalog())},
		{skills.DocsPath, skillsDoc},
	} {
		out := filepath.Join("..", "..", filepath.FromSlash(doc.path))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(out, doc.content, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("已生成 " + doc.path)
	}
}
