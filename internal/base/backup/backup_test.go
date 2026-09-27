package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// sqliteMaster 建一个数据目录，SQLite 库就在数据目录里（像 serve 那样），迁移好。
func sqliteMaster(t *testing.T) (string, db.Config, *bun.DB) {
	t.Helper()
	dataDir := t.TempDir()
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	cfg := db.Config{Driver: db.DriverSQLite, Path: filepath.Join(dataDir, db.SQLiteFile)}
	bdb, err := db.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	return dataDir, cfg, bdb
}

func seedUser(t *testing.T, bdb *bun.DB, name string) {
	t.Helper()
	now := time.Now().UTC()
	u := &model.User{Username: name, Role: "user", IsActive: true, PasswordHash: "x", RecoveryCodes: json.RawMessage(`[]`),
		NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now, ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func seedSession(t *testing.T, bdb *bun.DB, user, hash string) {
	t.Helper()
	s := &model.Session{TokenHash: hash, Username: user, ExpiresAt: time.Now().Add(time.Hour).UTC(), CreatedAt: time.Now().UTC()}
	if _, err := bdb.NewInsert().Model(s).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, bdb *bun.DB, m any) int {
	t.Helper()
	n, err := bdb.NewSelect().Model(m).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func zipNames(t *testing.T, path string) []string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

func sqliteSource(dataDir string, bdb *bun.DB) Source {
	return Source{DataDir: dataDir, DB: bdb, Driver: db.DriverSQLite, Version: "test"}
}

// master-backup「SQLite 备份的内容」。
func TestCreateSQLite(t *testing.T) {
	dataDir, _, bdb := sqliteMaster(t)
	seedUser(t, bdb, "alice")
	seedSession(t, bdb, "alice", "h1")
	seedSession(t, bdb, "alice", "h2")
	write(t, filepath.Join(dataDir, db.MasterKeyFile), "key")
	write(t, filepath.Join(dataDir, db.SubscribesDir, "a.yaml"), "a")
	write(t, filepath.Join(dataDir, db.SubscribesDir, ".hidden"), "h")
	write(t, filepath.Join(dataDir, db.RuleTemplatesDir, "sub", "r.txt"), "r")
	write(t, filepath.Join(dataDir, db.LogsDir, db.LogFile), "log")
	write(t, filepath.Join(dataDir, db.PublicDir, "p.txt"), "p")
	info, err := Create(context.Background(), sqliteSource(dataDir, bdb), PrefixBackup)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(dataDir), info.Name)
	if !strings.HasPrefix(info.Name, PrefixBackup) || info.Driver != db.DriverSQLite {
		t.Fatalf("Info：%+v", info)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("备份应当 0600，得到 %o", fi.Mode().Perm())
	}
	want := []string{"database/satchel.db", "manifest.json", db.MasterKeyFile, db.RuleTemplatesDir + "/sub/r.txt", db.SubscribesDir + "/a.yaml"}
	if got := zipNames(t, path); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("条目应当是 %v，得到 %v", want, got)
	}
	tmp, err := extractSQLite(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := quickCheckFile(context.Background(), tmp); err != nil {
		t.Fatal(err)
	}
	copyDB, _ := db.OpenDSN(context.Background(), db.DriverSQLite, db.SQLiteDSN(tmp))
	defer copyDB.Close()
	if count(t, copyDB, (*model.Session)(nil)) != 0 || count(t, copyDB, (*model.User)(nil)) != 1 {
		t.Fatal("备份里的库应当没有会话、有那个用户")
	}
	if count(t, bdb, (*model.Session)(nil)) != 2 {
		t.Fatal("原库的会话不应当被动")
	}
	entries, _ := os.ReadDir(Dir(dataDir))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("不应当留下临时文件：%s", e.Name())
		}
	}
}

