package backup

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Source 是生成一份备份要的东西：数据目录、打开的库与它的驱动；PostgreSQL 另要连接参数（给 pg_dump）。
type Source struct {
	DataDir string
	DB      *bun.DB
	Driver  db.Driver
	PG      *PGConn
	Version string
}

// Create 在 backups/ 生成一份备份（master-backup「备份的内容与格式」）：先写 . 开头的临时文件，写完、fsync、0600 之后改名，
// 所以列出备份时永远看不到写了一半的。prefix 是 PrefixBackup 或 PrefixBeforeRestore。
func Create(ctx context.Context, src Source, prefix string) (Info, error) {
	dir := Dir(src.DataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "建备份目录失败", err)
	}
	st, err := db.Status(ctx, src.DB)
	if err != nil {
		return Info{}, err
	}
	m := Manifest{Format: Format, CreatedAt: time.Now().UTC().Truncate(time.Second), Driver: src.Driver, Migrations: st.Applied, Version: src.Version}
	var pgDump string
	if src.Driver == db.DriverPostgres {
		if pgDump, err = findPGDump(ctx, src.DB); err != nil {
			return Info{}, err
		}
		if m.Schema, err = currentSchema(ctx, src.DB); err != nil {
			return Info{}, err
		}
	}
	name := NewName(dir, prefix, m.CreatedAt)
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "建备份的临时文件失败", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	zw := zip.NewWriter(tmp)
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := writeEntry(zw, EntryManifest, bytes.NewReader(raw)); err != nil {
		return Info{}, err
	}
	if src.Driver == db.DriverPostgres {
		err = dumpPostgres(ctx, zw, pgDump, src.PG, m.Schema)
	} else {
		err = dumpSQLite(ctx, zw, src.DB, dir)
	}
	if err != nil {
		return Info{}, err
	}
	if err := addDataFiles(zw, src.DataDir); err != nil {
		return Info{}, err
	}
	if err := zw.Close(); err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "写备份失败", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "设置备份权限失败", err)
	}
	if err := tmp.Sync(); err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "写备份失败", err)
	}
	info, _ := tmp.Stat()
	if err := tmp.Close(); err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "写备份失败", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "写备份失败", err)
	}
	if err := SyncDir(dir); err != nil {
		return Info{}, v1.Wrap(v1.CodeInternal, "写备份失败", err)
	}
	created := m.CreatedAt
	return Info{Name: name, Size: info.Size(), CreatedAt: &created, Driver: m.Driver}, nil
}

func writeEntry(zw *zip.Writer, name string, r io.Reader) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "写备份失败", err)
	}
	if _, err := io.Copy(w, r); err != nil {
		return v1.Wrap(v1.CodeInternal, "写备份失败："+name, err)
	}
	return nil
}

// dumpSQLite 用 VACUUM INTO 导出一份一致的单文件库，再在这份拷贝里清空会话表，写进 database/satchel.db。
func dumpSQLite(ctx context.Context, zw *zip.Writer, bdb *bun.DB, dir string) error {
	f, err := os.CreateTemp(dir, ".vacuum-*.db")
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "建导出用的临时文件失败", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path) // VACUUM INTO 要求目标不存在
	defer removeSQLiteFiles(path)
	if _, err := bdb.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return v1.Wrap(v1.CodeDatabase, "导出 SQLite 库失败", err)
	}
	copyDB, err := db.OpenDSN(ctx, db.DriverSQLite, db.SQLiteDSN(path))
	if err != nil {
		return err
	}
	_, err = copyDB.ExecContext(ctx, "DELETE FROM sessions")
	if cerr := copyDB.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "清空导出库里的会话失败", err)
	}
	src, err := os.Open(path)
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "读导出的库失败", err)
	}
	defer src.Close()
	return writeEntry(zw, EntrySQLite, src)
}

func removeSQLiteFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(path + suffix)
	}
}

// dumpPostgres 用 pg_dump 导出当前 schema 的纯 SQL（不带会话表的数据、属主与权限），写进 database/postgres.sql。
// 导出时去掉 CREATE SCHEMA 那一句（恢复时由 psql 先删后建）与 transaction_timeout 的设置（新 pg_dump 才有，旧服务器不认）。
func dumpPostgres(ctx context.Context, zw *zip.Writer, pgDump string, conn *PGConn, schema string) error {
	cmd := exec.CommandContext(ctx, pgDump, "--format=plain", "--no-owner", "--no-privileges",
		"--schema="+quoteIdent(schema), "--exclude-table-data="+quoteIdent(schema)+".sessions")
	cmd.Env = conn.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "启动 pg_dump 失败", err)
	}
	if err := cmd.Start(); err != nil {
		return v1.Wrap(v1.CodeUnavailable, "启动 pg_dump 失败", err)
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: EntryPostgres, Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return v1.Wrap(v1.CodeInternal, "写备份失败", err)
	}
	ferr := filterDump(w, out, schema)
	werr := cmd.Wait()
	if werr != nil {
		return v1.Newf(v1.CodeUnavailable, "pg_dump 失败：%s", strings.TrimSpace(stderr.String()))
	}
	if ferr != nil {
		return v1.Wrap(v1.CodeInternal, "写备份失败", ferr)
	}
	return nil
}

func filterDump(w io.Writer, r io.Reader, schema string) error {
	skip := map[string]bool{
		"CREATE SCHEMA " + schema + ";":             true,
		"CREATE SCHEMA " + quoteIdent(schema) + ";": true,
		"SET transaction_timeout = 0;":              true,
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && !skip[string(bytes.TrimRight(line, "\r\n"))] {
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// quoteIdent 给标识符加双引号（里面的双引号写两遍）。
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// addDataFiles 把数据目录里进备份的文件与目录写进 ZIP：database.json、config.yaml、master.key（存在时），
// subscribes/ 与 rule_templates/ 下的普通文件（. 开头的不进），路径相对数据目录。
func addDataFiles(zw *zip.Writer, dataDir string) error {
	for _, name := range dataFiles {
		if err := addFile(zw, dataDir, name); err != nil {
			return err
		}
	}
	for _, d := range dataDirs {
		root := filepath.Join(dataDir, d)
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return filepath.SkipDir
			}
			if err != nil {
				return err
			}
			if strings.HasPrefix(e.Name(), ".") && path != root {
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !e.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(dataDir, path)
			return addFile(zw, dataDir, filepath.ToSlash(rel))
		})
		if err != nil {
			return v1.Wrap(v1.CodeInternal, "打包 "+d+" 失败", err)
		}
	}
	return nil
}

func addFile(zw *zip.Writer, dataDir, rel string) error {
	f, err := os.Open(filepath.Join(dataDir, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "打包 "+rel+" 失败", err)
	}
	defer f.Close()
	return writeEntry(zw, rel, f)
}
