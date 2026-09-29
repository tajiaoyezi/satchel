package database

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	"github.com/satchel/satchel/internal/service/jobs"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func admin() context.Context { return v1.WithIdentity(context.Background(), v1.LocalAdmin("root")) }

func init() { stopDelay = 10 * time.Millisecond }

type fixture struct {
	s       *Service
	dataDir string
	src     *bun.DB
	target  *bun.DB
	jobs    *jobs.Service
	gate    *db.WriteGate
	mu      sync.Mutex
	locked  bool
	stops   int
}

// newFixture：源是数据目录里的 SQLite（像 serve 那样），目标是 PostgreSQL 的随机 schema，长任务用真的 service/jobs。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dataDir: t.TempDir(), gate: &db.WriteGate{}}
	cfg := db.Config{Driver: db.DriverSQLite, Path: filepath.Join(f.dataDir, db.SQLiteFile)}
	src, err := db.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	if _, err := db.Migrate(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	f.src = src
	f.target = dbtest.OpenPostgres(t)
	f.jobs = jobs.New(corejobs.New(src, store.New(src, schema.Default())), command.Catalog(),
		func(*command.Command, *command.Invocation) string { return `{"args":[],"flags":{}}` }, nil)
	t.Cleanup(func() { f.jobs.Stop(context.Background()) })
	f.s = New(Deps{DataDir: f.dataDir, DB: src, Config: cfg, Gate: f.gate, Registry: schema.Default(),
		Lock: func(what string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.locked {
				return v1.Newf(v1.CodeConflict, "已有一次%s在进行", what)
			}
			f.locked = true
			return nil
		},
		Unlock: func() { f.mu.Lock(); f.locked = false; f.mu.Unlock() },
		StartJob: func(ctx context.Context, inv *command.Invocation, run func(context.Context) (any, error), done func()) (any, error) {
			return f.jobs.Start(ctx, inv, run, done)
		},
		RequestStop: func() { f.mu.Lock(); f.stops++; f.mu.Unlock() },
		OpenTarget:  func(context.Context, db.Config) (*bun.DB, func(), error) { return f.target, func() {}, nil },
	})
	return f
}

// settled 等锁放开（done 回调在 job 写完结局之后才跑），返回此刻的锁、拦截与停止次数。
func (f *fixture) settled() (locked, suspended bool, stops int) {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		locked, stops = f.locked, f.stops
		f.mu.Unlock()
		if !locked || time.Now().After(deadline) {
			return locked, f.gate.Suspended(), stops
		}
	}
}

func targetFlags() map[string]any {
	return map[string]any{"host": "pg.example", "port": 5432, "name": "satchel", "user": "satchel", "password": "pw", "sslmode": "require"}
}

func (f *fixture) call(t *testing.T, ctx context.Context, name string) (any, error) {
	t.Helper()
	return f.s.Bindings()[name](ctx, &command.Invocation{Path: strings.Fields(name), Flags: targetFlags()})
}

// migrate 发起迁移并模拟受理请求结束，等 job 结束，返回 SQLite 里那一行。
func (f *fixture) migrate(t *testing.T) (*corejobs.Job, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(admin())
	out, err := f.call(t, ctx, "database migrate")
	cancel()
	if err != nil {
		return nil, err
	}
	id := out.(*corejobs.Job).JobID
	// 经 job get 看（成功时结局写不进 SQLite——写锁一直拿着——只在内存里）。
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		got, err := f.jobs.Bindings()["job get"](admin(), &command.Invocation{Path: []string{"job", "get"}, Args: []string{id}, Flags: map[string]any{}})
		if j, ok := got.(*corejobs.Job); err == nil && ok && j.Finished() {
			return j, nil
		}
	}
	t.Fatal("迁移没在限时内结束")
	return nil, nil
}