// buildZip 手工拼一份备份，给校验的反例用。
func buildZip(t *testing.T, dir, name string, m *Manifest, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if m != nil {
		raw, _ := json.Marshal(m)
		w, _ := zw.Create(EntryManifest)
		w.Write(raw)
	}
	for n, c := range entries {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	path := filepath.Join(dir, name)
	write(t, path, buf.String())
	return path
}

func migrations(t *testing.T) []string {
	names, err := db.MigrationNames(dialectOf(db.DriverSQLite))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// master-backup「备份的校验」：每条一个反例。
func TestValidate(t *testing.T) {
	dir := t.TempDir()
	good := func() *Manifest {
		return &Manifest{Format: Format, Driver: db.DriverSQLite, Migrations: migrations(t), CreatedAt: time.Now()}
	}
	ok := buildZip(t, dir, "ok.zip", good(), map[string]string{EntrySQLite: "x", "subscribes/a": "a"})
	if _, err := Validate(ok, db.DriverSQLite); err != nil {
		t.Fatalf("合法的备份：%v", err)
	}
	write(t, filepath.Join(dir, "notzip.zip"), "hello")
	future := good()
	future.Migrations = append(future.Migrations, "0002_future")
	badFormat := good()
	badFormat.Format = "x"
	cases := []struct {
		path string
		code v1.Code
		want string
	}{
		{filepath.Join(dir, "notzip.zip"), v1.CodeBadRequest, "ZIP"},
		{buildZip(t, dir, "nomanifest.zip", nil, map[string]string{EntrySQLite: "x"}), v1.CodeBadRequest, "manifest"},
		{buildZip(t, dir, "format.zip", badFormat, map[string]string{EntrySQLite: "x"}), v1.CodeBadRequest, "格式"},
		{buildZip(t, dir, "nodb.zip", good(), map[string]string{EntryPostgres: "x"}), v1.CodeBadRequest, EntrySQLite},
		{buildZip(t, dir, "future.zip", future, map[string]string{EntrySQLite: "x"}), v1.CodeBadRequest, "0002_future"},
		{buildZip(t, dir, "escape.zip", good(), map[string]string{EntrySQLite: "x", "../escape": "e"}), v1.CodeBadRequest, "../escape"},
		{buildZip(t, dir, "other.zip", good(), map[string]string{EntrySQLite: "x", "logs/satchel.log": "l"}), v1.CodeBadRequest, "logs/"},
	}
	for _, c := range cases {
		_, err := Validate(c.path, db.DriverSQLite)
		if e := v1.AsError(err); e.Code != c.code || !strings.Contains(e.Reason, c.want) {
			t.Errorf("%s 应当是 %s 且含 %q，得到 %v", filepath.Base(c.path), c.code, c.want, err)
		}
	}
	if _, err := Validate(ok, db.DriverPostgres); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("跨驱动应当 conflict，得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Fatal("校验不应当解出任何文件")
	}
}

// master-backup「只留 7 份」：按修改时间留最新的，受保护的那份不删；List 从新到旧、不列临时文件。
func TestListAndPrune(t *testing.T) {
	dataDir := t.TempDir()
	dir := Dir(dataDir)
	os.MkdirAll(dir, 0o700)
	now := time.Now()
	for i := 0; i < 9; i++ {
		p := buildZip(t, dir, "satchel-backup-"+strconv.Itoa(i)+".zip", &Manifest{Format: Format, Driver: db.DriverSQLite}, nil)
		os.Chtimes(p, now.Add(time.Duration(i)*time.Minute), now.Add(time.Duration(i)*time.Minute))
	}
	write(t, filepath.Join(dir, ".satchel-backup-x.zip.123"), "tmp")
	list, _ := List(dataDir)
	if len(list) != 9 || list[0].Name != "satchel-backup-8.zip" || list[0].Driver != db.DriverSQLite || list[0].CreatedAt == nil {
		t.Fatalf("List：%+v", list)
	}
	if err := Prune(dataDir, Keep, "satchel-backup-0.zip"); err != nil {
		t.Fatal(err)
	}
	list, _ = List(dataDir)
	var names []string
	for _, b := range list {
		names = append(names, b.Name)
	}
	if len(list) != 8 || names[len(names)-1] != "satchel-backup-0.zip" || strings.Contains(strings.Join(names, ","), "satchel-backup-1.zip") {
		t.Fatalf("应当留最新 7 份加受保护的 0：%v", names)
	}
	if _, err := Path(dataDir, "../x.zip"); v1.AsError(err).Code != v1.CodeNotFound {
		t.Fatalf("不在清单里的名字应当 not_found，得到 %v", err)
	}
}

// 找可用备份：跳过恢复前自动生成的与库坏了的。
func TestLatestUsable(t *testing.T) {
	dataDir, _, bdb := sqliteMaster(t)
	good, err := Create(context.Background(), sqliteSource(dataDir, bdb), PrefixBackup)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Format: Format, Driver: db.DriverSQLite, Migrations: migrations(t)}
	later := time.Now().Add(time.Hour)
	for _, name := range []string{"satchel-backup-broken.zip", PrefixBeforeRestore + "x.zip"} {
		p := buildZip(t, Dir(dataDir), name, m, map[string]string{EntrySQLite: "not a database"})
		os.Chtimes(p, later, later)
	}
	if name, err := LatestUsable(context.Background(), dataDir); err != nil || name != good.Name {
		t.Fatalf("应当找到 %s，得到 %q %v", good.Name, name, err)
	}
}

// master-backup「SQLite 恢复到过去」：库、master.key、subscribes/ 回到备份那一刻；database.json 不动；留下 before-restore 与 replaced。
func TestRestoreSQLite(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	seedUser(t, bdb, "alice")
	write(t, filepath.Join(dataDir, db.MasterKeyFile), "old-key")
	write(t, filepath.Join(dataDir, db.SubscribesDir, "a.yaml"), "a")
	b, err := Create(context.Background(), sqliteSource(dataDir, bdb), PrefixBackup)
	if err != nil {
		t.Fatal(err)
	}
	seedUser(t, bdb, "late")
	write(t, filepath.Join(dataDir, db.MasterKeyFile), "new-key")
	write(t, filepath.Join(dataDir, db.SubscribesDir, "late.yaml"), "late")
	write(t, filepath.Join(dataDir, db.ConfigFile), `{"driver":"sqlite"}`)
	bdb.Close()
	if err := WriteMarker(dataDir, &Marker{Backup: b.Name, Source: SourceManual, Actor: "root", Phase: PhasePending}); err != nil {
		t.Fatal(err)
	}
	m, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg, Version: "test"}, quiet)
	if err != nil || m == nil || m.Phase != PhaseRestored {
		t.Fatalf("恢复：%+v %v", m, err)
	}
	after, _ := db.Open(context.Background(), cfg)
	defer after.Close()
	var names []string
	after.NewSelect().Model((*model.User)(nil)).Column("username").Scan(context.Background(), &names)
	if strings.Join(names, ",") != "alice" {
		t.Fatalf("恢复后应当只有 alice，得到 %v", names)
	}
	if k, _ := os.ReadFile(filepath.Join(dataDir, db.MasterKeyFile)); string(k) != "old-key" {
		t.Fatalf("master.key 应当回到备份里的：%q", k)
	}
	if _, err := os.Stat(filepath.Join(dataDir, db.SubscribesDir, "late.yaml")); err == nil {
		t.Fatal("subscribes/ 应当换成备份里的")
	}
	if c, _ := os.ReadFile(filepath.Join(dataDir, db.ConfigFile)); string(c) != `{"driver":"sqlite"}` {
		t.Fatal("database.json 不应当被动")
	}
	list, _ := List(dataDir)
	before := false
	for _, x := range list {
		before = before || strings.HasPrefix(x.Name, PrefixBeforeRestore)
	}
	replaced, _ := filepath.Glob(filepath.Join(Dir(dataDir), "replaced-*", db.MasterKeyFile))
	if !before || len(replaced) != 1 {
		t.Fatalf("应当有 before-restore 备份与 replaced/ 里的旧 master.key：%v %v", list, replaced)
	}
}

