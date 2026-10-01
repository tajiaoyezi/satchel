package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/selfupdate"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func admin() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{Actor: "root", ActorKind: v1.ActorLocalAdmin, Role: v1.RoleAdmin})
}

func user() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})
}

// jobOutcome 是同步的假长任务跑完的结果。
type jobOutcome struct {
	result any
	err    error
}

type fixture struct {
	s        *Service
	dataDir  string
	root     string
	target   string
	files    map[string][]byte
	gh       *httptest.Server
	hits     atomic.Int64 // 假 GitHub 收到的请求数
	priv     ed25519.PrivateKey
	gate     *db.WriteGate
	locked   int
	unlocked int
	backups  []string
	execs    []string
	phases   []string
	backupFn func(ctx context.Context) (string, error)
}

func newFixture(t *testing.T, bdb *bun.DB) *fixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fixture{dataDir: t.TempDir(), root: t.TempDir(), files: map[string][]byte{}, priv: priv, gate: &db.WriteGate{}}
	f.target = filepath.Join(t.TempDir(), "satchel")
	os.WriteFile(f.target, []byte("binary 0.1.0"), 0o755)
	f.gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		data, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(f.gh.Close)
	f.release("v0.1.1", []byte("binary 0.1.1"))
	settings := coresettings.New(bdb, store.New(bdb, schema.Default()), schema.Default())
	if err := settings.EnsureSingleton(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.backupFn = func(context.Context) (string, error) {
		name := "before-upgrade-x.zip"
		f.backups = append(f.backups, name)
		return name, nil
	}
	f.s = New(Deps{DataDir: f.dataDir, Version: "0.1.0", Settings: settings,
		Sources: selfupdate.Sources{Repo: "o/satchel", API: f.gh.URL + "/api", Web: f.gh.URL + "/web"},
		Verify: func(bin, sig string) error {
			b, _ := os.ReadFile(bin)
			g, _ := os.ReadFile(sig)
			if !ed25519.Verify(pub, b, g) {
				return errors.New("签名不匹配")
			}
			return nil
		},
		Root: f.root, GOOS: "linux", GOARCH: "amd64",
		Executable: func() (string, error) { return f.target, nil },
		RunVersion: func(_ context.Context, path string) (string, error) {
			data, err := os.ReadFile(path)
			return strings.TrimPrefix(string(data), "binary "), err
		},
		Lock: func(string) error {
			if f.locked > f.unlocked {
				return v1.New(v1.CodeConflict, "已有一次升级在进行")
			}
			f.locked++
			return nil
		},
		Unlock:       func() { f.unlocked++ },
		CreateBackup: func(ctx context.Context) (string, error) { return f.backupFn(ctx) },
		Gate:         f.gate,
		StartJob: func(ctx context.Context, _ *command.Invocation, run func(context.Context) (any, error), done func()) (any, error) {
			defer done()
			ctx = corejobs.WithRun(ctx, "job-00000000000000aa", func(v any) { f.phases = append(f.phases, v.(Progress).Phase) })
			res, err := run(ctx)
			return jobOutcome{res, err}, nil
		},
		RequestExec: func(path string) { f.execs = append(f.execs, path) },
	})
	return f
}

// release 让假 GitHub 的最新正式版是 tag，二进制是 bin（签好名）。
func (f *fixture) release(tag string, bin []byte) {
	f.files["/api/repos/o/satchel/releases/latest"] = []byte(`{"tag_name":"` + tag + `","html_url":"https://example/r","body":"notes"}`)
	path := "/web/o/satchel/releases/download/" + tag + "/" + selfupdate.BinaryName("amd64")
	f.files[path], f.files[path+".sig"] = bin, ed25519.Sign(f.priv, bin)
}

func (f *fixture) call(t *testing.T, ctx context.Context, name string, args []string, flags map[string]any) (any, error) {
	t.Helper()
	if flags == nil {
		flags = map[string]any{}
	}
	return f.s.Bindings()[name](ctx, &command.Invocation{Path: strings.Fields(name), Args: args, Flags: flags})
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return string(data)
}

func sqliteDB(t *testing.T) *bun.DB {
	t.Helper()
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	return bdb
}

