package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 待恢复标记的来源与阶段（master-backup「发起恢复」「启动时执行恢复」「恢复之后的收尾」）。
const (
	SourceManual = "manual"
	SourceSetup  = "setup"
	SourceAuto   = "auto"

	PhasePending  = "pending"
	PhaseRestored = "restored"
	PhaseFailed   = "failed"
)

// Marker 是数据目录里的 restore-pending.json：它在库之外，因为库本身要被换掉。
type Marker struct {
	Backup      string    `json:"backup"`
	Source      string    `json:"source"`
	Actor       string    `json:"actor"`
	RequestedAt time.Time `json:"requested_at"`
	Phase       string    `json:"phase"`
	Error       string    `json:"error,omitempty"`
	// ReplacedDir 是这次恢复把换下来的东西放进去的目录（backups/ 下，名字带随机后缀）：动任何东西之前先写进标记，
	// 中途崩溃重启后按它接着做或撤回，不靠内存里的记录。自动恢复用 corrupt- 开头，别的用 replaced- 开头。
	ReplacedDir string `json:"replaced_dir,omitempty"`
}

func markerPath(dataDir string) string { return filepath.Join(dataDir, db.RestorePendingFile) }

// ReadMarker 读待恢复标记；没有时返回 nil。
func ReadMarker(dataDir string) (*Marker, error) {
	raw, err := os.ReadFile(markerPath(dataDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "读待恢复标记失败", err)
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "待恢复标记 "+db.RestorePendingFile+" 不是合法的 JSON", err).
			WithNext("检查或删掉数据目录里的 " + db.RestorePendingFile)
	}
	return &m, nil
}

// WriteMarker 写待恢复标记（0600），先写临时文件再改名。
func WriteMarker(dataDir string, m *Marker) error {
	raw, _ := json.MarshalIndent(m, "", "  ")
	tmp, err := os.CreateTemp(dataDir, "."+db.RestorePendingFile+".*")
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "写待恢复标记失败", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), markerPath(dataDir))
	}
	if err == nil {
		err = SyncDir(dataDir) // 标记是崩溃后唯一的记录：改名要真的落盘
	}
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "写待恢复标记失败", err)
	}
	return nil
}

// SyncDir 把一个目录的目录项刷到磁盘（改名、删除之后调，断电也不会丢或乱序）。
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// RemoveMarker 删掉待恢复标记。
func RemoveMarker(dataDir string) error {
	if err := os.Remove(markerPath(dataDir)); err != nil && !os.IsNotExist(err) {
		return v1.Wrap(v1.CodeInternal, "删除待恢复标记失败", err)
	}
	if err := SyncDir(dataDir); err != nil {
		return v1.Wrap(v1.CodeInternal, "删除待恢复标记失败", err)
	}
	return nil
}

// Target 是要恢复到的主控：数据目录与数据库配置；PostgreSQL 另要连接参数（给 psql 与 pg_dump）。
// DB 可选：给了就用它（不关），没给按 Config 自己开、用完关——serve 启动时没给；测试给同一个 schema 上的连接。
type Target struct {
	DataDir string
	Config  db.Config
	PG      *PGConn
	Version string
	DB      *bun.DB
}

// Startup 是 serve 在打开库做业务之前调的（master-serve「启动平台与启动顺序」）：
//  1. 有 phase 为 pending 的标记就执行恢复；
//  2. SQLite 驱动下做启动检查，库确定损坏时从本机最新可用的备份自动恢复；
//
// 返回此刻的标记（nil 表示没有要收尾的），serve 迁移之后按它做收尾。自动恢复无从下手（没有可用的备份，或恢复本身失败）时返回错误，拒绝启动。
func Startup(ctx context.Context, t Target, logger *slog.Logger) (*Marker, error) {
	m, err := ApplyPending(ctx, t, logger)
	if err != nil {
		return nil, err
	}
	if t.Config.Driver != db.DriverSQLite {
		return m, nil
	}
	if m != nil {
		// 有标记（换好了或失败了）时库文件一定要在：不在就说明换到一半、撤回也没成，绝不能让驱动在这里建一个空库。
		if !fileExists(t.Config.Path) {
			return nil, v1.Newf(v1.CodeDatabase, "从备份恢复没有完成，库文件 %s 不在", t.Config.Path).
				WithState("restore", m).
				WithNext("原来的库文件在数据目录的 backups/" + m.ReplacedDir + "/ 里：把它移回原处再启动；不要删 " + db.RestorePendingFile)
		}
		return m, nil
	}
	return checkSQLite(ctx, t, logger)
}

