package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
)

// 在线迁移的端到端：SQLite 主控经 CLI（当场验证）迁到一个空的 PostgreSQL（测试的随机 schema），serve 自己停下；
// database.json 指向目标，目标里数据与这个 job 都在，satchel.db 留着；MCP 做不了。
func TestMigrateEndToEnd(t *testing.T) {
	dataDir := shortTempDir(t)
	b := bootFrom(t, dataDir)
	setupAdmin(t, b.tcpURL)
	target := dbtest.OpenPostgres(t)
	b.app.databases.SetOpenTarget(func(context.Context, db.Config) (*bun.DB, func(), error) { return target, func() {}, nil })

	if stdout, _, code := b.cli("database", "show", "--json"); code != 0 || !strings.Contains(stdout, "sqlite") {
		t.Fatalf("database show：%s", stdout)
	}
	flags := []string{"--host", "pg.example", "--name", "satchel", "--user", "satchel", "--sslmode", "disable"}
	if stdout, stderr, code := b.cliPrompt([]string{"pgpass", "pgpass"}, append([]string{"database", "test", "--json"}, flags...)...); code != 0 || !strings.Contains(stdout, `"empty": true`) && !strings.Contains(stdout, `"empty":true`) {
		t.Fatalf("database test：%d %s %s", code, stdout, stderr)
	}
	if text, isErr := b.mcpRun(append([]string{"database", "migrate"}, flags...)...); !isErr || !strings.Contains(text, "human_required") {
		t.Fatalf("MCP 上迁移应当被拒：%s", text)
	}
	// 目标库密码（password 类型的 flag 读两遍）、当场验证的密码、验证码（admin 没开两步验证，留空）依次从「终端」读。
	// 不带 --no-wait：CLI 跟到 done（主控过几秒才停，跟着的 CLI 看得到结局），然后 serve 自己停下。
	stdout, stderr, code := b.cliPrompt([]string{"pgpass", "pgpass", "secret12", ""}, append([]string{"database", "migrate", "--verify-user", "admin", "--json"}, flags...)...)
	if code != 0 || !strings.Contains(stdout, `"done"`) {
		t.Fatalf("迁移应当跟到 done：%d %s %s", code, stdout, stderr)
	}
	b.waitStopped()

	cfg, err := db.LoadConfig(dataDir)
	if err != nil || cfg.Driver != db.DriverPostgres || cfg.Host != "pg.example" || cfg.Password != "pgpass" {
		t.Fatalf("database.json 应当指向目标：%+v %v", cfg, err)
	}
	if n, _ := target.NewSelect().Model((*model.User)(nil)).Where("username = ?", "admin").Count(context.Background()); n != 1 {
		t.Fatal("目标里应当有 admin")
	}
	var job model.Job
	if err := target.NewSelect().Model(&job).Where("kind = ?", "database migrate").Scan(context.Background()); err != nil || job.Status != "done" {
		t.Fatalf("目标里这次迁移的 job 应当是 done：%+v %v", job, err)
	}
	var audits int
	audits, _ = target.NewSelect().Model((*model.AuditLog)(nil)).Where("command = ?", "database migrate").Count(context.Background())
	if audits != 1 {
		t.Fatalf("发起迁移的审计记录应当在快照里、被带到目标：%d", audits)
	}
	if _, err := os.Stat(filepath.Join(dataDir, db.SQLiteFile)); err != nil {
		t.Fatal("satchel.db 应当留着")
	}
}
