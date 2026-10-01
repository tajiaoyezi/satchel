package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/selfupdate"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

const upgradeJob = "job-00000000000000aa"

// upgraded 是一个「旧版本已经把二进制换成新版本、写好升级标记、正要 exec」的数据目录。
type upgraded struct {
	dataDir, target, backup string
	logs                    *syncBuffer
	mu                      sync.Mutex
	execs                   []string
}

// prepareUpgraded 起一个主控：建 admin（开两步验证）、插一个 running 的升级 job、生成升级前备份，之后再加一个 bob
// （回退换回备份后 bob 就没了）；停下；目标目录里放新旧两个「二进制」，写 switching 的升级标记。exec 换成只记录路径。
func prepareUpgraded(t *testing.T) *upgraded { return prepareUpgradedWith(t, nil) }

// prepareUpgradedWith 同 prepareUpgraded；configure 在建好 admin、开两步验证之前对这个主控做额外的设置。
func prepareUpgradedWith(t *testing.T, configure func(b *booted)) *upgraded {
	t.Helper()
	u := &upgraded{dataDir: shortTempDir(t), logs: &syncBuffer{}}
	b := bootFrom(t, u.dataDir)
	setupAdmin(t, b.tcpURL)
	if configure != nil {
		configure(b)
	}
	enableTOTP(t, b)
	ctx := context.Background()
	repo := corejobs.New(b.db, store.New(b.db, schema.Default()))
	job, err := repo.Insert(ctx, upgradeJob, "update apply", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRunning(ctx, job.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.app.backups.Begin("升级"); err != nil {
		t.Fatal(err)
	}
	info, err := b.app.backups.CreateBeforeUpgrade(ctx)
	b.app.backups.End()
	if err != nil {
		t.Fatal(err)
	}
	u.backup = info.Name
	seedUser(t, b.db, "bob", "bobpass12")
	b.cancel()
	b.waitStopped()

	u.target = filepath.Join(t.TempDir(), "satchel")
	os.WriteFile(u.target, []byte("new"), 0o755)
	os.WriteFile(selfupdate.PreviousPath(u.target), []byte("old"), 0o755)
	u.writeMarker(t, func(*selfupdate.Marker) {})
	prev := execBinary
	execBinary = func(path string, _ []string) error {
		u.mu.Lock()
		u.execs = append(u.execs, path)
		u.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { execBinary = prev })
	return u
}

func (u *upgraded) writeMarker(t *testing.T, edit func(*selfupdate.Marker)) {
	t.Helper()
	m := &selfupdate.Marker{JobID: upgradeJob, FromVersion: "0.1.0", ToVersion: "0.1.1", Channel: "stable", Backup: u.backup,
		Target: u.target, Previous: selfupdate.PreviousPath(u.target), Actor: "root", RequestedAt: time.Now().UTC(), Phase: selfupdate.PhaseSwitching}
	edit(m)
	if err := selfupdate.WriteMarker(u.dataDir, m); err != nil {
		t.Fatal(err)
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// serve 以 version 运行 serveWith（像 serve 那样），返回停止它的函数与结果通道。
func (u *upgraded) serve(t *testing.T, version string, dbCfg db.Config) (stop func(), done chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	finished := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(u.logs, nil))
	go func() {
		_, err := serveWith(ctx, u.dataDir, dbCfg, db.ServeConfig{Listen: "127.0.0.1:0"}, logger, nopCloser{}, version)
		done <- err
		close(finished)
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Error("serve 没有在限时内停止")
		}
	}
	t.Cleanup(stop)
	return stop, done
}

func (u *upgraded) sqlite() db.Config {
	return db.Config{Driver: db.DriverSQLite, Path: filepath.Join(u.dataDir, db.SQLiteFile)}
}

func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("serve 应当自己结束")
	}
	return nil
}

func (u *upgraded) exec() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.execs...)
}

func (u *upgraded) job(t *testing.T) *corejobs.Job {
	t.Helper()
	bdb, err := db.Open(context.Background(), u.sqlite())
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	j, err := corejobs.New(bdb, store.New(bdb, schema.Default())).Get(context.Background(), upgradeJob)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return string(data)
}

// assertRolledBack：目标路径放回了旧二进制、exec 的是它、升级标记是 rolling_back 且原因含 reason、待恢复标记指向升级前备份。
func (u *upgraded) assertRolledBack(t *testing.T, reason string) {
	t.Helper()
	if ex := u.exec(); len(ex) != 1 || ex[0] != u.target || read(t, u.target) != "old" {
		t.Fatalf("应当放回旧二进制并 exec 它：%v %q", ex, read(t, u.target))
	}
	m, _ := selfupdate.ReadMarker(u.dataDir)
	if m == nil || m.Phase != selfupdate.PhaseRollingBack || !strings.Contains(m.Error, reason) {
		t.Fatalf("升级标记应当是 rolling_back 且原因含 %q：%+v", reason, m)
	}
	p, _ := archive.ReadMarker(u.dataDir)
	if p == nil || p.Backup != u.backup || p.Source != archive.SourceUpgradeRollback || p.Phase != archive.PhasePending {
		t.Fatalf("待恢复标记应当指向升级前备份：%+v", p)
	}
}

