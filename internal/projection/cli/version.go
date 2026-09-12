package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/satchel/satchel/internal/base/buildinfo"
)

func newVersionCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印版本号、commit 与构建时间",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "satchel %s\ncommit: %s\nbuilt: %s\n", info.Version, info.Commit, info.Date)
			return err
		},
	}
}
