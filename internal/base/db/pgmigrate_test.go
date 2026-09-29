package db_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
)

func migrated(t *testing.T, bdb *bun.DB) *bun.DB {
	t.Helper()
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	return bdb
}

// seedAll 往 SQLite 里放各种类型的值：布尔、时间、JSON、可空列、会话、设置键值表、长任务。
func seedAll(t *testing.T, bdb *bun.DB) time.Time {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 9, 27, 1, 2, 3, 456789000, time.UTC)
	for _, name := range []string{"alice", "bob"} {
		u := &model.User{Username: name, Role: "admin", IsActive: true, PasswordHash: "h", TotpEnabled: name == "alice",
			RecoveryCodes: json.RawMessage(`["x","y"]`), NodeSpeedLimitOverrides: json.RawMessage(`{"1":5}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`),
			CreatedAt: at, UpdatedAt: at, ResourceVersion: 3}
		if _, err := bdb.NewInsert().Model(u).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bdb.NewInsert().Model(&model.Session{TokenHash: "th", Username: "alice", ExpiresAt: at.Add(time.Hour), CreatedAt: at}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := bdb.NewInsert().Model(&model.SystemSettingEntry{Key: "branding_site_title", Value: "S", UpdatedAt: at}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := bdb.NewInsert().Model(&model.Job{JobID: "job-1", Kind: "k", Args: json.RawMessage(`{}`), Status: "running", CreatedAt: at, UpdatedAt: at, ResourceVersion: 1}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// 用库默认值写时间（CURRENT_TIMESTAMP 的文本写法）。
	if _, err := bdb.ExecContext(ctx, "INSERT INTO audit_logs (actor, actor_kind, command, args_digest, result) VALUES ('root', 'local', 'whoami', '{}', 'ok')"); err != nil {
		t.Fatal(err)
	}
	return at
}

// master-db-migration「拷贝与核对」「数据原样过去」：各种类型的值原样到 PostgreSQL，序列接着最大 id，清理后目标回到空的。
func TestCopyToPostgres(t *testing.T) {
	ctx := context.Background()
	src := migrated(t, dbtest.OpenSQLite(t))
	at := seedAll(t, src)
	dst := dbtest.OpenPostgres(t)
	info, err := db.PGInfo(ctx, dst)
	if err != nil || len(info.Tables) != 0 || info.Major < 10 || info.Version == "" {
		t.Fatalf("空 schema 的概况：%+v %v", info, err)
	}
	migrated(t, dst)
	srcTx, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srcTx.Rollback()
	dstTx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen []db.CopyProgress
	rep, err := db.CopyToPostgres(ctx, srcTx, dstTx, schema.Default(), func(p db.CopyProgress) { seen = append(seen, p) })
	if err == nil {
		err = db.VerifyCounts(ctx, srcTx, dstTx, schema.Default())
	}
	if err != nil {
		dstTx.Rollback()
		t.Fatal(err)
	}
	if err := dstTx.Commit(); err != nil {
		t.Fatal(err)
	}
	if rep.Tables != len(schema.Default().Tables()) || rep.Rows < 6 || len(seen) != rep.Tables || seen[len(seen)-1].TablesDone != rep.Tables {
		t.Fatalf("报告：%+v，进度 %d 次", rep, len(seen))
	}
	var users []model.User
	if err := dst.NewSelect().Model(&users).OrderExpr("id").Scan(ctx); err != nil || len(users) != 2 {
		t.Fatalf("用户：%v %v", users, err)
	}
	a := users[0]
	if !a.TotpEnabled || !a.IsActive || !a.CreatedAt.Equal(at) || a.ResourceVersion != 3 || string(a.RecoveryCodes) != `["x", "y"]` && string(a.RecoveryCodes) != `["x","y"]` {
		t.Fatalf("alice 的值没有原样过去：%+v", a)
	}
	var job model.Job
	if err := dst.NewSelect().Model(&job).Where("job_id = ?", "job-1").Scan(ctx); err != nil || job.ExitCode != nil || job.StartedAt != nil {
		t.Fatalf("可空列应当还是 NULL：%+v %v", job, err)
	}
	var audit model.AuditLog
	if err := dst.NewSelect().Model(&audit).Scan(ctx); err != nil || audit.At.IsZero() {
		t.Fatalf("库默认值写的时间应当认得出：%+v %v", audit, err)
	}
	// 序列：新插一行接着最大 id。
	u := &model.User{Username: "carol", Role: "user", PasswordHash: "h", RecoveryCodes: json.RawMessage(`[]`), NodeSpeedLimitOverrides: json.RawMessage(`{}`),
		NodeDeviceLimitOverrides: json.RawMessage(`{}`), CreatedAt: at, UpdatedAt: at, ResourceVersion: 1}
	if _, err := dst.NewInsert().Model(u).Exec(ctx); err != nil || u.ID != users[1].ID+1 {
		t.Fatalf("新行应当接着最大 id：%d %v", u.ID, err)
	}
	// 清理：目标回到空的。
	if err := db.DropCreated(ctx, dst, schema.Default()); err != nil {
		t.Fatal(err)
	}
	if info, _ := db.PGInfo(ctx, dst); len(info.Tables) != 0 {
		t.Fatalf("清理后应当没有表：%v", info.Tables)
	}
}