// master-self-update「健康检查与成功收尾」：新版本启动、healthz 通过后 job 是 done、标记删掉，不 exec。
func TestUpgradeSucceeds(t *testing.T) {
	u := prepareUpgraded(t)
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("健康检查通过后升级标记应当删掉：%s", u.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	j := u.job(t)
	if j.Status != corejobs.StatusDone || !strings.Contains(*j.Output, `"to_version":"0.1.1"`) || !strings.Contains(*j.Output, u.backup) {
		t.Fatalf("job 应当是 done：%+v %s", j, *j.Output)
	}
	if len(u.exec()) != 0 || read(t, u.target) != "new" {
		t.Fatal("成功时不应当 exec、不应当放回旧二进制")
	}
}

// master-self-update「健康检查不过也回退」，接着旧版本启动：换回升级前的库、重发恢复码、job 写 failed、删标记
// （「新版本迁移失败时成对回退」的后半段与这里相同）。
func TestUpgradeHealthFailsThenOldFinishes(t *testing.T) {
	u := prepareUpgraded(t)
	prev := healthTimeout
	healthTimeout = time.Nanosecond
	t.Cleanup(func() { healthTimeout = prev })
	_, done := u.serve(t, "0.1.1", u.sqlite())
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	u.assertRolledBack(t, "健康检查")

	healthTimeout = prev
	u.execs = nil
	stop, _ := u.serve(t, "0.1.0", u.sqlite())
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("旧版本收尾后升级标记应当删掉：%s", u.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	j := u.job(t)
	if j.Status != corejobs.StatusFailed || !strings.Contains(*j.Output, "健康检查") || !strings.Contains(*j.Output, `"to_version":"0.1.1"`) {
		t.Fatalf("job 应当是 failed 并带原因：%+v %s", j, *j.Output)
	}
	bdb, err := db.Open(context.Background(), u.sqlite())
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	var n int
	bdb.QueryRowContext(context.Background(), "SELECT count(*) FROM users WHERE username = 'bob'").Scan(&n)
	var last string
	bdb.QueryRowContext(context.Background(), "SELECT value FROM system_settings WHERE key = 'last_restore'").Scan(&last)
	if n != 0 || !strings.Contains(last, archive.SourceUpgradeRollback) || !strings.Contains(last, u.backup) {
		t.Fatalf("库应当回到升级前备份那一刻（bob 没了），last_restore 记着升级回退：bob=%d %s", n, last)
	}
	if files, _ := filepath.Glob(filepath.Join(u.dataDir, db.RecoveryCodesDir, "recovery-codes-*.txt")); len(files) != 1 {
		t.Fatalf("回退之后应当重发恢复码：%v", files)
	}
	if len(u.exec()) != 0 {
		t.Fatal("旧版本收尾时不应当 exec")
	}
}

// master-self-update「连续崩溃三次就回退」：第 4 次启动时不往下启动，直接回退。
func TestUpgradeTooManyStarts(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Attempts = maxUpgradeStarts })
	_, done := u.serve(t, "0.1.1", u.sqlite())
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	u.assertRolledBack(t, "连续")
}

// 新版本启动顺序里某一步失败（这里是开库失败）：不以非 0 退出，改为回退。
func TestUpgradeStartFails(t *testing.T) {
	u := prepareUpgraded(t)
	bad := db.Config{Driver: db.DriverSQLite, Path: filepath.Join(u.dataDir, "missing", "x.db")}
	_, done := u.serve(t, "0.1.1", bad)
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	u.assertRolledBack(t, "启动失败")
	if m, _ := selfupdate.ReadMarker(u.dataDir); m.Attempts != 1 {
		t.Fatalf("启动次数应当加过一次：%d", m.Attempts)
	}
}

// 升级标记是 switching、而跑的是旧版本：替换或 exec 之前进程就没了。job 写 failed，删标记，照常启动，不动二进制。
func TestUpgradeInterruptedBeforeSwitch(t *testing.T) {
	u := prepareUpgraded(t)
	stop, _ := u.serve(t, "0.1.0", u.sqlite())
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("升级标记应当删掉")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	if j := u.job(t); j.Status != corejobs.StatusFailed || !strings.Contains(*j.Output, "没有完成切换") {
		t.Fatalf("%+v", j)
	}
	if len(u.exec()) != 0 || read(t, u.target) != "new" {
		t.Fatal("不应当动二进制")
	}
	if p, _ := archive.ReadMarker(u.dataDir); p != nil {
		t.Fatal("不应当写待恢复标记")
	}
}

