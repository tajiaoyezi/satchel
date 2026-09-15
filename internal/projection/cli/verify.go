package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
)

// newVerifyCommand 是隐藏的 __verify：用编进二进制的发布公钥清单验一个文件的 Ed25519 分离签名。
// 安装脚本与 M1 的自升级用它，装机器上不用再带别的验签工具；不出现在帮助里（release-pipeline）。
func newVerifyCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:    "__verify <file> <sig>",
		Short:  "用发布公钥验签（内部命令）",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := release.VerifyFile(args[0], args[1]); err != nil {
				if errors.Is(err, release.ErrBadSignature) {
					return v1.Wrap(v1.CodeBadRequest, "签名与发布公钥不匹配："+args[0], err).
						WithNext("确认下载的文件与 .sig 来自同一个 Release，或文件在下载中被改动")
				}
				return v1.Wrap(v1.CodeBadRequest, "验签失败："+args[0], err)
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"file": args[0], "verified": true})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "验签通过 %s\n", args[0])
			return err
		},
	}
}
