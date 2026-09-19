package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/buildinfo"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
)

// 本地命令的处理函数与文本渲染：version、db migrate / status / unlock、__verify。
// 它们不经主控：db 直接调 base/db（迁移没有业务逻辑，不为它造 service），serve 起主控时用同一个函数。

func registerBuiltins(opts *Options) {
	opts.Local["version"] = func(context.Context, *command.Invocation) (any, error) { return buildinfo.Get(), nil }
	opts.Renderers["version"] = func(w io.Writer, result any) error {
		info := result.(buildinfo.Info)
		_, err := fmt.Fprintf(w, "satchel %s\ncommit: %s\nbuilt: %s\n", info.Version, info.Commit, info.Date)
		return err
	}
	opts.Local["db migrate"] = dbMigrate
	opts.Renderers["db migrate"] = renderMigrate
	opts.Local["db status"] = dbStatus
	opts.Renderers["db status"] = func(w io.Writer, result any) error { return printStatus(w, result.(db.MigrationStatus)) }
	opts.Local["db unlock"] = dbUnlock
	opts.Renderers["db unlock"] = func(w io.Writer, _ any) error {
		_, err := fmt.Fprintln(w, "迁移锁已清除")
		return err
	}
	opts.Local["__verify"] = verify
	opts.Renderers["__verify"] = func(w io.Writer, result any) error {
		_, err := fmt.Fprintf(w, "验签通过 %s\n", result.(verifyOutput).File)
		return err
	}
	opts.Renderers["explain"] = renderExplain
	opts.Renderers["whoami"] = renderWhoami
}

// OpenForWrite 为会写库的命令打开数据库：先确保数据目录与子目录存在（两种驱动都一样——
// 主控密钥、订阅文件、规则模板在 postgres 模式下也在这个目录里），再开库。只读命令不建目录。serve 也用它。
func OpenForWrite(ctx context.Context, dataDir string) (*bun.DB, error) {
	cfg, err := db.LoadConfig(dataDir)
	if err != nil {
		return nil, err
	}
	if err := db.EnsureDataDir(dataDir); err != nil {
		return nil, err
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

func dbMigrate(ctx context.Context, _ *command.Invocation) (any, error) {
	bdb, err := OpenForWrite(ctx, DataDir(ctx))
	if err != nil {
		return nil, err
	}
	defer bdb.Close()
	applied, err := db.Migrate(ctx, bdb) // 迁移完自带结构比对，不一致时返回 schema_mismatch
	if err != nil {
		return nil, err
	}
	if applied == nil {
		applied = []string{}
	}
	return migrateOutput{Applied: applied}, nil
}

func renderMigrate(w io.Writer, result any) error {
	out := result.(migrateOutput)
	if len(out.Applied) == 0 {
		_, err := fmt.Fprintln(w, "没有待应用的迁移；库结构与注册表一致")
		return err
	}
	return printList(w, "本次应用：", out.Applied)
}

func dbStatus(ctx context.Context, _ *command.Invocation) (any, error) {
	cfg, err := db.LoadConfig(DataDir(ctx))
	if err != nil {
		return nil, err
	}
	if sqliteFileMissing(cfg) {
		// 库文件还没有：不打开（打开会建文件），按内置清单报全部待应用。
		names, err := db.MigrationNames(schema.SQLite)
		if err != nil {
			return nil, err
		}
		return db.MigrationStatus{Applied: []string{}, Pending: names, Schema: db.SchemaCheck{Diff: []string{}, ExtraTables: []string{}}}, nil
	}
	bdb, err := db.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer bdb.Close()
	return db.Status(ctx, bdb)
}

type unlockOutput struct {
	Unlocked bool `json:"unlocked"`
}

func dbUnlock(ctx context.Context, _ *command.Invocation) (any, error) {
	cfg, err := db.LoadConfig(DataDir(ctx))
	if err != nil {
		return nil, err
	}
	// 清锁是幂等的：库文件还不存在就没有锁，也算清干净了，不当错误。
	if !sqliteFileMissing(cfg) {
		bdb, err := db.Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		defer bdb.Close()
		if err := db.Unlock(ctx, bdb); err != nil {
			return nil, err
		}
	}
	return unlockOutput{Unlocked: true}, nil
}

type verifyOutput struct {
	File     string `json:"file"`
	Verified bool   `json:"verified"`
}

// verify 是隐藏的 __verify：用编进二进制的发布公钥清单验一个文件的 Ed25519 分离签名。
// 安装脚本与自升级用它，装机器上不用再带别的验签工具；不出现在帮助里（release-pipeline）。
func verify(_ context.Context, inv *command.Invocation) (any, error) {
	file, sig := inv.Arg(0), inv.Arg(1)
	if err := release.VerifyFile(file, sig); err != nil {
		if errors.Is(err, release.ErrBadSignature) {
			return nil, v1.Wrap(v1.CodeBadRequest, "签名与发布公钥不匹配："+file, err).
				WithNext("确认下载的文件与 .sig 来自同一个 Release，或文件在下载中被改动")
		}
		return nil, v1.Wrap(v1.CodeBadRequest, "验签失败："+file, err)
	}
	return verifyOutput{File: file, Verified: true}, nil
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

// renderWhoami 是 whoami 的文本形式。
func renderWhoami(w io.Writer, result any) error {
	fields, err := toFields(result)
	if err != nil {
		return err
	}
	return renderFields(w, fields)
}