// waitMarkerGone 等升级标记被删掉。
func (u *upgraded) waitMarkerGone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("升级标记应当删掉：%s", u.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 审查：关闭公网访问、回环登记成反代、主控地址是域名时，TCP 上的 healthz 会被门 403；健康检查走 socket，照样成功。
func TestUpgradeSucceedsBehindGates(t *testing.T) {
	u := prepareUpgradedWith(t, func(b *booted) {
		if _, stderr, code := b.human("settings", "master-url", "set", "--url", "https://panel.example.com", "--resource-version", "1"); code != 0 {
			t.Fatalf("master-url set：%s", stderr)
		}
		b.gates("master_local_only=true", loopbackProxy)
	})
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if j := u.job(t); j.Status != corejobs.StatusDone || len(u.exec()) != 0 {
		t.Fatalf("门开着也应当升级成功、不回退：%+v %v", j, u.exec())
	}
}

// 审查：健康检查通过、标记已是 committed 之后崩溃，下次启动只补完收尾：写 done、删标记，不计次、不回退。
func TestUpgradeCommittedRestart(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Phase, m.Attempts = selfupdate.PhaseCommitted, maxUpgradeStarts })
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if j := u.job(t); j.Status != corejobs.StatusDone || len(u.exec()) != 0 || read(t, u.target) != "new" {
		t.Fatalf("应当只补完收尾：%+v %v", j, u.exec())
	}
}

// 审查：升级回退时换回升级前的库失败（这里是备份坏了）：旧版本拒绝启动、不收尾，两份标记都留着；再启动还是拒绝。
func TestUpgradeRollbackRestoreFails(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) {
		m.Phase, m.Error = selfupdate.PhaseRollingBack, "新版本启动失败：x"
	})
	if err := archive.WriteMarker(u.dataDir, &archive.Marker{Backup: u.backup, Source: archive.SourceUpgradeRollback, Actor: "root", Phase: archive.PhasePending}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(archive.Dir(u.dataDir), u.backup), []byte("not a zip"), 0o600)
	for i := 0; i < 2; i++ {
		_, done := u.serve(t, "0.1.0", u.sqlite())
		err := waitDone(t, done)
		if e := v1.AsError(err); err == nil || !strings.Contains(e.Reason, "升级回退时换回升级前的库失败") || !strings.Contains(e.Next, "pending") {
			t.Fatalf("第 %d 次启动应当拒绝：%v", i+1, err)
		}
		if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil || m.Phase != selfupdate.PhaseRollingBack {
			t.Fatalf("升级标记应当留着：%+v", m)
		}
		if p, _ := archive.ReadMarker(u.dataDir); p == nil || p.Phase != archive.PhaseFailed {
			t.Fatalf("待恢复标记应当留着、是 failed：%+v", p)
		}
	}
	if j := u.job(t); j.Status != corejobs.StatusRunning {
		t.Fatalf("job 不应当被写成「已回到旧版本」：%+v", j)
	}
}

// 审查：同一个数据目录上已有主控在跑时，再起一个 serve 不碰升级状态（不会对着正在跑的那个回退）。
func TestUpgradeSecondInstance(t *testing.T) {
	u := prepareUpgraded(t)
	first := bootFrom(t, u.dataDir)
	_ = first
	_, done := u.serve(t, "0.1.1", u.sqlite())
	if err := waitDone(t, done); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("应当 conflict：%v", err)
	}
	if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil || m.Phase != selfupdate.PhaseSwitching || m.Attempts != 0 {
		t.Fatalf("升级标记不应当被动：%+v", m)
	}
	if len(u.exec()) != 0 || read(t, u.target) != "new" {
		t.Fatal("不应当回退")
	}
}

// 审查：健康检查不过、回退本身也失败（.bak 没了）：暂停写入并停下，不 exec；标记是 rolling_back，下次启动接着回退。
func TestUpgradeRollbackFailsStops(t *testing.T) {
	u := prepareUpgraded(t)
	prev := healthTimeout
	healthTimeout = time.Nanosecond
	t.Cleanup(func() { healthTimeout = prev })
	os.Remove(selfupdate.PreviousPath(u.target))
	_, done := u.serve(t, "0.1.1", u.sqlite())
	if err := waitDone(t, done); err == nil || !strings.Contains(v1.AsError(err).Reason, "回退也失败") {
		t.Fatalf("回退失败时应当以错误停下（非 0 退出，服务管理器会拉起）：%v", err)
	}
	if len(u.exec()) != 0 {
		t.Fatal("回退失败时不应当 exec")
	}
	if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil || m.Phase != selfupdate.PhaseRollingBack {
		t.Fatalf("升级标记应当是 rolling_back：%+v", m)
	}
	if !strings.Contains(u.logs.String(), "暂停写入并停止") {
		t.Fatalf("应当暂停写入并停止：%s", u.logs.String())
	}
}

