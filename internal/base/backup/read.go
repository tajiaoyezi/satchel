package backup

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// maxManifestBytes 是清单的上限：一个正常的清单只有几百字节。
const maxManifestBytes = 1 << 20

// maxExtract 是一份备份解压后的总体积上限（16 GiB；变量只为测试能调小）：校验时按条目头里声明的大小先挡一次，
// 解压时边写边数、不信头里的数字，超了就删掉临时文件——压缩得很小、解开很大的包不能把磁盘写满。
var maxExtract int64 = 16 << 30

func extractTooBig() error {
	return v1.Newf(v1.CodeBadRequest, "备份解压后超过 %d 字节的上限", maxExtract)
}

// readManifest 打开一份备份、读出清单；zr 由调用方关。
func readManifest(path string) (*zip.ReadCloser, *Manifest, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, nil, v1.Wrap(v1.CodeBadRequest, "备份不是能打开的 ZIP", err)
	}
	f := findEntry(&zr.Reader, EntryManifest)
	if f == nil {
		zr.Close()
		return nil, nil, v1.New(v1.CodeBadRequest, "备份里没有 manifest.json")
	}
	rc, err := f.Open()
	if err != nil {
		zr.Close()
		return nil, nil, v1.Wrap(v1.CodeBadRequest, "读不了 manifest.json", err)
	}
	defer rc.Close()
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(rc, maxManifestBytes)).Decode(&m); err != nil {
		zr.Close()
		return nil, nil, v1.Wrap(v1.CodeBadRequest, "manifest.json 不是合法的 JSON", err)
	}
	return zr, &m, nil
}