// ApplyPending 执行 phase 为 pending 的待恢复标记（master-backup「启动时执行恢复」）。换库成功把 phase 改成 restored；
// 失败把已经移走的移回原处，phase 改成 failed 并记下原因，照常用原来的库启动（返回的标记交给收尾写 last_restore）。
func ApplyPending(ctx context.Context, t Target, logger *slog.Logger) (*Marker, error) {
	m, err := ReadMarker(t.DataDir)
	if err != nil || m == nil || m.Phase != PhasePending {
		return m, err
	}
	logger.Warn("开始从备份恢复", "backup", m.Backup, "source", m.Source)
	if err := apply(ctx, t, m, logger); err != nil {
		m.Phase, m.Error = PhaseFailed, v1.AsError(err).Reason
		logger.Error("从备份恢复失败，照常用原来的库启动", "backup", m.Backup, "error", err)
	} else {
		m.Phase = PhaseRestored
		logger.Warn("已从备份恢复", "backup", m.Backup)
	}
	return m, WriteMarker(t.DataDir, m)
}

// mover 记下换库换文件时停放过的路径，失败时倒着撤回：删掉原位置上新放进去的，再把停放的移回来。
type mover struct{ parked []move }

type move struct{ from, to string }

// park 把 from 停放到 to，接着做的时候原位置可以放新的。可以接着上一次中途崩溃的恢复做：to 已经在（上一次停放的原件），
// 说明 from 上是上一次放进去、没做完的新东西，删掉它，不能把它移过去盖掉原件。撤回时一律删掉 from、把 to 移回来。
func (mv *mover) park(from, to string) error {
	mv.parked = append(mv.parked, move{from, to})
	if _, err := os.Lstat(to); err == nil {
		return os.RemoveAll(from)
	}
	if _, err := os.Lstat(from); os.IsNotExist(err) {
		return nil
	}
	return os.Rename(from, to)
}