// 审查：数据目录的锁被另一个主控拿着时，serve 在碰任何升级状态之前就 conflict。
func TestUpgradeLockedByAnother(t *testing.T) {
	u := prepareUpgraded(t)
	held, err := acquireServeLock(u.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, done := u.serve(t, "0.1.1", u.sqlite())
	if err := waitDone(t, done); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("应当 conflict：%v", err)
	}
	if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil || m.Phase != selfupdate.PhaseSwitching || m.Attempts != 0 {
		t.Fatalf("升级标记不应当被动：%+v", m)
	}
}

// 审查：新版本启动途中收到停止信号（ctx 取消）不算启动失败：不回退，这次启动也不计次。
func TestUpgradeStopDuringStart(t *testing.T) {
	u := prepareUpgraded(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := serveWith(ctx, u.dataDir, u.sqlite(), db.ServeConfig{Listen: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(u.logs, nil)), nopCloser{}, "0.1.1")
	t.Logf("serveWith：%v", err)
	if len(u.exec()) != 0 || read(t, u.target) != "new" {
		t.Fatal("停止不是失败：不应当回退")
	}
	if p, _ := archive.ReadMarker(u.dataDir); p != nil {
		t.Fatal("不应当写待恢复标记")
	}
	if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil || m.Phase != selfupdate.PhaseSwitching || m.Attempts != 0 {
		t.Fatalf("标记应当还是 switching、这次不计次：%+v", m)
	}
}

// 审查：标记是 committed（健康检查已通过）而跑的是旧版本（有人手工换回了旧二进制）：升级是成功的，只补完收尾，不换库。
func TestUpgradeCommittedOldVersion(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Phase = selfupdate.PhaseCommitted })
	stop, _ := u.serve(t, "0.1.0", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if j := u.job(t); j.Status != corejobs.StatusDone {
		t.Fatalf("job 应当是 done：%+v", j)
	}
	if p, _ := archive.ReadMarker(u.dataDir); p != nil || len(u.exec()) != 0 {
		t.Fatal("不应当换库、不应当 exec")
	}
	bdb, err := db.Open(context.Background(), u.sqlite())
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	var n int
	bdb.QueryRowContext(context.Background(), "SELECT count(*) FROM users WHERE username = 'bob'").Scan(&n)
	if n != 1 {
		t.Fatal("库不应当换回升级前的备份")
	}
}

// 审查：committed 之后 job 行已经不在（超过保留期被清理）也当作已写好：删标记、照常启动，不会每次启动都失败。
func TestUpgradeCommittedJobGone(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Phase, m.JobID = selfupdate.PhaseCommitted, "job-00000000000000ff" })
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	u.waitMarkerGone(t)
	stop()
}

// 审查：健康检查先经 socket 要 200，再向 TCP 监听要一个低于 500 的回应（门的 403 也算通）；TCP 上 5xx 或连不上算不通。
func TestWaitHealthyChecksTCP(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	socketSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})}
	go socketSrv.Serve(ln)
	defer socketSrv.Close()
	for _, c := range []struct {
		status int
		ok     bool
	}{{http.StatusOK, true}, {http.StatusForbidden, true}, {http.StatusInternalServerError, false}} {
		tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(c.status) }))
		err := waitHealthy(context.Background(), sock, tcp.Listener.Addr(), 500*time.Millisecond)
		tcp.Close()
		if (err == nil) != c.ok {
			t.Errorf("TCP 回 %d 时健康检查应当 %v：%v", c.status, c.ok, err)
		}
	}
}

// 审查：健康检查通过之后收尾暂时失败，在进程内重试直到做完（不用等下次启动）。
func TestUpgradeCommitRetries(t *testing.T) {
	u := prepareUpgraded(t)
	prevRetry, prevCommit := commitRetry, commitUpgrade
	commitRetry = 10 * time.Millisecond
	var calls atomic.Int32
	commitUpgrade = func(up *upgradeStart, ctx context.Context, a *app) error {
		if calls.Add(1) <= 2 {
			return errors.New("库暂时写不进去")
		}
		return prevCommit(up, ctx, a)
	}
	t.Cleanup(func() { commitRetry, commitUpgrade = prevRetry, prevCommit })
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if calls.Load() != 3 || u.job(t).Status != corejobs.StatusDone {
		t.Fatalf("应当重试到做完：calls=%d", calls.Load())
	}
}