// master-self-update「检查更新」：有新版本、Docker 里能查不能升、普通用户不行。
func TestCheck(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	out, err := f.call(t, admin(), "update check", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(*CheckResult)
	if res.LatestVersion != "0.1.1" || !res.HasUpdate || res.Channel != "stable" || res.Source != selfupdate.SourceGitHub || !res.CanApply || res.CurrentVersion != "0.1.0" {
		t.Fatalf("%+v", res)
	}
	if !res.CDN.Enabled || res.CDN.Used || !strings.Contains(res.CDN.Reason, "域名") {
		t.Fatalf("CDN 开着但域名为空：%+v", res.CDN)
	}
	os.WriteFile(filepath.Join(f.root, ".dockerenv"), nil, 0o644)
	out, _ = f.call(t, admin(), "update check", nil, nil)
	if res := out.(*CheckResult); res.CanApply || !strings.Contains(res.Reason, "镜像") {
		t.Fatalf("Docker 里应当不能升：%+v", res)
	}
	if _, err := f.call(t, user(), "update check", nil, nil); v1.AsError(err).Code != v1.CodeForbidden {
		t.Fatalf("普通用户：%v", err)
	}
	if _, err := f.call(t, admin(), "update check", nil, map[string]any{"channel": "nightly"}); v1.AsError(err).Code != v1.CodeBadRequest {
		t.Fatalf("渠道不对：%v", err)
	}
	delete(f.files, "/api/repos/o/satchel/releases/latest")
	if _, err := f.call(t, admin(), "update check", nil, nil); v1.AsError(err).Code != v1.CodeUnavailable {
		t.Fatalf("取不到版本应当 unavailable：%v", err)
	}
}

// master-self-update「应用升级的前提」的每一条反例：都不受理、不下载。
func TestApplyPreconditions(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(f *fixture)
		version string
		want    string
	}{
		{"Docker", func(f *fixture) { os.WriteFile(filepath.Join(f.root, ".dockerenv"), nil, 0o644) }, "0.1.1", "docker compose"},
		{"开发版", func(f *fixture) { f.s.d.Version = "0.0.0-dev" }, "0.1.1", "开发版"},
		{"不是 Linux", func(f *fixture) { f.s.d.GOOS = "darwin" }, "0.1.1", "Linux"},
		{"已是最新", func(f *fixture) { f.s.d.Version = "0.1.1" }, "0.1.1", "已是最新"},
		{"版本号不对", func(f *fixture) {}, "0.1.2", "0.1.1"},
		{"锁被占着", func(f *fixture) { f.locked = 1 }, "0.1.1", "升级"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, sqliteDB(t))
			c.prepare(f)
			_, err := f.call(t, admin(), "update apply", []string{c.version}, nil)
			e := v1.AsError(err)
			if e.Code != v1.CodeConflict || !strings.Contains(e.Reason+e.Next, c.want) {
				t.Fatalf("应当 conflict 并提到 %q：%v", c.want, err)
			}
			if len(f.phases) != 0 || read(t, f.target) != "binary 0.1.0" {
				t.Fatal("不应当受理")
			}
		})
	}
}

