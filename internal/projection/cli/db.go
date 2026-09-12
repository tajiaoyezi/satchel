package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/spf13/cobra"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/schema"
)

// db 子命令直接调 base/db：迁移没有业务逻辑，不为它造 service。serve 启动时用同一个函数。
func newDBCommand(opts *options) *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "db",
		Short: "数据库：执行迁移、查看迁移状态、清除迁移锁",
	}
	cmd.PersistentFlags().StringVar(&dataDir, "data-dir", "",
		"数据目录（默认取环境变量 "+db.EnvDataDir+"，再默认 "+db.DefaultDataDir+"）")
	cmd.AddCommand(newDBMigrateCommand(opts, &dataDir), newDBStatusCommand(opts, &dataDir), newDBUnlockCommand(opts, &dataDir))
	return cmd
}

// loadConfig 解析数据目录并读配置，不创建任何东西。
func loadConfig(dataDir string) (string, db.Config, error) {
	dir := db.DataDir(dataDir)
	cfg, err := db.LoadConfig(dir)
	return dir, cfg, err
}

// openForWrite 为会写库的命令打开数据库：SQLite 模式先建数据目录，postgres 模式不碰它。
func openForWrite(ctx context.Context, dataDir string) (*bun.DB, error) {
	dir, cfg, err := loadConfig(dataDir)
	if err != nil {
		return nil, err
	}
	if cfg.Driver == db.DriverSQLite {
		if err := db.EnsureDataDir(dir); err != nil {
			return nil, err
		}
	}
	return db.Open(ctx, cfg)
}

// sqliteFileMissing 报告 SQLite 模式下库文件是否还不存在（不存在就不能打开，打开会把它建出来）。
func sqliteFileMissing(cfg db.Config) bool {
	if cfg.Driver != db.DriverSQLite {
		return false
	}
	_, err := os.Stat(cfg.Path)
	return errors.Is(err, fs.ErrNotExist)
}

type migrateOutput struct {
	Applied []string `json:"applied"`
}

func newDBMigrateCommand(opts *options, dataDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "执行全部未应用的迁移，然后比对库结构与注册表",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bdb, err := openForWrite(cmd.Context(), *dataDir)
			if err != nil {
				return err
			}
			defer bdb.Close()
			applied, err := db.Migrate(cmd.Context(), bdb) // 迁移完自带结构比对，不一致时返回 schema_mismatch
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), migrateOutput{Applied: applied})
			}
			if len(applied) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "没有待应用的迁移；库结构与注册表一致")
				return err
			}
			return printList(cmd.OutOrStdout(), "本次应用：", applied)
		},
	}
}

func newDBStatusCommand(opts *options, dataDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "列出已应用与待应用的迁移，并比对库结构（不写盘）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cfg, err := loadConfig(*dataDir)
			if err != nil {
				return err
			}
			var st db.MigrationStatus
			if sqliteFileMissing(cfg) {
				// 库文件还没有：不打开（打开会建文件），按内置清单报全部待应用。
				names, err := db.MigrationNames(schema.SQLite)
				if err != nil {
					return err
				}
				st = db.MigrationStatus{Applied: []string{}, Pending: names, Schema: db.SchemaCheck{Diff: []string{}, ExtraTables: []string{}}}
			} else {
				bdb, err := db.Open(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				defer bdb.Close()
				if st, err = db.Status(cmd.Context(), bdb); err != nil {
					return err
				}
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), st)
			}
			return printStatus(cmd.OutOrStdout(), st)
		},
	}
}

func newDBUnlockCommand(opts *options, dataDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "清除上一次迁移被中断后残留的迁移锁",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cfg, err := loadConfig(*dataDir)
			if err != nil {
				return err
			}
			// 清锁是幂等的：库文件还不存在就没有锁，也算清干净了，不当错误。
			if !sqliteFileMissing(cfg) {
				bdb, err := db.Open(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				defer bdb.Close()
				if err := db.Unlock(cmd.Context(), bdb); err != nil {
					return err
				}
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Unlocked bool `json:"unlocked"`
				}{true})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "迁移锁已清除")
			return err
		},
	}
}

// printStatus 是 db status 的文本输出：迁移清单、记账是否缺失、结构比对的三种结果与多出来的表。
func printStatus(w io.Writer, st db.MigrationStatus) error {
	if err := printList(w, "已应用：", st.Applied); err != nil {
		return err
	}
	if err := printList(w, "待应用：", st.Pending); err != nil {
		return err
	}
	if st.BookkeepingMissing {
		if _, err := fmt.Fprintln(w, "迁移记账：缺失（库里已有表，但 "+db.MigrationsTable+" 表不存在；按旧结构建的库或被手工改过，首个正式发布前删库重建）"); err != nil {
			return err
		}
	}
	var err error
	switch {
	case !st.Schema.Checked:
		_, err = fmt.Fprintln(w, "库结构：还有迁移没应用，未比对")
	case *st.Schema.Consistent:
		_, err = fmt.Fprintln(w, "库结构：与注册表一致")
	default:
		err = printList(w, fmt.Sprintf("库结构：与注册表有 %d 处不一致（首个正式发布前删库重建，之后写新迁移）：", len(st.Schema.Diff)), st.Schema.Diff)
	}
	if err != nil {
		return err
	}
	if len(st.Schema.ExtraTables) > 0 {
		return printList(w, "库里有、注册表里没有的表（不算错误，Satchel 不管它们）：", st.Schema.ExtraTables)
	}
	return nil
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