// 审查：旧进程在「放回旧二进制也失败」时已经把 job 写成 failed、留着升级标记；重启后新版本通过健康检查，按标记改写成 done。
func TestUpgradeFailedJobThenSucceeds(t *testing.T) {
	u := prepareUpgraded(t)
	bdb, err := db.Open(context.Background(), u.sqlite())
	if err != nil {
		t.Fatal(err)
	}
	repo := corejobs.New(bdb, store.New(bdb, schema.Default()))
	j, _ := repo.Get(context.Background(), upgradeJob)
	if err := repo.Finish(context.Background(), j.ID, corejobs.StatusRunning, corejobs.StatusFailed, 1, `{"code":"internal","reason":"放回旧二进制也失败"}`, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	bdb.Close()
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if got := u.job(t); got.Status != corejobs.StatusDone || !strings.Contains(*got.Output, "0.1.1") {
		t.Fatalf("升级其实成功了，job 应当改写成 done：%+v", got)
	}
}

// 审查：健康检查通过之前收到停止信号（不论停在哪一步），这次启动不计次；健康检查已通过或已开始回退时不动。
func TestUncountIfStopped(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Attempts = 1 })
	m, _ := selfupdate.ReadMarker(u.dataDir)
	up := &upgradeStart{dataDir: u.dataDir, logger: slog.New(slog.NewTextHandler(u.logs, nil)), m: m, isNew: true}
	up.uncountIfStopped(context.Background()) // 没有停止：不动
	if m, _ := selfupdate.ReadMarker(u.dataDir); m.Attempts != 1 {
		t.Fatalf("没停止时不应当减：%d", m.Attempts)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	up.uncountIfStopped(stopped)
	if m, _ := selfupdate.ReadMarker(u.dataDir); m.Attempts != 0 {
		t.Fatalf("停止时应当减掉这次：%d", m.Attempts)
	}
	up.uncountIfStopped(stopped)
	if m, _ := selfupdate.ReadMarker(u.dataDir); m.Attempts != 0 {
		t.Fatalf("不应当减成负数：%d", m.Attempts)
	}
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Phase, m.Attempts = selfupdate.PhaseCommitted, 1 })
	up.uncountIfStopped(stopped)
	if m, _ := selfupdate.ReadMarker(u.dataDir); m.Attempts != 1 {
		t.Fatal("健康检查已通过时不动")
	}
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Phase, m.Attempts = selfupdate.PhaseRollingBack, 1 })
	up.uncountIfStopped(stopped)
	if m, _ := selfupdate.ReadMarker(u.dataDir); m.Attempts != 1 || m.Phase != selfupdate.PhaseRollingBack {
		t.Fatal("已经开始回退时不动")
	}
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Attempts = 3 })
	up.uncountIfStopped(stopped)
	if m, _ := selfupdate.ReadMarker(u.dataDir); m.Attempts != 2 {
		t.Fatalf("一次只减这一次启动：%d", m.Attempts)
	}
}

// 审查：开始监听之后、健康检查收尾之前收到停止信号，这次启动也不计次；而且要等健康检查的 goroutine 停下再判断，
// 不能和它同时改写标记（否则可能把刚落盘的 committed 盖回 switching）。
func TestUncountAfterServe(t *testing.T) {
	u := prepareUpgraded(t)
	prev := commitUpgrade
	reached := make(chan struct{})
	duringCommit := make(chan *selfupdate.Marker, 1)
	commitUpgrade = func(_ *upgradeStart, ctx context.Context, _ *app) error {
		close(reached) // 健康检查已过，收尾卡住直到停止
		<-ctx.Done()
		// 收尾还没结束（还在落盘之类）时，没有别人改过标记：serve 必须等这个 goroutine 结束才去判断计次。
		time.Sleep(300 * time.Millisecond)
		m, _ := selfupdate.ReadMarker(u.dataDir)
		duringCommit <- m
		return ctx.Err()
	}
	t.Cleanup(func() { commitUpgrade = prev })
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatalf("健康检查应当通过：%s", u.logs.String())
	}
	stop()
	if m := <-duringCommit; m == nil || m.Phase != selfupdate.PhaseSwitching || m.Attempts != 1 {
		t.Fatalf("收尾还在跑时标记不应当被改写：%+v", m)
	}
	if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil || m.Phase != selfupdate.PhaseSwitching || m.Attempts != 0 {
		t.Fatalf("停止不算这次启动：%+v", m)
	}
}

