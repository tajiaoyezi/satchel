package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func admin() context.Context { return v1.WithIdentity(context.Background(), v1.LocalAdmin("root")) }
func user() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})
}

type fixture struct {
	s       *Service
	dataDir string
	bdb     *bun.DB
	stops   int
	codes   int
}

// newFixture 建服务：长任务用同步的假 StartJob（直接跑完、返回结果），RequestStop 只计数。
func newFixture(t *testing.T, bdb *bun.DB) *fixture {
	t.Helper()
	f := &fixture{dataDir: t.TempDir(), bdb: bdb}
	if err := db.EnsureDataDir(f.dataDir); err != nil {
		t.Fatal(err)
	}
	st := store.New(bdb, schema.Default())
	settings := coresettings.New(bdb, st, schema.Default())
	if err := settings.EnsureSingleton(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver := db.DriverSQLite
	if db.DialectOf(bdb) == schema.Postgres {
		driver = db.DriverPostgres
	}
	f.s = New(Deps{DataDir: f.dataDir, DB: bdb, Driver: driver, Version: "test",
		Users: users.New(bdb, st), Settings: settings, Audit: coreaudit.New(bdb, st),
		StartJob: func(ctx context.Context, _ *command.Invocation, run func(context.Context) (any, error), done func()) (any, error) {
			defer done()
			return run(ctx)
		},
		RequestStop: func() { f.stops++ },
		GenerateCodes: func() ([]string, []string, error) {
			f.codes++
			n := strings.Repeat(string(rune('a'+f.codes)), 8)
			return []string{n}, []string{"hash-" + n}, nil
		},
	})
	return f
}

func (f *fixture) call(t *testing.T, ctx context.Context, name string, args []string, body io.Reader) (any, error) {
	t.Helper()
	inv := &command.Invocation{Path: strings.Fields(name), Args: args, Flags: map[string]any{}, Body: body}
	if name == "backup list" {
		inv.Page = &command.Page{Limit: 50}
	}
	return f.s.Bindings()[name](ctx, inv)
}

func code(err error) v1.Code { return v1.AsError(err).Code }

// sqliteDB 是迁移好的 SQLite 库（备份命令的测试只跑 SQLite：PostgreSQL 的导出要 pg_dump，在 base/backup 里测）。
func sqliteDB(t *testing.T) *bun.DB {
	t.Helper()
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	return bdb
}

// master-backup 的 create / list / download 与权限（SQLite：PostgreSQL 的导出要 pg_dump，在 base/backup 里测）。
func TestCreateListDownload(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	out, err := f.call(t, admin(), "backup create", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := out.(archive.Info)
	res, _ := f.call(t, admin(), "backup list", nil, nil)
	if page := res.(*command.PageResult); page.Total != 1 || page.Items[0].(archive.Info).Name != info.Name {
		t.Fatalf("backup list：%+v", page)
	}
	res, err = f.call(t, admin(), "backup download", []string{info.Name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := res.(*command.File)
	rc, _ := file.Open()
	b, _ := io.ReadAll(rc)
	rc.Close()
	if file.Name != info.Name || int64(len(b)) != info.Size || !bytes.HasPrefix(b, []byte("PK")) {
		t.Fatalf("下载的文件：%+v %d", file, len(b))
	}
	if _, err := f.call(t, admin(), "backup download", []string{"nosuch.zip"}, nil); code(err) != v1.CodeNotFound {
		t.Fatalf("没有的备份应当 not_found：%v", err)
	}
	for _, name := range []string{"backup create", "backup list", "backup download", "backup restore", "backup upload"} {
		if _, err := f.call(t, user(), name, []string{info.Name}, strings.NewReader("x")); code(err) != v1.CodeForbidden {
			t.Errorf("%s 普通用户应当 forbidden：%v", name, err)
		}
	}
}

// master-backup「同时只跑一个」：一次备份在跑时再发起是 conflict。
func TestCreateBusy(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	release := make(chan struct{})
	started := make(chan struct{})
	f.s.d.StartJob = func(ctx context.Context, _ *command.Invocation, run func(context.Context) (any, error), done func()) (any, error) {
		go func() { defer done(); close(started); <-release; run(ctx) }()
		return map[string]string{"job_id": "job-1"}, nil
	}
	if _, err := f.call(t, admin(), "backup create", nil, nil); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := f.call(t, admin(), "backup create", nil, nil); code(err) != v1.CodeConflict {
		t.Fatalf("第二次应当 conflict：%v", err)
	}
	if !f.s.Busy() {
		t.Fatal("应当报告在忙")
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); f.s.Busy() && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if f.s.Busy() {
		t.Fatal("备份跑完应当放开")
	}
}

// master-backup「上传后可以恢复」与校验失败时不留文件、超过上限被拒。
func TestUpload(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	out, _ := f.call(t, admin(), "backup create", nil, nil)
	raw, _ := os.ReadFile(filepath.Join(archive.Dir(f.dataDir), out.(archive.Info).Name))
	res, err := f.call(t, admin(), "backup upload", nil, bytes.NewReader(raw))
	if err != nil || !strings.HasPrefix(res.(archive.Info).Name, archive.PrefixUploaded) {
		t.Fatalf("上传：%+v %v", res, err)
	}
	if _, err := f.call(t, admin(), "backup upload", nil, strings.NewReader("not a zip")); code(err) != v1.CodeBadRequest {
		t.Fatalf("不是 ZIP 应当 bad_request：%v", err)
	}
	old := maxUpload
	maxUpload = 10
	defer func() { maxUpload = old }()
	if _, err := f.call(t, admin(), "backup upload", nil, bytes.NewReader(raw)); code(err) != v1.CodeBadRequest {
		t.Fatalf("超过上限应当 bad_request：%v", err)
	}
	list, _ := archive.List(f.dataDir)
	entries, _ := os.ReadDir(archive.Dir(f.dataDir))
	if len(list) != 2 || len(entries) != 2 {
		t.Fatalf("失败的上传不应当留下文件：%v %d", list, len(entries))
	}
}

// master-backup「发起后退出」：写待恢复标记、让 serve 停止；标记在时再发起是 conflict。初始化向导的恢复同样。
func TestRestoreSchedules(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	out, _ := f.call(t, admin(), "backup create", nil, nil)
	name := out.(archive.Info).Name
	res, err := f.call(t, admin(), "backup restore", []string{name}, nil)
	if err != nil || !res.(*RestartResult).Restarting || f.stops != 1 {
		t.Fatalf("发起恢复：%+v %v stops=%d", res, err, f.stops)
	}
	m, _ := archive.ReadMarker(f.dataDir)
	if m == nil || m.Phase != archive.PhasePending || m.Source != archive.SourceManual || m.Actor != "root" || m.Backup != name {
		t.Fatalf("待恢复标记：%+v", m)
	}
	if _, err := f.call(t, admin(), "backup create", nil, nil); code(err) != v1.CodeConflict {
		t.Fatalf("发起恢复之后再备份应当 conflict：%v", err)
	}

	g := newFixture(t, sqliteDB(t))
	res, err = g.s.SetupRestore(context.Background(), &command.Invocation{Path: []string{"setup", "restore"}, Body: bytes.NewReader(mustRead(t, filepath.Join(archive.Dir(f.dataDir), name)))})
	if err != nil || g.stops != 1 {
		t.Fatalf("向导恢复：%+v %v", res, err)
	}
	if m, _ := archive.ReadMarker(g.dataDir); m == nil || m.Source != archive.SourceSetup || !strings.HasPrefix(m.Backup, archive.PrefixUploaded) {
		t.Fatalf("向导恢复的标记：%+v", m)
	}
}

func mustRead(t *testing.T, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func seedUser(t *testing.T, bdb *bun.DB, name string, totp bool, codes []string) {
	t.Helper()
	raw, _ := json.Marshal(codes)
	now := time.Now().UTC()
	u := &model.User{Username: name, Role: "admin", IsActive: true, PasswordHash: "x", TotpEnabled: totp, RecoveryCodes: raw,
		NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now, ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func codesOf(t *testing.T, bdb *bun.DB, name string) []string {
	t.Helper()
	var u model.User
	if err := bdb.NewSelect().Model(&u).Where("username = ?", name).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var codes []string
	json.Unmarshal(u.RecoveryCodes, &codes)
	return codes
}

func lastRestore(t *testing.T, bdb *bun.DB) LastRestore {
	t.Helper()
	var e model.SystemSettingEntry
	if err := bdb.NewSelect().Model(&e).Where("key = ?", "last_restore").Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var l LastRestore
	json.Unmarshal([]byte(e.Value), &l)
	return l
}

// master-backup「旧恢复码作废」与「收尾中途崩溃」：Finalize 在一个事务里换码、写 last_restore、插审计，提交后写文件、删标记。
func TestFinalize(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := context.Background()
		seedUser(t, bdb, "admin", true, []string{"old1", "old2"})
		seedUser(t, bdb, "bob", false, []string{})
		m := &archive.Marker{Backup: "b.zip", Source: archive.SourceManual, Actor: "root", Phase: archive.PhaseRestored}
		archive.WriteMarker(f.dataDir, m)
		if err := f.s.Finalize(ctx, m); err != nil {
			t.Fatal(err)
		}
		first := codesOf(t, bdb, "admin")
		if len(first) != 1 || first[0] == "old1" || len(codesOf(t, bdb, "bob")) != 0 {
			t.Fatalf("旧码应当全部作废、开了两步验证的换新：%v", first)
		}
		l := lastRestore(t, bdb)
		if l.Result != "ok" || l.Source != archive.SourceManual || l.Backup != "b.zip" || l.RecoveryCodesFile == "" {
			t.Fatalf("last_restore：%+v", l)
		}
		fi, err := os.Stat(l.RecoveryCodesFile)
		if err != nil || fi.Mode().Perm() != 0o600 || filepath.Dir(l.RecoveryCodesFile) != filepath.Join(f.dataDir, db.RecoveryCodesDir) {
			t.Fatalf("恢复码文件：%v %v", l.RecoveryCodesFile, err)
		}
		if content := string(mustRead(t, l.RecoveryCodesFile)); !strings.Contains(content, "admin: ") || strings.Contains(content, "bob:") {
			t.Fatalf("文件内容：%s", content)
		}
		if m, _ := archive.ReadMarker(f.dataDir); m != nil {
			t.Fatal("收尾后标记应当删掉")
		}
		var rec model.AuditLog
		if err := bdb.NewSelect().Model(&rec).Where("command = ?", "backup restore").Scan(ctx); err != nil || rec.Actor != "root" || rec.Result != "ok" {
			t.Fatalf("审计：%+v %v", rec, err)
		}
		// 中途崩溃（标记还是 restored）：重来一遍，再换一批，只有最后这批在库里。
		m2 := &archive.Marker{Backup: "b.zip", Source: archive.SourceManual, Actor: "root", Phase: archive.PhaseRestored}
		archive.WriteMarker(f.dataDir, m2)
		time.Sleep(time.Second) // 文件名精确到秒，让第二个文件名不同
		if err := f.s.Finalize(ctx, m2); err != nil {
			t.Fatal(err)
		}
		if second := codesOf(t, bdb, "admin"); len(second) != 1 || second[0] == first[0] {
			t.Fatalf("重来应当再换一批：%v %v", first, second)
		}
		// 失败的恢复：只写 last_restore 与失败的审计。
		m3 := &archive.Marker{Backup: "bad.zip", Source: archive.SourceManual, Phase: archive.PhaseFailed, Error: "psql 失败"}
		archive.WriteMarker(f.dataDir, m3)
		before := codesOf(t, bdb, "admin")
		if err := f.s.Finalize(ctx, m3); err != nil {
			t.Fatal(err)
		}
		if l := lastRestore(t, bdb); l.Result != "failed" || l.Error != "psql 失败" || l.RecoveryCodesFile != "" {
			t.Fatalf("失败的 last_restore：%+v", l)
		}
		if after := codesOf(t, bdb, "admin"); after[0] != before[0] {
			t.Fatal("失败的恢复不应当换码")
		}
	})
}

// slowBody 在被读到一半时调一次 during（模拟上传期间别的请求做了点什么），然后把剩下的给出去。
type slowBody struct {
	r      io.Reader
	during func()
	once   bool
}

func (b *slowBody) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		b.during()
	}
	return b.r.Read(p)
}

// 审查第 3 条：向导恢复收文件期间有人建了管理员 → 收完之后 conflict、删掉收下的文件、不写标记；收的过程中 setup init 被挡住。
func TestSetupRestoreRace(t *testing.T) {
	src := newFixture(t, sqliteDB(t))
	out, _ := src.call(t, admin(), "backup create", nil, nil)
	raw := mustRead(t, filepath.Join(archive.Dir(src.dataDir), out.(archive.Info).Name))

	f := newFixture(t, sqliteDB(t))
	var guardErr error
	body := &slowBody{r: bytes.NewReader(raw), during: func() {
		guardErr = f.s.SetupGuard() // 收文件期间 setup init 会被挡住
		seedUser(t, f.bdb, "admin", false, []string{})
	}}
	_, err := f.s.SetupRestore(context.Background(), &command.Invocation{Path: []string{"setup", "restore"}, Body: body})
	if code(err) != v1.CodeConflict || code(guardErr) != v1.CodeConflict {
		t.Fatalf("收完后应当 conflict、收的过程中 setup init 应当被挡：%v %v", err, guardErr)
	}
	if list, _ := archive.List(f.dataDir); len(list) != 0 {
		t.Fatalf("收下的文件应当删掉：%v", list)
	}
	if m, _ := archive.ReadMarker(f.dataDir); m != nil || f.stops != 0 || f.s.Busy() {
		t.Fatalf("不应当写标记、不应当停、应当放开：%+v %d", m, f.stops)
	}
}

// 审查第 4 条：发起恢复之后上传是 conflict；保留清理保护标记里点名的那份。
func TestUploadRespectsRestore(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	out, _ := f.call(t, admin(), "backup create", nil, nil)
	name := out.(archive.Info).Name
	raw := mustRead(t, filepath.Join(archive.Dir(f.dataDir), name))
	// 让要恢复的那份变成最旧的，再塞 7 份更新的。
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(archive.Dir(f.dataDir), name), old, old)
	for i := 0; i < archive.Keep; i++ {
		os.WriteFile(filepath.Join(archive.Dir(f.dataDir), "satchel-backup-new"+string(rune('a'+i))+".zip"), raw, 0o600)
	}
	archive.WriteMarker(f.dataDir, &archive.Marker{Backup: name, Source: archive.SourceManual, Phase: archive.PhasePending})
	if _, err := f.call(t, admin(), "backup upload", nil, bytes.NewReader(raw)); code(err) != v1.CodeConflict {
		t.Fatalf("发起恢复之后上传应当 conflict：%v", err)
	}
	f.s.prune("satchel-backup-newa.zip")
	if _, err := os.Stat(filepath.Join(archive.Dir(f.dataDir), name)); err != nil {
		t.Fatal("标记里点名的备份不应当被清理掉")
	}
}