func seed(t *testing.T, bdb *bun.DB) {
	t.Helper()
	now := time.Now().UTC()
	u := &model.User{Username: "admin", Role: "admin", IsActive: true, PasswordHash: "h", RecoveryCodes: json.RawMessage(`[]`),
		NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now, ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// master-db-migration「看当前的数据库」「试连目标库」。
func TestShowAndTest(t *testing.T) {
	f := newFixture(t)
	out, err := f.call(t, admin(), "database show")
	info := out.(Info)
	if err != nil || info.Driver != db.DriverSQLite || info.Path == "" || info.Size == 0 || info.EnvOverride {
		t.Fatalf("show：%+v %v", info, err)
	}
	out, err = f.call(t, admin(), "database test")
	res := out.(TestResult)
	if err != nil || !res.Empty || res.ServerVersion == "" {
		t.Fatalf("test：%+v %v", res, err)
	}
	if info, _ := db.PGInfo(context.Background(), f.target); len(info.Tables) != 0 {
		t.Fatal("试连不应当在目标库上建东西")
	}
}

// master-db-migration「迁移的前提」：每一条反例都不受理 job。
func TestPreconditions(t *testing.T) {
	f := newFixture(t)
	f.s.d.Config.Driver = db.DriverPostgres
	if _, err := f.call(t, admin(), "database migrate"); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("已是 PostgreSQL 应当 conflict：%v", err)
	}
	f.s.d.Config.Driver = db.DriverSQLite
	t.Setenv("SATCHEL_DATABASE_HOST", "x")
	if _, err := f.call(t, admin(), "database migrate"); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("环境变量覆盖应当 conflict：%v", err)
	}
	t.Setenv("SATCHEL_DATABASE_HOST", "")
	f.locked = true
	if _, err := f.call(t, admin(), "database migrate"); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("锁被占着应当 conflict：%v", err)
	}
	f.locked = false
	f.s.d.OpenTarget = func(context.Context, db.Config) (*bun.DB, func(), error) {
		return nil, nil, errors.New("connection refused")
	}
	if _, err := f.call(t, admin(), "database migrate"); v1.AsError(err).Code != v1.CodeUnavailable || !strings.Contains(v1.AsError(err).Reason, "connection refused") {
		t.Fatalf("连不上应当 unavailable、reason 带驱动的原因：%v", err)
	}
	if locked, _, _ := f.settled(); locked {
		t.Fatal("连不上时应当放开锁")
	}
	f.s.d.OpenTarget = func(context.Context, db.Config) (*bun.DB, func(), error) { return f.target, func() {}, nil }
	if _, err := f.target.ExecContext(context.Background(), "CREATE TABLE t (id int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, admin(), "database migrate"); v1.AsError(err).Code != v1.CodeConflict || !strings.Contains(v1.AsError(err).Reason, "t") {
		t.Fatalf("目标不为空应当 conflict 并点名：%v", err)
	}
	if n, _ := f.src.NewSelect().Model((*model.Job)(nil)).Count(context.Background()); n != 0 {
		t.Fatal("前提不满足时不应当受理 job")
	}
}

// master-db-migration「数据原样过去」「提交后退出」：成功时 database.json 改成 PostgreSQL，目标里行数一致、job 行是 done，serve 被要求停止。
func TestMigrateSuccess(t *testing.T) {
	f := newFixture(t)
	seed(t, f.src)
	j, err := f.migrate(t)
	if err != nil || j.Status != corejobs.StatusDone {
		t.Fatalf("迁移：%+v %v", j, err)
	}
	cfg, err := db.LoadConfig(f.dataDir)
	if err != nil || cfg.Driver != db.DriverPostgres || cfg.Host != "pg.example" || cfg.Password != "pw" || cfg.SSLMode != "require" {
		t.Fatalf("database.json：%+v %v", cfg, err)
	}
	if n, _ := f.target.NewSelect().Model((*model.User)(nil)).Count(context.Background()); n != 1 {
		t.Fatalf("目标里应当有那个用户：%d", n)
	}
	var tj model.Job
	if err := f.target.NewSelect().Model(&tj).Where("job_id = ?", j.JobID).Scan(context.Background()); err != nil || tj.Status != corejobs.StatusDone {
		t.Fatalf("目标里这个 job 应当是 done：%+v %v", tj, err)
	}
	time.Sleep(100 * time.Millisecond) // 停止是 stopDelay 之后才要求的
	if locked, suspended, stops := f.settled(); stops != 1 || !suspended || locked {
		t.Fatalf("应当要求停止、拦截保持、锁放开：stops=%d gate=%v locked=%v", stops, suspended, locked)
	}
	// 审查第 1 条：成功后写锁一直拿到进程退出，别的连接写不进 SQLite（等满 busy_timeout 失败）。
	other, err := db.Open(context.Background(), f.s.d.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExecContext(context.Background(), "UPDATE users SET email = 'x'"); err == nil {
		t.Fatal("成功之后 SQLite 的写锁应当一直拿着，别的写入写不进去")
	}
}

// master-db-migration「拷贝中途失败」：目标清空、database.json 没变、拦截与锁放开，job 是 failed。
func TestMigrateFailure(t *testing.T) {
	f := newFixture(t)
	seed(t, f.src)
	// SQLite 的整数列里塞一个文本：拷贝时转不成 PostgreSQL 的整数。
	if _, err := f.src.ExecContext(context.Background(), "UPDATE users SET resource_version = 'abc'"); err != nil {
		t.Fatal(err)
	}
	j, err := f.migrate(t)
	if err != nil || j.Status != corejobs.StatusFailed || !strings.Contains(*j.Output, "resource_version") {
		t.Fatalf("应当失败并点名那一列：%+v %v", j, err)
	}
	if cfg, _ := db.LoadConfig(f.dataDir); cfg.Driver != db.DriverSQLite {
		t.Fatal("database.json 不应当变")
	}
	if info, _ := db.PGInfo(context.Background(), f.target); len(info.Tables) != 0 {
		t.Fatalf("目标应当清空：%v", info.Tables)
	}
	if locked, suspended, stops := f.settled(); suspended || locked || stops != 0 {
		t.Fatalf("拦截与锁应当放开、不应当停止：gate=%v locked=%v stops=%d", suspended, locked, stops)
	}
}

// 审查第 4 条：reason 带驱动的原因，但去掉密码与连接串。
func TestDriverReason(t *testing.T) {
	cfg := db.Config{Password: "s3cret"}
	got := driverReason(v1.Wrap(v1.CodeDatabase, "连接数据库失败", errors.New("failed to connect to postgres://u:s3cret@h/db: password authentication failed (s3cret)")), cfg)
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "password authentication failed") {
		t.Fatalf("应当去掉密码、留下原因：%s", got)
	}
}