// undo 倒着撤回；有一步撤不回就把错误带回去（这时原件还在停放目录里，由人处理）。
func (mv *mover) undo() error {
	var first error
	for i := len(mv.parked) - 1; i >= 0; i-- {
		m := mv.parked[i]
		if err := os.RemoveAll(m.from); err != nil && first == nil {
			first = err
		}
		if _, err := os.Lstat(m.to); err == nil {
			if err := os.Rename(m.to, m.from); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func stamp(at time.Time) string { return at.UTC().Format("20060102T150405Z") }

// uniqueDir 取一个停放目录名：时间加随机后缀，同一秒里重试也不会撞进上一次的目录。
func uniqueDir(prefix string, at time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + stamp(at) + "-" + hex.EncodeToString(b)
}

func apply(ctx context.Context, t Target, m *Marker, logger *slog.Logger) (err error) {
	path := filepath.Join(Dir(t.DataDir), m.Backup)
	man, err := Validate(path, t.Config.Driver)
	if err != nil {
		return err
	}
	// 停放目录在动任何东西之前定下并写进标记：崩溃重启后按它接着做或撤回。
	if m.ReplacedDir == "" {
		prefix := "replaced-"
		if m.Source == SourceAuto {
			prefix = "corrupt-"
		}
		m.ReplacedDir = uniqueDir(prefix, time.Now())
		if err := WriteMarker(t.DataDir, m); err != nil {
			return err
		}
	}
	replaced := filepath.Join(Dir(t.DataDir), m.ReplacedDir)
	if err := os.MkdirAll(replaced, 0o700); err != nil {
		return v1.Wrap(v1.CodeInternal, "建 "+m.ReplacedDir+" 目录失败", err)
	}
	var schema string
	var major int
	if err := func() error {
		bdb := t.DB
		if bdb == nil && (t.Config.Driver == db.DriverPostgres || fileExists(t.Config.Path)) {
			opened, err := db.Open(ctx, t.Config)
			if err != nil {
				if t.Config.Driver == db.DriverPostgres {
					return err
				}
				opened = nil // SQLite 打不开就不做恢复前的备份，照样换库
			}
			if opened != nil {
				// 换文件之前关掉：SQLite 在连接还开着时改名库文件是未定义行为。
				defer opened.Close()
				bdb = opened
			}
		}
		// 1. 恢复前先存一份：这次恢复的后悔药（自动恢复时库已经坏了，不存）。
		if m.Source != SourceAuto && bdb != nil {
			info, err := Create(ctx, Source{DataDir: t.DataDir, DB: bdb, Driver: t.Config.Driver, PG: t.PG, Version: t.Version}, PrefixBeforeRestore)
			if err != nil {
				return v1.Wrap(v1.CodeInternal, "恢复前生成 before-restore 备份失败，没有动任何东西", err)
			}
			logger.Warn("恢复前已生成一份当前的备份", "backup", info.Name)
		}
		if t.Config.Driver == db.DriverPostgres {
			var err error
			if schema, err = currentSchema(ctx, bdb); err != nil {
				return err
			}
			major, err = serverMajor(ctx, bdb)
			return err
		}
		return nil
	}(); err != nil {
		return err
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return v1.Wrap(v1.CodeBadRequest, "打不开备份", err)
	}
	defer zr.Close()
	mv := &mover{}
	defer func() {
		if err != nil {
			if uerr := mv.undo(); uerr != nil {
				err = v1.Wrap(v1.CodeInternal, fmt.Sprintf("恢复失败（%v），撤回也没有做完：原来的东西在 backups/%s/ 里", v1.AsError(err).Reason, m.ReplacedDir), uerr)
			}
		}
	}()
	// 2. 先换文件（撤得回），再换库（PostgreSQL 换了就撤不回）。database.json 与 config.yaml 不动。
	if err := restoreFiles(&zr.Reader, t.DataDir, replaced, mv); err != nil {
		return err
	}
	if t.Config.Driver == db.DriverPostgres {
		if man.Schema != schema {
			return v1.Newf(v1.CodeConflict, "备份的 schema 是 %q，当前连接的是 %q：只能恢复回同名的 schema", man.Schema, schema)
		}
		return restorePostgres(ctx, &zr.Reader, t, schema, major)
	}
	if err := restoreSQLite(ctx, &zr.Reader, t, replaced, mv); err != nil {
		return err
	}
	// 换好的东西落盘之后才把标记改成 restored（调用方写标记）。
	SyncDir(replaced)
	return SyncDir(filepath.Dir(t.Config.Path))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// restoreFiles 把 master.key、subscribes/、rule_templates/ 移进 replaced/，再放入备份里的（目录 0700、文件 0600）。
func restoreFiles(zr *zip.Reader, dataDir, replaced string, mv *mover) error {
	for _, name := range append(append([]string{}, restoredFiles...), restoredDirs...) {
		if err := mv.park(filepath.Join(dataDir, name), filepath.Join(replaced, name)); err != nil {
			return v1.Wrap(v1.CodeInternal, "移走当前的 "+name+" 失败", err)
		}
	}
	for _, d := range restoredDirs {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o700); err != nil {
			return v1.Wrap(v1.CodeInternal, "建 "+d+" 失败", err)
		}
	}
	for _, f := range zr.File {
		if !restorable(f.Name) {
			continue
		}
		dst := filepath.Join(dataDir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return v1.Wrap(v1.CodeInternal, "恢复 "+f.Name+" 失败", err)
		}
		tmp, err := extractTo(f, filepath.Dir(dst), "."+filepath.Base(dst)+".*")
		if err == nil {
			err = os.Rename(tmp, dst)
		}
		if err != nil {
			return v1.Wrap(v1.CodeInternal, "恢复 "+f.Name+" 失败", err)
		}
	}
	return nil
}

func restorable(name string) bool {
	for _, f := range restoredFiles {
		if name == f {
			return true
		}
	}
	for _, d := range restoredDirs {
		if strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}

// restoreSQLite 把备份里的库解到库文件旁边的临时文件、通过 quick_check，再把当前的库文件（含 -wal、-shm）移进 replaced/，
// 最后改名成库文件。
func restoreSQLite(ctx context.Context, zr *zip.Reader, t Target, replaced string, mv *mover) error {
	f := findEntry(zr, EntrySQLite)
	tmp, err := extractTo(f, filepath.Dir(t.Config.Path), ".restore-*.db")
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "解出备份里的库失败", err)
	}
	defer removeSQLiteFiles(tmp)
	if err := quickCheckFile(ctx, tmp); err != nil {
		return v1.Wrap(v1.CodeBadRequest, "备份里的库没通过 quick_check", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := mv.park(t.Config.Path+suffix, filepath.Join(replaced, filepath.Base(t.Config.Path)+suffix)); err != nil {
			return v1.Wrap(v1.CodeInternal, "移走当前的库文件失败", err)
		}
	}
	if err := os.Rename(tmp, t.Config.Path); err != nil {
		return v1.Wrap(v1.CodeInternal, "放入备份里的库失败", err)
	}
	return nil
}

// restorePostgres 用 psql 在一个事务里先删掉整个 schema、再执行备份里的 SQL；任何一句出错整个事务回滚。
func restorePostgres(ctx context.Context, zr *zip.Reader, t Target, cur string, major int) error {
	psql, err := findPsql(ctx, major)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(Dir(t.DataDir), ".restore-sql-*")
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "建临时目录失败", err)
	}
	defer os.RemoveAll(dir)
	dump, err := extractTo(findEntry(zr, EntryPostgres), dir, "dump-*.sql")
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "解出备份里的 SQL 失败", err)
	}
	pre := filepath.Join(dir, "pre.sql")
	q := quoteIdent(cur)
	if err := os.WriteFile(pre, []byte(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE;\nCREATE SCHEMA %s;\n", q, q)), 0o600); err != nil {
		return v1.Wrap(v1.CodeInternal, "写临时文件失败", err)
	}
	// 连接此刻都空闲、不在事务里（测试给的连接也是），不持有表锁，psql 的 DROP SCHEMA 不会被它们挡住。
	cmd := exec.CommandContext(ctx, psql, "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-f", pre, "-f", dump)
	cmd.Env = t.PG.env()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &out
	if err := cmd.Run(); err != nil {
		return v1.Newf(v1.CodeDatabase, "psql 执行备份里的 SQL 失败，事务已回滚：%s", lastLine(out.String()))
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// quickCheckTimeout 是启动检查的时限。
const quickCheckTimeout = 5 * time.Minute

// corruptMarkers 是确定损坏的错误字样（照 mmwx 的 isDefiniteDatabaseCorruption）。
var corruptMarkers = []string{"quick_check 没通过", "file is not a database", "database disk image is malformed", "sqlite_corrupt", "sqlite_notadb"}

func definitelyCorrupt(err error) bool {
	// 四字段错误的 Error() 只有 reason，驱动的原话在被包着的错误里：整条链都看。
	var b strings.Builder
	for e := err; e != nil; e = errors.Unwrap(e) {
		b.WriteString(e.Error())
		b.WriteString("\n")
	}
	s := strings.ToLower(b.String())
	for _, m := range corruptMarkers {
		if strings.Contains(s, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// checkSQLite 是 SQLite 的启动检查（master-backup「SQLite 启动检查与自动恢复」）：库文件存在时跑 quick_check；
// 确定损坏时找本机最新可用的备份，把坏的库文件移进 backups/corrupt-<时间>/，写 source 为 auto 的标记并恢复。
// 别的错误（权限不足之类）原样返回，不自动恢复。
func checkSQLite(ctx context.Context, t Target, logger *slog.Logger) (*Marker, error) {
	if !fileExists(t.Config.Path) {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, quickCheckTimeout)
	defer cancel()
	err := func() error {
		bdb := t.DB
		if bdb == nil {
			var err error
			if bdb, err = db.Open(cctx, t.Config); err != nil {
				return err
			}
			defer bdb.Close()
		}
		return db.QuickCheck(cctx, bdb)
	}()
	if err == nil {
		return nil, nil
	}
	if !definitelyCorrupt(err) {
		return nil, err
	}
	logger.Error("SQLite 库已损坏，尝试从本机备份自动恢复", "error", err)
	name, lerr := LatestUsable(ctx, t.DataDir)
	if lerr != nil {
		return nil, lerr
	}
	if name == "" {
		return nil, v1.Newf(v1.CodeDatabase, "SQLite 库已损坏（%v），backups/ 里没有可用的本机备份", err).
			WithNext("把一份同驱动的备份放进数据目录的 backups/ 再启动（会自动用它恢复），或把损坏的库文件移走、从空库开始")
	}
	// 先写标记（坏库要停放到哪也在里面），再动库文件：之后任何时候崩溃，重启都按标记接着恢复，不会在原路径上建出空库。
	m := &Marker{Backup: name, Source: SourceAuto, Actor: "system", RequestedAt: time.Now().UTC(), Phase: PhasePending,
		ReplacedDir: uniqueDir("corrupt-", time.Now())}
	if err := WriteMarker(t.DataDir, m); err != nil {
		return nil, err
	}
	// 自动恢复失败时标记留作 failed（坏库已移回原处）：照常往下走，库打不开就拒绝启动；下次启动看到 failed 不再重试同一份备份。
	return ApplyPending(ctx, t, logger)
}
