// gen 从表注册表生成两套迁移 SQL、bun 模型与 kind 结构体，由 go generate ./internal/base/schema/ 触发。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/satchel/satchel/internal/base/schema"
)

func main() {
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	outputs, err := schema.Outputs(schema.Default())
	if err != nil {
		fatal(err)
	}
	for rel, content := range outputs {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("已生成", rel)
	}
}

// repoRoot 从当前目录向上找 go.mod 所在的目录。
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("从 %s 向上没有找到 go.mod", dir)
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gen:", err)
	os.Exit(1)
}
