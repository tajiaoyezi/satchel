package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
)

// db 子命令直接调 base/db：迁移没有业务逻辑，不为它造 service。serve 启动时用同一个函数。
func newDBCommand(opts *options) *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "db",
		Short: "数据库：执行迁移、查看迁移状态",
	}
	cmd.PersistentFlags().StringVar(&dataDir, "data-dir", "",
		"数据目录（默认取环境变量 "+db.EnvDataDir+"，再默认 "+db.DefaultDataDir+"）")
	cmd.AddCommand(newDBMigrateCommand(opts, &dataDir), newDBStatusCommand(opts, &dataDir))
	return cmd
}

// openDB 解析数据目录、建目录、读配置、打开库。
func openDB(ctx context.Context, dataDir string) (*bun.DB, error) {
	dir := db.DataDir(dataDir)
	if err := db.EnsureDataDir(dir); err != nil {
		return nil, err
	}
	cfg, err := db.LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	return db.Open(ctx, cfg)
}

func newDBMigrateCommand(opts *options, dataDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "执行全部未应用的迁移",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bdb, err := openDB(cmd.Context(), *dataDir)
			if err != nil {
				return err
			}
			defer bdb.Close()
			applied, err := db.Migrate(cmd.Context(), bdb)
			if err != nil {
				return err
			}
			if applied == nil {
				applied = []string{}
			}
			if opts.json {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string][]string{"applied": applied})
			}
			if len(applied) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "没有待应用的迁移")
				return err
			}
			return printList(cmd.OutOrStdout(), "本次应用：", applied)
		},
	}
}

func newDBStatusCommand(opts *options, dataDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "列出已应用与待应用的迁移",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bdb, err := openDB(cmd.Context(), *dataDir)
			if err != nil {
				return err
			}
			defer bdb.Close()
			st, err := db.Status(cmd.Context(), bdb)
			if err != nil {
				return err
			}
			if opts.json {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(st)
			}
			if err := printList(cmd.OutOrStdout(), "已应用：", st.Applied); err != nil {
				return err
			}
			return printList(cmd.OutOrStdout(), "待应用：", st.Pending)
		},
	}
}

func printList(w io.Writer, title string, items []string) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, title+"（无）")
		return err
	}
	if _, err := fmt.Fprintln(w, title); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintln(w, "  "+item); err != nil {
			return err
		}
	}
	return nil
}