func findEntry(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// Validate 校验一份备份（master-backup「备份的校验」）：ZIP 能打开、清单格式认识、数据库条目与清单的驱动对得上、
// 迁移不比本二进制新、条目路径都在允许的几项之内——任一条不过是 bad_request；驱动与当前的不同是 conflict。
// 返回读出的清单。
func Validate(path string, driver db.Driver) (*Manifest, error) {
	zr, m, err := readManifest(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	if m.Format != Format {
		return nil, v1.Newf(v1.CodeBadRequest, "备份清单的格式 %q 不认识（应当是 %s）", m.Format, Format)
	}
	want := map[db.Driver]string{db.DriverSQLite: EntrySQLite, db.DriverPostgres: EntryPostgres}[m.Driver]
	if want == "" {
		return nil, v1.Newf(v1.CodeBadRequest, "备份清单的驱动 %q 不认识", m.Driver)
	}
	if findEntry(&zr.Reader, want) == nil {
		return nil, v1.Newf(v1.CodeBadRequest, "备份清单说驱动是 %s，却没有 %s", m.Driver, want)
	}
	var total uint64
	for _, f := range zr.File {
		if !entryAllowed(f.Name) {
			return nil, v1.Newf(v1.CodeBadRequest, "备份里有不允许的条目 %q", f.Name).WithNext("备份只能含清单、数据库、database.json、config.yaml、master.key、subscribes/ 与 rule_templates/")
		}
		total += f.UncompressedSize64
	}
	if total > uint64(maxExtract) {
		return nil, extractTooBig()
	}
	known, err := db.MigrationNames(dialectOf(m.Driver))
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, n := range known {
		have[n] = true
	}
	for _, n := range m.Migrations {
		if !have[n] {
			return nil, v1.Newf(v1.CodeBadRequest, "备份里有本主控不认识的迁移 %s：这份备份比本主控新", n).WithNext("先把主控升级到生成这份备份的版本（" + m.Version + "）或更新，再恢复")
		}
	}
	if m.Driver != driver {
		return nil, v1.Newf(v1.CodeConflict, "这份备份是 %s 的，当前主控用的是 %s：不支持跨驱动恢复", m.Driver, driver).
			WithNext("在同驱动的主控上恢复，之后用数据库在线迁移换驱动")
	}
	return m, nil
}

// List 列出 backups/ 里的备份（. 开头的临时文件不算），按修改时间从新到旧；清单读不出的 created_at 与 driver 为空。
func List(dataDir string) ([]Info, error) {
	dir := Dir(dataDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "列备份目录失败", err)
	}
	type item struct {
		info Info
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		info := Info{Name: e.Name(), Size: fi.Size()}
		if zr, m, err := readManifest(filepath.Join(dir, e.Name())); err == nil {
			created := m.CreatedAt
			info.CreatedAt, info.Driver = &created, m.Driver
			zr.Close()
		}
		items = append(items, item{info, fi.ModTime()})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	out := make([]Info, 0, len(items))
	for _, it := range items {
		out = append(out, it.info)
	}
	return out, nil
}

// Prune 让 backups/ 里的 .zip 只留修改时间最新的 keep 份，多的删掉；protect 里的名字不删（正在被一次恢复引用的那份）。
func Prune(dataDir string, keep int, protect ...string) error {
	list, err := List(dataDir)
	if err != nil {
		return err
	}
	kept := 0
	for _, b := range list {
		protected := false
		for _, p := range protect {
			protected = protected || p == b.Name
		}
		if kept < keep || protected {
			kept++
			continue
		}
		if err := os.Remove(filepath.Join(Dir(dataDir), b.Name)); err != nil && !os.IsNotExist(err) {
			return v1.Wrap(v1.CodeInternal, "删除旧备份 "+b.Name+" 失败", err)
		}
	}
	return nil
}

// Path 返回 backups/ 里一份备份的路径；名字不是 List 列出的一项（含 /、..、. 开头）时是 not_found。
func Path(dataDir, name string) (string, error) {
	list, err := List(dataDir)
	if err != nil {
		return "", err
	}
	for _, b := range list {
		if b.Name == name {
			return filepath.Join(Dir(dataDir), name), nil
		}
	}
	return "", v1.Newf(v1.CodeNotFound, "没有备份 %q", name).WithNext("用 backup list 看有哪些")
}

// LatestUsable 从 backups/ 里按修改时间从新到旧找第一份能用来自动恢复 SQLite 的备份（跳过恢复前自动生成的那种）：
// 校验通过，库文件解出来也通过 quick_check。没有时返回空名字。
func LatestUsable(ctx context.Context, dataDir string) (string, error) {
	list, err := List(dataDir)
	if err != nil {
		return "", err
	}
	for _, b := range list {
		if strings.HasPrefix(b.Name, PrefixBeforeRestore) {
			continue
		}
		path := filepath.Join(Dir(dataDir), b.Name)
		if _, err := Validate(path, db.DriverSQLite); err != nil {
			continue
		}
		tmp, err := extractSQLite(path, Dir(dataDir))
		if err != nil {
			continue
		}
		err = quickCheckFile(ctx, tmp)
		removeSQLiteFiles(tmp)
		if err == nil {
			return b.Name, nil
		}
	}
	return "", nil
}

// extractSQLite 把备份里的库解到 dir 下的临时文件，返回路径。
func extractSQLite(path, dir string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	f := findEntry(&zr.Reader, EntrySQLite)
	if f == nil {
		return "", fmt.Errorf("没有 %s", EntrySQLite)
	}
	return extractTo(f, dir, ".restore-*.db")
}

// extractTo 把一个条目解到 dir 下按 pattern 取名的临时文件（0600），返回路径。
func extractTo(f *zip.File, dir, pattern string) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	out, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(out, io.LimitReader(rc, maxExtract+1))
	if err == nil && n > maxExtract {
		err = extractTooBig()
	}
	if err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), out.Close()
}

// quickCheckFile 打开一个 SQLite 文件跑 quick_check。
func quickCheckFile(ctx context.Context, path string) error {
	bdb, err := db.OpenDSN(ctx, db.DriverSQLite, db.SQLiteDSN(path))
	if err != nil {
		return err
	}
	defer bdb.Close()
	return db.QuickCheck(ctx, bdb)
}

func dialectOf(d db.Driver) schema.Dialect {
	if d == db.DriverPostgres {
		return schema.Postgres
	}
	return schema.SQLite
}