// 换库失败时撤回：备份里的库坏了 → 文件与库都回到原样，标记是 failed。
func TestRestoreSQLiteRollback(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	seedUser(t, bdb, "alice")
	write(t, filepath.Join(dataDir, db.MasterKeyFile), "cur-key")
	write(t, filepath.Join(dataDir, db.SubscribesDir, "cur.yaml"), "c")
	bdb.Close()
	m := &Manifest{Format: Format, Driver: db.DriverSQLite, Migrations: migrations(t)}
	bad := buildZip(t, Dir(dataDir), "satchel-backup-bad.zip", m, map[string]string{EntrySQLite: "garbage", db.MasterKeyFile: "bak-key"})
	WriteMarker(dataDir, &Marker{Backup: filepath.Base(bad), Source: SourceManual, Phase: PhasePending})
	got, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg, Version: "test"}, quiet)
	if err != nil || got.Phase != PhaseFailed || got.Error == "" {
		t.Fatalf("应当是 failed 并带原因：%+v %v", got, err)
	}
	if k, _ := os.ReadFile(filepath.Join(dataDir, db.MasterKeyFile)); string(k) != "cur-key" {
		t.Fatalf("master.key 应当撤回：%q", k)
	}
	if _, err := os.Stat(filepath.Join(dataDir, db.SubscribesDir, "cur.yaml")); err != nil {
		t.Fatal("subscribes/ 应当撤回")
	}
	after, _ := db.Open(context.Background(), cfg)
	defer after.Close()
	if count(t, after, (*model.User)(nil)) != 1 {
		t.Fatal("库应当原样")
	}
}

func corruptFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt(bytes.Repeat([]byte{0xAB}, 100), 0)
	f.Close()
}

// master-backup「坏库自动恢复」与「没有备份时拒绝启动」。
func TestAutoRestore(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	seedUser(t, bdb, "alice")
	if _, err := Create(context.Background(), sqliteSource(dataDir, bdb), PrefixBackup); err != nil {
		t.Fatal(err)
	}
	bdb.Close()
	corruptFile(t, cfg.Path)
	m, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg, Version: "test"}, quiet)
	if err != nil || m == nil || m.Source != SourceAuto || m.Phase != PhaseRestored {
		t.Fatalf("应当自动恢复：%+v %v", m, err)
	}
	if bad, _ := filepath.Glob(filepath.Join(Dir(dataDir), "corrupt-*", db.SQLiteFile)); len(bad) != 1 {
		t.Fatal("坏库应当移进 corrupt-*/")
	}
	after, _ := db.Open(context.Background(), cfg)
	defer after.Close()
	if count(t, after, (*model.User)(nil)) != 1 {
		t.Fatal("数据应当回到备份那一刻")
	}
	if l, _ := List(dataDir); len(l) != 1 {
		t.Fatalf("自动恢复不生成 before-restore：%v", l)
	}
}

func TestAutoRestoreWithoutBackup(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	bdb.Close()
	corruptFile(t, cfg.Path)
	_, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg, Version: "test"}, quiet)
	if e := v1.AsError(err); e.Code != v1.CodeDatabase || !strings.Contains(e.Next, "backups/") {
		t.Fatalf("应当拒绝启动并说明怎么办：%v", err)
	}
	if _, err := os.Stat(cfg.Path); err != nil {
		t.Fatal("库文件不应当被移动")
	}
}

// pgTarget 给 PostgreSQL 的备份与恢复测试：需要 pg_dump 与 psql。本机没有时跳过；CI 设了 SATCHEL_TEST_REQUIRE_PG_TOOLS=1，缺了就失败。
func pgTarget(t *testing.T) (*bun.DB, *PGConn) {
	t.Helper()
	for _, tool := range []string{"pg_dump", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("SATCHEL_TEST_REQUIRE_PG_TOOLS") == "1" {
				t.Fatalf("SATCHEL_TEST_REQUIRE_PG_TOOLS=1 但找不到 %s", tool)
			}
			t.Skipf("找不到 %s，跳过 PostgreSQL 的备份与恢复测试（CI 里跑）", tool)
		}
	}
	bdb := dbtest.OpenPostgres(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv(dbtest.EnvDSN))
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	port, _ := strconv.Atoi(u.Port())
	return bdb, &PGConn{Host: u.Hostname(), Port: port, User: u.User.Username(), Password: pw, Database: strings.TrimPrefix(u.Path, "/"), SSLMode: u.Query().Get("sslmode")}
}