// 审查：committed 的启动收尾也要能把旧进程写成 failed 的 job 改写成 done（与健康检查之后的收尾一致）。
func TestUpgradeCommittedFailedJob(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Phase = selfupdate.PhaseCommitted })
	bdb, err := db.Open(context.Background(), u.sqlite())
	if err != nil {
		t.Fatal(err)
	}
	repo := corejobs.New(bdb, store.New(bdb, schema.Default()))
	j, _ := repo.Get(context.Background(), upgradeJob)
	if err := repo.Finish(context.Background(), j.ID, corejobs.StatusRunning, corejobs.StatusFailed, 1, `{"code":"internal","reason":"放回旧二进制也失败"}`, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	bdb.Close()
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if got := u.job(t); got.Status != corejobs.StatusDone {
		t.Fatalf("应当改写成 done：%+v", got)
	}
}

// 审查：有升级标记时，结构不一致的错误不能保留原来「删掉库重新迁移」的提示；别的错误在原提示前面加上升级的提示。
func TestUpgradeHintReplacesSchemaAdvice(t *testing.T) {
	up := &upgradeStart{m: &selfupdate.Marker{FromVersion: "0.1.0", ToVersion: "0.1.1", Phase: selfupdate.PhaseCommitted, Backup: "before-upgrade-x.zip"}, committed: true}
	e := v1.AsError(up.withUpgradeHint(db.MismatchError([]string{"users 多了列 x"})))
	if e.Code != v1.CodeSchemaMismatch || strings.Contains(e.Next, "删掉库") || !strings.Contains(e.Next, "不要删库") || len(e.State) == 0 {
		t.Fatalf("结构不一致：%+v", e)
	}
	e = v1.AsError(up.withUpgradeHint(v1.New(v1.CodeDatabase, "连不上").WithNext("检查 PostgreSQL")))
	if !strings.Contains(e.Next, "不要删库") || !strings.HasSuffix(e.Next, "检查 PostgreSQL") {
		t.Fatalf("别的错误保留原提示：%+v", e)
	}
	if up.withUpgradeHint(nil) != nil {
		t.Fatal("nil 原样返回")
	}
}

// 审查：committed 而跑的是第三个版本（健康检查通过之后有人换上了别的版本）：升级是成功的，照样写 done、删标记。
func TestUpgradeCommittedOtherVersion(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Phase = selfupdate.PhaseCommitted })
	stop, _ := u.serve(t, "0.2.0", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if j := u.job(t); j.Status != corejobs.StatusDone {
		t.Fatalf("job 应当是 done：%+v", j)
	}
}

// 审查：有升级标记、跑的是旧版本时启动失败（例如库结构比它新），错误提示不要删库，而不是照搬结构不一致的「删库重新迁移」。
func TestUpgradeHintOnOldStartFailure(t *testing.T) {
	u := prepareUpgraded(t)
	bad := db.Config{Driver: db.DriverSQLite, Path: filepath.Join(u.dataDir, "missing", "x.db")}
	_, done := u.serve(t, "0.1.0", bad)
	err := waitDone(t, done)
	if e := v1.AsError(err); err == nil || !strings.Contains(e.Next, "不要删库") || !strings.Contains(e.Next, u.backup) {
		t.Fatalf("应当提示不要删库并指出升级前的备份：%v / %s", err, e.Next)
	}
	if m, _ := selfupdate.ReadMarker(u.dataDir); m == nil {
		t.Fatal("升级标记应当留着")
	}
}

// 审查：serve 开始之前就要求停止（例如健康检查一开始就失败、回退也失败），serve 一开始就停，不会一直跑下去。
func TestRequestStopBeforeServe(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	dataDir := shortTempDir(t)
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(dataDir, bdb, slog.New(slog.NewTextHandler(io.Discard, nil)), db.ServeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tcp, unix, err := a.listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.failStop(errors.New("回退也失败"))
	done := make(chan error, 1)
	go func() { done <- a.serve(context.Background(), tcp, unix) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve 开始之前的停止请求不应当丢")
	}
	if a.stopError() == nil {
		t.Fatal("应当带着停止的原因")
	}
}

// 审查：TCP 存活检查的访问地址：监听全部地址（0.0.0.0、[::]）时用 127.0.0.1，具体地址原样。
func TestLoopbackOf(t *testing.T) {
	for _, c := range []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.IPv4zero, Port: 12889}, "127.0.0.1:12889"},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 12889}, "127.0.0.1:12889"},
		{&net.TCPAddr{Port: 12889}, "127.0.0.1:12889"},
		{&net.TCPAddr{IP: net.ParseIP("192.168.1.10"), Port: 12889}, "192.168.1.10:12889"},
		{&net.TCPAddr{IP: net.IPv6loopback, Port: 12889}, "[::1]:12889"},
	} {
		if got := loopbackOf(c.addr); got != c.want {
			t.Errorf("%v → %s，应当 %s", c.addr, got, c.want)
		}
	}
}

