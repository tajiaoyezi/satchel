package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
)

func TestDBMigrateAndStatus(t *testing.T) {
	t.Setenv("SATCHEL_DATABASE_DRIVER", "")
	dir := filepath.Join(t.TempDir(), "data")
	stdout, stderr, err := run(t, "db", "migrate", "--data-dir", dir)
	if err != nil {
		t.Fatalf("db migrate 出错：%v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "0001_init") {
		t.Fatalf("migrate 的输出应当含 0001_init：\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, db.SQLiteFile)); err != nil {
		t.Fatalf("数据目录里应当出现 %s：%v", db.SQLiteFile, err)
	}
	stdout, _, err = run(t, "db", "status", "--data-dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st db.MigrationStatus
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatalf("status --json 不是合法 JSON：%v\n%s", err, stdout)
	}
	if strings.Join(st.Applied, ",") != "0001_init" || len(st.Pending) != 0 {
		t.Fatalf("status 应当是已应用 0001_init、无待应用，得到 %+v", st)
	}
	stdout, _, err = run(t, "db", "migrate", "--data-dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != `{"applied":[]}` {
		t.Fatalf("重跑 migrate --json 应当输出空清单，得到 %s", stdout)
	}
}

func TestDBDataDirFromEnv(t *testing.T) {
	t.Setenv("SATCHEL_DATABASE_DRIVER", "")
	dir := t.TempDir()
	t.Setenv(db.EnvDataDir, dir)
	if _, stderr, err := run(t, "db", "migrate"); err != nil {
		t.Fatalf("db migrate 出错：%v\n%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, db.SQLiteFile)); err != nil {
		t.Fatalf("应当按环境变量指定的目录建库：%v", err)
	}
}