// master-backup「PostgreSQL 备份不带会话数据」与「PostgreSQL 恢复失败时库不变」，以及正常的恢复。
func TestPostgresBackupRestore(t *testing.T) {
	bdb, conn := pgTarget(t)
	ctx := context.Background()
	dataDir := t.TempDir()
	db.EnsureDataDir(dataDir)
	seedUser(t, bdb, "alice")
	seedSession(t, bdb, "alice", "secret-session-hash")
	src := Source{DataDir: dataDir, DB: bdb, Driver: db.DriverPostgres, PG: conn, Version: "test"}
	b, err := Create(ctx, src, PrefixBackup)
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.OpenReader(filepath.Join(Dir(dataDir), b.Name))
	rc, _ := findEntry(&zr.Reader, EntryPostgres).Open()
	sqlText, _ := io.ReadAll(rc)
	rc.Close()
	zr.Close()
	if !strings.Contains(string(sqlText), "sessions") || strings.Contains(string(sqlText), "secret-session-hash") {
		t.Fatal("SQL 里应当有 sessions 的表结构、没有它的数据")
	}
	seedUser(t, bdb, "late")
	target := Target{DataDir: dataDir, Config: db.Config{Driver: db.DriverPostgres}, PG: conn, Version: "test", DB: bdb}
	WriteMarker(dataDir, &Marker{Backup: b.Name, Source: SourceManual, Phase: PhasePending})
	m, err := ApplyPending(ctx, target, quiet)
	if err != nil || m.Phase != PhaseRestored {
		t.Fatalf("恢复：%+v %v", m, err)
	}
	if n := count(t, bdb, (*model.User)(nil)); n != 1 || count(t, bdb, (*model.Session)(nil)) != 0 {
		t.Fatalf("恢复后应当只有 alice、没有会话：%d", n)
	}

	// SQL 中途出错：事务回滚，数据不变。
	seedUser(t, bdb, "late2")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	man, _ := json.Marshal(Manifest{Format: Format, Driver: db.DriverPostgres, Migrations: migrationsFor(t, db.DriverPostgres), Schema: mustSchema(t, bdb)})
	w, _ := zw.Create(EntryManifest)
	w.Write(man)
	w, _ = zw.Create(EntryPostgres)
	w.Write(append(sqlText, []byte("\nSELECT 1/0;\n")...))
	zw.Close()
	write(t, filepath.Join(Dir(dataDir), "satchel-backup-bad.zip"), buf.String())
	WriteMarker(dataDir, &Marker{Backup: "satchel-backup-bad.zip", Source: SourceManual, Phase: PhasePending})
	m, err = ApplyPending(ctx, target, quiet)
	if err != nil || m.Phase != PhaseFailed {
		t.Fatalf("应当失败：%+v %v", m, err)
	}
	if n := count(t, bdb, (*model.User)(nil)); n != 2 {
		t.Fatalf("事务应当回滚、数据不变（alice 与 late2）：%d", n)
	}
}

