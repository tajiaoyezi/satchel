package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func sqliteOnly(t *testing.T) {
	t.Helper()
	for _, name := range []string{"SATCHEL_DATABASE_DRIVER", "SATCHEL_DATABASE_PATH", "SATCHEL_DATABASE_HOST", "SATCHEL_DATABASE_PORT",
		"SATCHEL_DATABASE_NAME", "SATCHEL_DATABASE_USER", "SATCHEL_DATABASE_PASSWORD", "SATCHEL_DATABASE_SSLMODE", db.EnvDataDir, "SATCHEL_OUTPUT"} {
		t.Setenv(name, "")
	}
}

// rawSQL 直接对库文件执行一条语句，模拟人为改动或残留状态。
func rawSQL(t *testing.T, dir, query string, args ...any) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.SQLiteDSN(filepath.Join(dir, db.SQLiteFile)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s：%v", query, err)
	}
}

func TestDBMigrateAndStatus(t *testing.T) {
	sqliteOnly(t)
	dir := filepath.Join(t.TempDir(), "data")
	stdout, stderr, code := run(t, "db", "migrate", "--data-dir", dir)
	if code != 0 {
		t.Fatalf("db migrate 退出码 %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "0001_init") {
		t.Fatalf("migrate 的输出应当含 0001_init：\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, db.SQLiteFile)); err != nil {
		t.Fatalf("数据目录里应当出现 %s：%v", db.SQLiteFile, err)
	}
	// 数据目录布局：迁移把订阅文件与规则模板两个子目录一起建出来，0700。
	for _, sub := range db.DataSubDirs {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !info.IsDir() {
			t.Fatalf("migrate 后数据目录里应当有子目录 %s：%v", sub, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s 权限应当是 0700，得到 %o", sub, perm)
		}
	}
	stdout, _, code = run(t, "db", "status", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatal(code)
	}
	got := decodeJSONObject(t, stdout)
	var st db.MigrationStatus
	if err := jsonUnmarshalFields(got, &st); err != nil {
		t.Fatalf("status --json 解析失败：%v\n%s", err, stdout)
	}
	if strings.Join(st.Applied, ",") != "0001_init" || len(st.Pending) != 0 || !st.Schema.Checked || st.Schema.Consistent == nil || !*st.Schema.Consistent {
		t.Fatalf("status 应当是已应用 0001_init、无待应用、结构一致，得到 %+v", st)
	}
	if !strings.Contains(stdout, `"consistent":true`) || !strings.Contains(stdout, `"extraTables":[]`) || !strings.Contains(stdout, `"bookkeepingMissing":false`) {
		t.Fatalf("status --json 的形状不对：%s", stdout)
	}
	stdout, _, code = run(t, "db", "migrate", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatal(code)
	}
	got = decodeJSONObject(t, stdout)
	if string(got["applied"]) != "[]" {
		t.Fatalf("重跑 migrate --json 的 applied 应当为空清单，得到 %s", stdout)
	}
}

// db status 是只读命令：对不存在的目录也能报全部待应用，且不建目录、不建库、不建迁移表。
func TestDBStatusHasNoSideEffects(t *testing.T) {
	sqliteOnly(t)
	dir := filepath.Join(t.TempDir(), "never")
	stdout, stderr, code := run(t, "db", "status", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("退出码 %d\n%s", code, stderr)
	}
	got := decodeJSONObject(t, stdout)
	var st db.MigrationStatus
	if err := jsonUnmarshalFields(got, &st); err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Pending, ",") != "0001_init" || len(st.Applied) != 0 || st.Schema.Checked || st.Schema.Consistent != nil {
		t.Fatalf("应当报全部待应用、未比对结构且 consistent 为 null，得到 %+v", st)
	}
	if !strings.Contains(stdout, `"consistent":null`) {
		t.Fatalf("没比对时 consistent 应当输出 null：%s", stdout)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("db status 不该创建数据目录")
	}
	_, _, code = run(t, "db", "status", "--data-dir", dir)
	if code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("文本模式的 db status 也不该创建数据目录")
	}
}

// postgres 模式下命令不碰数据目录：目录不存在、也无权创建，status 仍然只是连不上库而不是建目录失败。
func TestDBPostgresModeDoesNotTouchDataDir(t *testing.T) {
	sqliteOnly(t)
	t.Setenv("SATCHEL_DATABASE_DRIVER", "postgres")
	t.Setenv("SATCHEL_DATABASE_HOST", "127.0.0.1")
	t.Setenv("SATCHEL_DATABASE_PORT", "1")
	dir := "/proc/satchel-cannot-create/data"
	_, stderr, code := run(t, "db", "status", "--data-dir", dir, "--json")
	if code != v1.ExitFailure {
		t.Fatalf("连不上库应当退出码 1，得到 %d\n%s", code, stderr)
	}
	e := decodeError(t, stderr)
	if e.Code != v1.CodeDatabase || strings.Contains(e.Reason, "创建数据目录") || strings.Contains(e.Reason, "refused") {
		t.Fatalf("应当是连库失败、reason 不含驱动原文也不提建目录：%+v", e)
	}
	if e.Next == "" {
		t.Fatal("连不上库应当给下一步提示")
	}
}

// 老库对新 0001：没有迁移可应用，但结构不一致要报出来，退出码非 0。
func TestDBMigrateReportsSchemaDrift(t *testing.T) {
	sqliteOnly(t)
	dir := filepath.Join(t.TempDir(), "data")
	if _, stderr, code := run(t, "db", "migrate", "--data-dir", dir); code != 0 {
		t.Fatal(stderr)
	}
	rawSQL(t, dir, "ALTER TABLE tasks ADD COLUMN colour TEXT")
	stdout, stderr, code := run(t, "db", "migrate", "--data-dir", dir, "--json")
	if code != v1.ExitFailure {
		t.Fatalf("结构漂移应当退出码 1，得到 %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	e := decodeError(t, stderr)
	if e.Code != v1.CodeSchemaMismatch || !strings.Contains(e.Reason, "colour") || e.Next == "" {
		t.Fatalf("应当是 schema_mismatch 并点名 colour、给出处置：%+v", e)
	}
	_, stderr, code = run(t, "db", "migrate", "--data-dir", dir)
	if code != v1.ExitFailure || !strings.Contains(stderr, "colour") || !strings.Contains(stderr, "删库重建") {
		t.Fatalf("文本模式应当列出差异并提示删库重建：\n%s", stderr)
	}
	stdout, _, code = run(t, "db", "status", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, "不一致") || !strings.Contains(stdout, "colour") {
		t.Fatalf("db status 应当在输出里列出差异：code=%d\n%s", code, stdout)
	}
}

func TestDBUnlockClearsResidualLock(t *testing.T) {
	sqliteOnly(t)
	dir := filepath.Join(t.TempDir(), "data")
	if _, stderr, code := run(t, "db", "migrate", "--data-dir", dir); code != 0 {
		t.Fatal(stderr)
	}
	rawSQL(t, dir, "INSERT INTO "+db.MigrationLocksTable+" (table_name) VALUES (?)", db.MigrationsTable)
	_, stderr, code := run(t, "db", "migrate", "--data-dir", dir, "--json")
	if code != v1.ExitFailure {
		t.Fatalf("残留锁应当让 migrate 失败，得到 %d\n%s", code, stderr)
	}
	if e := decodeError(t, stderr); e.Code != v1.CodeConflict || !strings.Contains(e.Next, "db unlock") {
		t.Fatalf("应当是 conflict 并提示 db unlock：%+v", e)
	}
	stdout, stderr, code := run(t, "db", "unlock", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("db unlock 退出码 %d\n%s", code, stderr)
	}
	decodeJSONObject(t, stdout)
	if _, stderr, code := run(t, "db", "migrate", "--data-dir", dir); code != 0 {
		t.Fatalf("清锁后 migrate 应当正常：\n%s", stderr)
	}
	stdout, stderr, code = run(t, "db", "unlock", "--data-dir", filepath.Join(t.TempDir(), "nope"), "--json")
	if code != 0 {
		t.Fatalf("库不存在时 unlock 是幂等的、应当成功，得到 %d\n%s", code, stderr)
	}
	decodeJSONObject(t, stdout)
	if _, err := os.Stat(filepath.Join(t.TempDir(), "nope")); !os.IsNotExist(err) {
		t.Fatal("unlock 不该把库建出来")
	}
}

// database.json 坏了是配置错误（config，退出码 1），不是命令行用法错误（退出码 2）。
func TestDBBadConfigIsConfigError(t *testing.T) {
	sqliteOnly(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, db.ConfigFile), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, "db", "status", "--data-dir", dir, "--json")
	if code != v1.ExitFailure {
		t.Fatalf("配置错误应当退出码 1，得到 %d\n%s", code, stderr)
	}
	if e := decodeError(t, stderr); e.Code != v1.CodeConfig || !strings.Contains(e.Reason, "database.json") {
		t.Fatalf("应当是 config 并点名文件：%+v", e)
	}
}

// 迁移记账缺失的老库：migrate 不动库并说明原因，status 标出记账缺失。
func TestDBMigrateOnDatabaseWithoutBookkeeping(t *testing.T) {
	sqliteOnly(t)
	dir := filepath.Join(t.TempDir(), "data")
	if _, stderr, code := run(t, "db", "migrate", "--data-dir", dir); code != 0 {
		t.Fatal(stderr)
	}
	rawSQL(t, dir, "DROP TABLE "+db.MigrationLocksTable)
	rawSQL(t, dir, "DROP TABLE "+db.MigrationsTable)
	_, stderr, code := run(t, "db", "migrate", "--data-dir", dir, "--json")
	if code != v1.ExitFailure {
		t.Fatalf("应当退出码 1，得到 %d\n%s", code, stderr)
	}
	if e := decodeError(t, stderr); e.Code != v1.CodeSchemaMismatch || !strings.Contains(e.Reason, "迁移记账") || e.Next == "" {
		t.Fatalf("应当是 schema_mismatch 并说明记账缺失与处置：%+v", e)
	}
	stdout, _, code := run(t, "db", "status", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, "迁移记账：缺失") || !strings.Contains(stdout, "与注册表一致") {
		t.Fatalf("status 应当标出记账缺失并照常比对：code=%d\n%s", code, stdout)
	}
}

// 库里多出来的表只提示，不让 migrate 失败。
func TestDBExtraTablesAreOnlyReported(t *testing.T) {
	sqliteOnly(t)
	dir := filepath.Join(t.TempDir(), "data")
	if _, stderr, code := run(t, "db", "migrate", "--data-dir", dir); code != 0 {
		t.Fatal(stderr)
	}
	rawSQL(t, dir, "CREATE TABLE someone_elses (id INTEGER)")
	if _, stderr, code := run(t, "db", "migrate", "--data-dir", dir); code != 0 {
		t.Fatalf("多出来的表不该让 migrate 失败：\n%s", stderr)
	}
	stdout, _, code := run(t, "db", "status", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, "与注册表一致") || !strings.Contains(stdout, "someone_elses") {
		t.Fatalf("status 应当一致并列出多出来的表：\n%s", stdout)
	}
}

func TestDBDataDirFromEnv(t *testing.T) {
	sqliteOnly(t)
	dir := t.TempDir()
	t.Setenv(db.EnvDataDir, dir)
	if _, stderr, code := run(t, "db", "migrate"); code != 0 {
		t.Fatalf("db migrate 退出码 %d\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, db.SQLiteFile)); err != nil {
		t.Fatalf("应当按环境变量指定的目录建库：%v", err)
	}
}