// README「自动回退不了的情况」的手工步骤真的走得通：放回旧二进制、写最少字段的待恢复标记、把升级标记改成 rolling_back，
// 用旧版本启动——换回升级前的库、重发恢复码、job 写 failed（带手写的原因）、两个标记都删掉。
func TestReadmeManualRollback(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Attempts = 2 }) // 新版本跑过、可能已经动了库
	os.WriteFile(u.target, []byte("old"), 0o755)                    // 步骤 2：cp <previous> <target>
	if err := os.WriteFile(filepath.Join(u.dataDir, db.RestorePendingFile),
		[]byte(`{"backup": "`+u.backup+`", "source": "upgrade_rollback", "phase": "pending"}`), 0o600); err != nil { // 步骤 3
		t.Fatal(err)
	}
	m, _ := selfupdate.ReadMarker(u.dataDir) // 步骤 4
	m.Phase, m.Error = selfupdate.PhaseRollingBack, "新版本启动即崩溃，手工回退"
	if err := selfupdate.WriteMarker(u.dataDir, m); err != nil {
		t.Fatal(err)
	}
	stop, _ := u.serve(t, "0.1.0", u.sqlite()) // 步骤 5
	u.waitMarkerGone(t)
	stop()
	if j := u.job(t); j.Status != corejobs.StatusFailed || !strings.Contains(*j.Output, "手工回退") {
		t.Fatalf("job 应当是 failed 并带手写的原因：%+v", j)
	}
	if p, _ := archive.ReadMarker(u.dataDir); p != nil {
		t.Fatal("待恢复标记应当删掉")
	}
	bdb, err := db.Open(context.Background(), u.sqlite())
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	var n int
	bdb.QueryRowContext(context.Background(), "SELECT count(*) FROM users WHERE username = 'bob'").Scan(&n)
	if n != 0 {
		t.Fatal("库应当换回升级前的备份")
	}
	if files, _ := filepath.Glob(filepath.Join(u.dataDir, db.RecoveryCodesDir, "recovery-codes-*.txt")); len(files) != 1 {
		t.Fatalf("应当重发恢复码：%v", files)
	}
	if len(u.exec()) != 0 {
		t.Fatal("旧版本收尾时不应当 exec")
	}
}

// 审查：要原地重启（自升级、回退）的同时收到了停止信号，就不 exec：直接退出，目标路径上已是该跑的二进制，下次启动按标记处理。
func TestNoExecWhenStopping(t *testing.T) {
	u := prepareUpgraded(t)
	prev := commitUpgrade
	ctx, cancel := context.WithCancel(context.Background())
	commitUpgrade = func(_ *upgradeStart, _ context.Context, a *app) error {
		a.requestExec(u.target) // 例如健康检查不过、回退之后要 exec 旧二进制
		cancel()                // 同时收到了停止信号
		return nil
	}
	t.Cleanup(func() { commitUpgrade = prev; cancel() })
	done := make(chan error, 1)
	go func() {
		_, err := serveWith(ctx, u.dataDir, u.sqlite(), db.ServeConfig{Listen: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(u.logs, nil)), nopCloser{}, "0.1.1")
		done <- err
	}()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	if len(u.exec()) != 0 || !strings.Contains(u.logs.String(), "不再原地重启") {
		t.Fatalf("收到停止信号时不应当 exec：%v", u.exec())
	}
}

// README「升级成功之后又换回了旧版本」：标记是 committed 时，照「自动回退不了」的步骤 2–5 做（步骤 4 把 phase 改成 rolling_back），
// 旧版本换回升级前的库、删掉两个标记。
func TestReadmeManualRollbackFromCommitted(t *testing.T) {
	u := prepareUpgraded(t)
	os.WriteFile(u.target, []byte("old"), 0o755)
	os.WriteFile(filepath.Join(u.dataDir, db.RestorePendingFile),
		[]byte(`{"backup": "`+u.backup+`", "source": "upgrade_rollback", "phase": "pending"}`), 0o600)
	u.writeMarker(t, func(m *selfupdate.Marker) {
		m.Phase, m.Error = selfupdate.PhaseRollingBack, "升级之后手工回到旧版本"
	})
	stop, _ := u.serve(t, "0.1.0", u.sqlite())
	u.waitMarkerGone(t)
	stop()
	if p, _ := archive.ReadMarker(u.dataDir); p != nil {
		t.Fatal("待恢复标记应当删掉")
	}
	if n := u.users(t, "bob"); n != 0 {
		t.Fatal("库应当换回升级前的备份")
	}
}

