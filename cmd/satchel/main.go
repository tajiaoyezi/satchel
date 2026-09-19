package main

import (
	"fmt"
	"io"
	"os"

	"github.com/satchel/satchel/internal/projection/cli"
)

// options 是 satchel 二进制的完整装配：默认的 CLI 选项加上只有装配根才能装的 serve。
func options() cli.Options {
	opts := cli.DefaultOptions()
	opts.Local["serve"] = serveCommand
	opts.Renderers["serve"] = func(w io.Writer, _ any) error {
		_, err := fmt.Fprintln(w, "主控已停止")
		return err
	}
	return opts
}

func main() {
	os.Exit(cli.Execute(options(), os.Args[1:], os.Stdout, os.Stderr))
}