// master-self-update「升级成功」的前半段（exec 之前）：替换、.bak、升级前备份、升级标记、写入暂停、交出结局、要求 exec。
func TestApplySwitches(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	out, err := f.call(t, admin(), "update apply", []string{"v0.1.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o := out.(jobOutcome); !errors.Is(o.err, corejobs.ErrHandedOff) {
		t.Fatalf("成功时应当交出结局：%+v", o)
	}
	if read(t, f.target) != "binary 0.1.1" || read(t, selfupdate.PreviousPath(f.target)) != "binary 0.1.0" {
		t.Fatalf("目标路径应当换成新版，.bak 是旧版：%q %q", read(t, f.target), read(t, selfupdate.PreviousPath(f.target)))
	}
	m, err := selfupdate.ReadMarker(f.dataDir)
	if err != nil || m == nil || m.Phase != selfupdate.PhaseSwitching || m.JobID != "job-00000000000000aa" || m.FromVersion != "0.1.0" ||
		m.ToVersion != "0.1.1" || m.Backup != "before-upgrade-x.zip" || m.Target != f.target || m.Actor != "root" {
		t.Fatalf("升级标记：%+v %v", m, err)
	}
	if !f.gate.Suspended() || len(f.execs) != 1 || f.execs[0] != f.target || f.unlocked != 1 {
		t.Fatalf("写入应当暂停、要求 exec 目标路径、锁在 job 结束时放开：%v %v %d", f.gate.Suspended(), f.execs, f.unlocked)
	}
	if reason, _ := f.gate.Why(); !strings.Contains(reason, "升级") {
		t.Fatalf("暂停的原因：%s", reason)
	}
	if strings.Join(f.phases, ",") != "downloading,downloading,verifying,backing_up,switching,restarting" {
		t.Fatalf("进度：%v", f.phases)
	}
	if entries, _ := os.ReadDir(filepath.Dir(f.target)); len(entries) != 2 {
		t.Fatalf("目标目录里只应当有新二进制与 .bak：%d", len(entries))
	}
}

// master-self-update「验签失败什么都不动」。
func TestApplyBadSignature(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	path := "/web/o/satchel/releases/download/v0.1.1/" + selfupdate.BinaryName("amd64")
	f.files[path] = []byte("binary 0.1.1 evil")
	out, err := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o := out.(jobOutcome); v1.AsError(o.err).Code != v1.CodeUnavailable || !strings.Contains(v1.AsError(o.err).Reason, "验签") {
		t.Fatalf("应当失败并说明验签：%+v", o)
	}
	if read(t, f.target) != "binary 0.1.0" || f.gate.Suspended() || len(f.backups) != 0 || len(f.execs) != 0 {
		t.Fatal("什么都不应当动")
	}
	if _, err := os.Stat(selfupdate.PreviousPath(f.target)); !os.IsNotExist(err) {
		t.Fatal("不应当有 .bak")
	}
	if m, _ := selfupdate.ReadMarker(f.dataDir); m != nil {
		t.Fatal("不应当有升级标记")
	}
	if entries, _ := os.ReadDir(filepath.Dir(f.target)); len(entries) != 1 {
		t.Fatalf("不应当留下临时文件：%d", len(entries))
	}
}

// 试跑的版本号对不上、备份失败：都撤回，写入放开。
func TestApplyRollsBackEarlySteps(t *testing.T) {
	t.Run("版本号对不上", func(t *testing.T) {
		f := newFixture(t, sqliteDB(t))
		f.release("v0.1.1", []byte("binary 0.1.9"))
		out, _ := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil)
		if o := out.(jobOutcome); o.err == nil || !strings.Contains(v1.AsError(o.err).Reason, "0.1.9") {
			t.Fatalf("%+v", o)
		}
		if read(t, f.target) != "binary 0.1.0" || f.gate.Suspended() || len(f.backups) != 0 {
			t.Fatal("不应当动")
		}
	})
	t.Run("备份失败", func(t *testing.T) {
		f := newFixture(t, sqliteDB(t))
		f.backupFn = func(context.Context) (string, error) { return "", v1.New(v1.CodeUnavailable, "没有 pg_dump") }
		out, _ := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil)
		if o := out.(jobOutcome); v1.AsError(o.err).Reason != "没有 pg_dump" {
			t.Fatalf("%+v", o)
		}
		if read(t, f.target) != "binary 0.1.0" || f.gate.Suspended() || len(f.execs) != 0 {
			t.Fatal("应当撤回：二进制不动、写入放开")
		}
		if m, _ := selfupdate.ReadMarker(f.dataDir); m != nil {
			t.Fatal("不应当有升级标记")
		}
	})
	t.Run("替换失败", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 不受目录权限限制")
		}
		f := newFixture(t, sqliteDB(t))
		dir := filepath.Dir(f.target)
		f.backupFn = func(context.Context) (string, error) {
			os.Chmod(dir, 0o500) // 目标目录变成只读：.bak 写不进去
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			return "before-upgrade-x.zip", nil
		}
		out, _ := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil)
		if o := out.(jobOutcome); o.err == nil {
			t.Fatalf("%+v", o)
		}
		os.Chmod(dir, 0o700)
		if read(t, f.target) != "binary 0.1.0" || f.gate.Suspended() || len(f.execs) != 0 {
			t.Fatal("应当撤回")
		}
		if m, _ := selfupdate.ReadMarker(f.dataDir); m != nil {
			t.Fatal("升级标记应当删掉")
		}
	})
}