func migrationsFor(t *testing.T, d db.Driver) []string {
	names, err := db.MigrationNames(dialectOf(d))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func mustSchema(t *testing.T, bdb *bun.DB) string {
	s, err := currentSchema(context.Background(), bdb)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// master-backup「没有 pg_dump」：找不到时是 unavailable 并点名，backups/ 里没有多出文件。
func TestPGDumpMissing(t *testing.T) {
	bdb := dbtest.OpenPostgres(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	dataDir := t.TempDir()
	_, err := Create(context.Background(), Source{DataDir: dataDir, DB: bdb, Driver: db.DriverPostgres, PG: &PGConn{}}, PrefixBackup)
	if e := v1.AsError(err); e.Code != v1.CodeUnavailable || !strings.Contains(e.Reason, "pg_dump") || !strings.Contains(e.Next, "postgresql-client") {
		t.Fatalf("应当是点名 pg_dump 的 unavailable：%v", err)
	}
	if l, _ := List(dataDir); len(l) != 0 {
		t.Fatal("不应当多出备份")
	}
}

// 审查第 1 条：自动恢复在「标记已写、坏库已移走」之后崩溃，重启按标记接着恢复，不会在原路径建出空库。
func TestAutoRestoreResumesAfterCrash(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	seedUser(t, bdb, "alice")
	b, err := Create(context.Background(), sqliteSource(dataDir, bdb), PrefixBackup)
	if err != nil {
		t.Fatal(err)
	}
	bdb.Close()
	corruptFile(t, cfg.Path)
	parked := filepath.Join(Dir(dataDir), "corrupt-x")
	os.MkdirAll(parked, 0o700)
	if err := WriteMarker(dataDir, &Marker{Backup: b.Name, Source: SourceAuto, Phase: PhasePending, ReplacedDir: "corrupt-x"}); err != nil {
		t.Fatal(err)
	}
	os.Rename(cfg.Path, filepath.Join(parked, db.SQLiteFile)) // 崩溃发生在这之后
	m, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg, Version: "test"}, quiet)
	if err != nil || m.Phase != PhaseRestored {
		t.Fatalf("应当接着恢复：%+v %v", m, err)
	}
	after, _ := db.Open(context.Background(), cfg)
	defer after.Close()
	if count(t, after, (*model.User)(nil)) != 1 {
		t.Fatal("应当是备份里的数据，不是空库")
	}
	if _, err := os.Stat(filepath.Join(parked, db.SQLiteFile)); err != nil {
		t.Fatal("坏库应当还留在停放目录里")
	}
}

// 审查第 2 条：手动恢复换到一半崩溃（库与 subscribes/ 已停放、subscribes/ 里有一半新东西），重启接着做完；停放目录里的原件不被盖掉。
func TestRestoreResumesMidSwap(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	seedUser(t, bdb, "alice")
	write(t, filepath.Join(dataDir, db.SubscribesDir, "a.yaml"), "backup-a")
	b, err := Create(context.Background(), sqliteSource(dataDir, bdb), PrefixBackup)
	if err != nil {
		t.Fatal(err)
	}
	seedUser(t, bdb, "late")
	write(t, filepath.Join(dataDir, db.SubscribesDir, "a.yaml"), "current-a")
	bdb.Close()
	parked := filepath.Join(Dir(dataDir), "replaced-x")
	os.MkdirAll(parked, 0o700)
	WriteMarker(dataDir, &Marker{Backup: b.Name, Source: SourceManual, Phase: PhasePending, ReplacedDir: "replaced-x"})
	// 模拟上一次做到一半：原件已停放，原位置上有一半新东西。
	os.Rename(filepath.Join(dataDir, db.SubscribesDir), filepath.Join(parked, db.SubscribesDir))
	write(t, filepath.Join(dataDir, db.SubscribesDir, "half.yaml"), "half")
	os.Rename(cfg.Path, filepath.Join(parked, db.SQLiteFile))
	m, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg, Version: "test"}, quiet)
	if err != nil || m.Phase != PhaseRestored {
		t.Fatalf("应当接着做完：%+v %v", m, err)
	}
	after, _ := db.Open(context.Background(), cfg)
	defer after.Close()
	if count(t, after, (*model.User)(nil)) != 1 {
		t.Fatal("应当是备份里的数据")
	}
	if c, _ := os.ReadFile(filepath.Join(dataDir, db.SubscribesDir, "a.yaml")); string(c) != "backup-a" {
		t.Fatalf("subscribes/ 应当是备份里的：%q", c)
	}
	if _, err := os.Stat(filepath.Join(dataDir, db.SubscribesDir, "half.yaml")); err == nil {
		t.Fatal("上一次的一半新东西应当清掉")
	}
	if c, _ := os.ReadFile(filepath.Join(parked, db.SubscribesDir, "a.yaml")); string(c) != "current-a" {
		t.Fatalf("停放目录里的原件不应当被盖掉：%q", c)
	}
}