// README「升级成功之后又换回了旧版本」：升级标记已经删了时，只写待恢复标记（备份名在 backups/ 里按 before-upgrade- 前缀找）。
func TestReadmeManualRollbackMarkerGone(t *testing.T) {
	u := prepareUpgraded(t)
	selfupdate.RemoveMarker(u.dataDir)
	matches, _ := filepath.Glob(filepath.Join(archive.Dir(u.dataDir), archive.PrefixBeforeUpgrade+"*.zip"))
	if len(matches) != 1 || filepath.Base(matches[0]) != u.backup {
		t.Fatalf("按前缀应当找得到升级前的备份：%v", matches)
	}
	os.WriteFile(filepath.Join(u.dataDir, db.RestorePendingFile),
		[]byte(`{"backup": "`+filepath.Base(matches[0])+`", "source": "upgrade_rollback", "phase": "pending"}`), 0o600)
	stop, _ := u.serve(t, "0.1.0", u.sqlite())
	deadline := time.Now().Add(15 * time.Second)
	for {
		if p, _ := archive.ReadMarker(u.dataDir); p == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("待恢复标记应当被收尾删掉：%s", u.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	if n := u.users(t, "bob"); n != 0 {
		t.Fatal("库应当换回升级前的备份")
	}
}

// users 数库里叫 name 的账号。
func (u *upgraded) users(t *testing.T, name string) int {
	t.Helper()
	bdb, err := db.Open(context.Background(), u.sqlite())
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	var n int
	bdb.QueryRowContext(context.Background(), "SELECT count(*) FROM users WHERE username = ?", name).Scan(&n)
	return n
}

// 归档前核对：刚升级上来的新版本在健康检查通过、标记改成 committed 之前暂停写入（这期间收下的写入会被回退丢掉），之后放开。
func TestNewVersionWritesSuspendedUntilCommitted(t *testing.T) {
	u := prepareUpgraded(t)
	prev := commitUpgrade
	type seen struct {
		before, after bool
		writeErr      error
	}
	got := make(chan seen, 1)
	commitUpgrade = func(up *upgradeStart, ctx context.Context, a *app) error {
		var s seen
		s.before = a.writeGate.Suspended()
		_, s.writeErr = a.runner.Run(v1.WithIdentity(ctx, v1.LocalAdmin("root")),
			&command.Invocation{Path: []string{"settings", "set"}, Flags: map[string]any{"set": map[string]any{"branding_site_title": "x"}, "resource-version": 1}})
		err := prev(up, ctx, a)
		s.after = a.writeGate.Suspended()
		got <- s
		return err
	}
	t.Cleanup(func() { commitUpgrade = prev })
	stop, _ := u.serve(t, "0.1.1", u.sqlite())
	var s seen
	select {
	case s = <-got:
	case <-time.After(15 * time.Second):
		t.Fatalf("健康检查应当通过：%s", u.logs.String())
	}
	u.waitMarkerGone(t)
	stop()
	if !s.before || v1.AsError(s.writeErr).Code != v1.CodeUnavailable || !strings.Contains(v1.AsError(s.writeErr).Reason, "健康检查") {
		t.Fatalf("健康检查通过之前应当暂停写入：suspended=%v err=%v", s.before, s.writeErr)
	}
	if s.after {
		t.Fatal("committed 之后应当放开写入")
	}
}

// 归档前核对：回退时，别的待恢复标记（例如新版本启动时库损坏、自动恢复留下的 restored 标记）要覆盖成这次回退的，
// 否则旧版本看不到 pending、不会换回升级前的库。
func TestRollbackOverwritesOtherRestoreMarker(t *testing.T) {
	u := prepareUpgraded(t)
	u.writeMarker(t, func(m *selfupdate.Marker) { m.Attempts = maxUpgradeStarts }) // 这次启动直接回退
	if err := archive.WriteMarker(u.dataDir, &archive.Marker{Backup: u.backup, Source: archive.SourceAuto, Phase: archive.PhaseRestored}); err != nil {
		t.Fatal(err)
	}
	_, done := u.serve(t, "0.1.1", u.sqlite())
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	p, _ := archive.ReadMarker(u.dataDir)
	if p == nil || p.Source != archive.SourceUpgradeRollback || p.Phase != archive.PhasePending || p.Backup != u.backup {
		t.Fatalf("应当覆盖成这次回退的待恢复标记：%+v", p)
	}
	if ex := u.exec(); len(ex) != 1 || ex[0] != u.target {
		t.Fatalf("应当 exec 旧二进制：%v", ex)
	}
}

var _ io.Closer = nopCloser{}