// 审查：改名已经成功、目录落盘失败时当作已替换——放回 .bak、删标记、放开写入；放回也失败时保留标记、写入继续暂停。
func TestApplyInstallSyncFails(t *testing.T) {
	prev := install
	t.Cleanup(func() { install = prev })

	t.Run("放回成功", func(t *testing.T) {
		f := newFixture(t, sqliteDB(t))
		install = func(staged, target string) (bool, error) {
			replaced, _ := selfupdate.Install(staged, target)
			return replaced, errors.New("fsync 失败")
		}
		out, _ := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil)
		if o := out.(jobOutcome); o.err == nil {
			t.Fatalf("%+v", o)
		}
		if read(t, f.target) != "binary 0.1.0" || f.gate.Suspended() || len(f.execs) != 0 {
			t.Fatalf("应当放回旧二进制、放开写入：%q", read(t, f.target))
		}
		if m, _ := selfupdate.ReadMarker(f.dataDir); m != nil {
			t.Fatal("升级标记应当删掉")
		}
	})
	t.Run("放回也失败", func(t *testing.T) {
		f := newFixture(t, sqliteDB(t))
		install = func(staged, target string) (bool, error) {
			replaced, _ := selfupdate.Install(staged, target)
			os.Remove(selfupdate.PreviousPath(target)) // .bak 没了，放不回去
			return replaced, errors.New("fsync 失败")
		}
		out, _ := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil)
		if o := out.(jobOutcome); o.err == nil || !strings.Contains(v1.AsError(o.err).Reason, "升级标记已保留") {
			t.Fatalf("%+v", o)
		}
		if read(t, f.target) != "binary 0.1.1" || !f.gate.Suspended() {
			t.Fatal("新二进制还在目标路径上：写入应当继续暂停")
		}
		if reason, next := f.gate.Why(); !strings.Contains(reason, "放回旧二进制也失败") || !strings.Contains(next, "重启主控") {
			t.Fatalf("暂停的说明应当换成升级失败、要重启：%s / %s", reason, next)
		}
		if m, _ := selfupdate.ReadMarker(f.dataDir); m == nil || m.Phase != selfupdate.PhaseSwitching {
			t.Fatalf("升级标记应当保留：%+v", m)
		}
	})
}

// 审查：暂停写入之前已经进门的命令还没走完时不备份；等不到它走完就失败、什么都不动。
func TestApplyWaitsInflightWrites(t *testing.T) {
	prev := drainTimeout
	drainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { drainTimeout = prev })
	f := newFixture(t, sqliteDB(t))
	if !f.gate.Enter() { // 一个进门了、还没走完的写命令
		t.Fatal("开关没开时应当能进门")
	}
	out, _ := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil)
	if o := out.(jobOutcome); o.err == nil || !strings.Contains(v1.AsError(o.err).Reason, "没有走完") {
		t.Fatalf("%+v", o)
	}
	if len(f.backups) != 0 || read(t, f.target) != "binary 0.1.0" || f.gate.Suspended() {
		t.Fatal("不应当备份、不应当替换，写入应当放开")
	}
	f.gate.Leave()
	if _, err := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.backups) != 1 || read(t, f.target) != "binary 0.1.1" {
		t.Fatal("没有进门的请求时照常升级")
	}
}

// 审查：先拿锁再联网取版本；拿锁之后的任一步失败都放锁（取不到版本、版本号不对），锁被占着时根本不联网。
func TestApplyLockOrder(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	delete(f.files, "/api/repos/o/satchel/releases/latest")
	if _, err := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil); v1.AsError(err).Code != v1.CodeUnavailable {
		t.Fatalf("取不到版本：%v", err)
	}
	f.release("v0.1.1", []byte("binary 0.1.1"))
	if _, err := f.call(t, admin(), "update apply", []string{"0.1.2"}, nil); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("版本号不对：%v", err)
	}
	if f.locked != 2 || f.unlocked != 2 {
		t.Fatalf("两次失败都应当放锁：locked=%d unlocked=%d", f.locked, f.unlocked)
	}
	before := f.hits.Load()
	f.locked++ // 锁被别人占着
	if _, err := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil); v1.AsError(err).Code != v1.CodeConflict || f.hits.Load() != before {
		t.Fatalf("锁被占着时应当直接 conflict、不联网：%v hits=%d→%d", err, before, f.hits.Load())
	}
}

// 审查：StartJob 失败（没受理）时锁由 apply 放、只放一次；受理之后由 job 结束时的 done 回调放。
func TestApplyStartJobFails(t *testing.T) {
	f := newFixture(t, sqliteDB(t))
	f.s.d.StartJob = func(context.Context, *command.Invocation, func(context.Context) (any, error), func()) (any, error) {
		return nil, v1.New(v1.CodeDatabase, "插不进 job")
	}
	if _, err := f.call(t, admin(), "update apply", []string{"0.1.1"}, nil); v1.AsError(err).Code != v1.CodeDatabase {
		t.Fatalf("%v", err)
	}
	if f.locked != 1 || f.unlocked != 1 {
		t.Fatalf("锁应当恰好放一次：locked=%d unlocked=%d", f.locked, f.unlocked)
	}
}