// 有标记而库文件不在：拒绝启动，绝不建空库。
func TestMissingDatabaseWithMarkerRefuses(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	bdb.Close()
	os.Remove(cfg.Path)
	WriteMarker(dataDir, &Marker{Backup: "b.zip", Source: SourceManual, Phase: PhaseFailed, ReplacedDir: "replaced-x"})
	if _, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg}, quiet); v1.AsError(err).Code != v1.CodeDatabase || !strings.Contains(v1.AsError(err).Next, "replaced-x") {
		t.Fatalf("应当拒绝启动并指出原件在哪：%v", err)
	}
	if _, err := os.Stat(cfg.Path); err == nil {
		t.Fatal("不应当建出空库")
	}
}

// 审查第 6 条：自动恢复失败时坏库回到原处、标记留作 failed，下次启动不再重试。
func TestAutoRestoreFailureKeepsMarker(t *testing.T) {
	dataDir, cfg, bdb := sqliteMaster(t)
	bdb.Close()
	corruptFile(t, cfg.Path)
	m := &Manifest{Format: Format, Driver: db.DriverSQLite, Migrations: migrations(t)}
	buildZip(t, Dir(dataDir), "satchel-backup-bad.zip", m, map[string]string{EntrySQLite: "garbage"})
	WriteMarker(dataDir, &Marker{Backup: "satchel-backup-bad.zip", Source: SourceAuto, Phase: PhasePending})
	got, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg}, quiet)
	if err != nil || got.Phase != PhaseFailed {
		t.Fatalf("应当是 failed：%+v %v", got, err)
	}
	if _, err := os.Stat(cfg.Path); err != nil {
		t.Fatal("坏库应当回到原处")
	}
	again, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg}, quiet)
	if err != nil || again.Phase != PhaseFailed || again.ReplacedDir != got.ReplacedDir {
		t.Fatalf("下次启动不应当重试：%+v %v", again, err)
	}
}

// 审查第 7 条：解压后超过上限的备份在校验时就被拒；解压时也边写边数。
func TestExtractLimit(t *testing.T) {
	old := maxExtract
	maxExtract = 1024
	defer func() { maxExtract = old }()
	dir := t.TempDir()
	m := &Manifest{Format: Format, Driver: db.DriverSQLite, Migrations: migrations(t)}
	bomb := buildZip(t, dir, "bomb.zip", m, map[string]string{EntrySQLite: strings.Repeat("\x00", 64<<10)})
	if _, err := Validate(bomb, db.DriverSQLite); v1.AsError(err).Code != v1.CodeBadRequest || !strings.Contains(v1.AsError(err).Reason, "解压") {
		t.Fatalf("解压太大应当 bad_request：%v", err)
	}
	if _, err := extractSQLite(bomb, dir); err == nil {
		t.Fatal("解压时也应当按上限拦住")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("拦住时不应当留下临时文件：%v", entries)
	}
}

// 库文件打不开（权限不足）不算损坏，不自动恢复，原样报错。
func TestUnreadableDatabaseNotRestored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	dataDir, cfg, bdb := sqliteMaster(t)
	if _, err := Create(context.Background(), sqliteSource(dataDir, bdb), PrefixBackup); err != nil {
		t.Fatal(err)
	}
	bdb.Close()
	os.Chmod(cfg.Path, 0)
	defer os.Chmod(cfg.Path, 0o600)
	if _, err := Startup(context.Background(), Target{DataDir: dataDir, Config: cfg}, quiet); err == nil {
		t.Fatal("打不开应当报错")
	}
	if m, _ := ReadMarker(dataDir); m != nil {
		t.Fatalf("不应当触发自动恢复：%+v", m)
	}
}
