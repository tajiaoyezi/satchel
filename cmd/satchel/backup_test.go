package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/model"
)

// booted 是像 serve 那样在一个数据目录上起的主控（库就在数据目录里）：openApp → listen → serve。stopped 在 serve 返回后关闭。
type booted struct {
	*harness
	stopped chan struct{}
}

func bootFrom(t *testing.T, dataDir string) *booted {
	t.Helper()
	logs := &syncBuffer{}
	a, err := openApp(context.Background(), dataDir, db.Config{Driver: db.DriverSQLite, Path: filepath.Join(dataDir, db.SQLiteFile)},
		db.ServeConfig{}, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	tcp, unix, err := a.listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &booted{harness: &harness{t: t, dataDir: dataDir, db: a.db, app: a, tcpURL: listenAddrOf(tcp), cancel: cancel, logs: logs}, stopped: make(chan struct{})}
	go func() {
		a.serve(ctx, tcp, unix)
		close(b.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-b.stopped:
		case <-time.After(15 * time.Second):
			t.Error("serve 没有在限时内停止")
		}
	})
	return b
}

// waitStopped 等 serve 自己停下（发起恢复之后）。
func (b *booted) waitStopped() {
	b.t.Helper()
	select {
	case <-b.stopped:
	case <-time.After(15 * time.Second):
		b.t.Fatal("发起恢复之后 serve 应当自己停下")
	}
}

func hashCode(c string) string {
	sum := sha256.Sum256([]byte(c))
	return hex.EncodeToString(sum[:])
}

// enableTOTP 直接把 admin 改成开了两步验证、带三枚已知的恢复码（M1 的测试里没有验证器可用，用恢复码作第二因素）。
func enableTOTP(t *testing.T, b *booted) {
	t.Helper()
	codes, _ := json.Marshal([]string{hashCode("aaaa0001"), hashCode("aaaa0002"), hashCode("aaaa0003")})
	if _, err := b.db.NewUpdate().Model((*model.User)(nil)).Set("totp_enabled = ?", true).Set("totp_secret = ?", "JBSWY3DPEHPK3PXP").
		Set("recovery_codes = ?", string(codes)).Where("username = ?", "admin").Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func userCount(t *testing.T, b *booted) int {
	t.Helper()
	n, err := b.db.NewSelect().Model((*model.User)(nil)).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// 备份与恢复的端到端（SQLite；PostgreSQL 的导出与恢复要 pg_dump / psql，在 base/backup 里测）：生成、列出、下载要当场验证、
// 上传比本主控新的被拒、MCP 与普通用户做不了；恢复发起后 serve 停下，「服务管理器」在同一数据目录上重新拉起后数据回到过去，
// 旧恢复码作废、新恢复码可用，last_restore 与审计都有。
func TestBackupRestoreEndToEnd(t *testing.T) {
	dataDir := shortTempDir(t)
	b := bootFrom(t, dataDir)
	base := b.tcpURL
	admin := setupAdmin(t, base)
	enableTOTP(t, b)

	stdout, stderr, exit := b.cli("backup", "create", "--json")
	if exit != 0 || !strings.Contains(stdout, `"done"`) {
		t.Fatalf("backup create 应当跟到 done：%d %s %s", exit, stdout, stderr)
	}
	list, _ := archive.List(dataDir)
	if len(list) != 1 {
		t.Fatalf("应当有一份备份：%v", list)
	}
	name := list[0].Name

	// 下载：不带当场验证是 human_required；带上密码与一枚恢复码后拿到 ZIP 字节。
	if r := send(t, admin.c, "POST", base+"/api/v1/backup/download/"+name, nil, `{}`); r.status != 403 || r.code() != "human_required" {
		t.Fatalf("不带验证的下载：%d %s", r.status, r.body)
	}
	r := send(t, admin.c, "POST", base+"/api/v1/backup/download/"+name, nil, `{"verify-password":"secret12","verify-code":"aaaa0002"}`)
	if r.status != 200 || !strings.HasPrefix(r.body, "PK") {
		t.Fatalf("带验证的下载：%d %.80s", r.status, r.body)
	}
	if text, isErr := b.mcpRun("backup", "download", name); !isErr || !strings.Contains(text, "human_required") {
		t.Fatalf("MCP 上下载应当被拒：%s", text)
	}

	// 上传一份比本主控新的备份：bad_request 并点名那条迁移。
	future := writeFutureBackup(t)
	if _, stderr, exit := b.cli("backup", "upload", "--file", future, "--json"); exit == 0 || !strings.Contains(stderr, "0002_future") {
		t.Fatalf("比本主控新的备份应当被拒：%d %s", exit, stderr)
	}

	// 普通用户做不了备份。
	seedUser(t, b.db, "bob", "bobpass12")
	bob := newBrowser(t)
	bob.login(base, "bob", "bobpass12", "")
	if status, fields, _ := bob.call("POST", base+"/api/v1/backup/create", `{}`, nil); status != 403 {
		t.Fatalf("普通用户 backup create 应当 403：%d %v", status, fields)
	}

	// 恢复：bob 是备份之后才有的。发起后 serve 自己停下。
	if n := userCount(t, b); n != 2 {
		t.Fatalf("恢复前应当有 2 个账号：%d", n)
	}
	stdout, stderr, exit = b.cliPrompt([]string{"secret12", "aaaa0003"}, "backup", "restore", name, "--verify-user", "admin", "--json")
	if exit != 0 || !strings.Contains(stdout, `"restarting": true`) && !strings.Contains(stdout, `"restarting":true`) {
		t.Fatalf("发起恢复：%d %s %s", exit, stdout, stderr)
	}
	b.waitStopped()

	// 「服务管理器」拉起：在同一个数据目录上重新启动，启动时换库并收尾。
	b2 := bootFrom(t, dataDir)
	if n := userCount(t, b2); n != 1 {
		t.Fatalf("恢复后应当只有 admin：%d", n)
	}
	files, _ := filepath.Glob(filepath.Join(dataDir, db.RecoveryCodesDir, "recovery-codes-*.txt"))
	if len(files) != 1 {
		t.Fatalf("应当有一份恢复码文件：%v", files)
	}
	m := regexp.MustCompile(`(?m)^admin: ([0-9a-f]{8})`).FindStringSubmatch(string(mustReadFile(t, files[0])))
	if m == nil {
		t.Fatal("恢复码文件里应当有 admin 的新码")
	}
	old := newBrowser(t)
	if status, _ := old.login(b2.tcpURL, "admin", "secret12", "aaaa0001"); status != 401 {
		t.Fatalf("备份里的旧恢复码应当作废：%d", status)
	}
	fresh := newBrowser(t)
	if status, fields := fresh.login(b2.tcpURL, "admin", "secret12", m[1]); status != 200 {
		t.Fatalf("新恢复码应当能登录：%d %v", status, fields)
	}
	if stdout, _, _ := b2.cli("settings", "show", "--json"); !strings.Contains(stdout, `"last_restore"`) || !strings.Contains(stdout, name) {
		t.Fatalf("settings show 应当看得到 last_restore：%s", stdout)
	}
	if stdout, _, _ := b2.cli("audit", "list", "--command", "backup restore", "--json"); !strings.Contains(stdout, `"actor_kind":"system"`) || !strings.Contains(stdout, name) {
		t.Fatalf("恢复后的库里应当有这次恢复的审计：%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dataDir, db.RestorePendingFile)); !os.IsNotExist(err) {
		t.Fatal("收尾后待恢复标记应当删掉")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFutureBackup(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	raw, _ := json.Marshal(archive.Manifest{Format: archive.Format, Driver: db.DriverSQLite, Migrations: []string{"0002_future"}})
	w, _ := zw.Create(archive.EntryManifest)
	w.Write(raw)
	w, _ = zw.Create(archive.EntrySQLite)
	w.Write([]byte("x"))
	zw.Close()
	path := filepath.Join(t.TempDir(), "future.zip")
	os.WriteFile(path, buf.Bytes(), 0o600)
	return path
}

// master-backup「坏库自动恢复」的端到端：本机有一份备份，库文件写坏之后重启，数据回到备份那一刻。
func TestAutoRestoreEndToEnd(t *testing.T) {
	dataDir := shortTempDir(t)
	b := bootFrom(t, dataDir)
	setupAdmin(t, b.tcpURL)
	if _, stderr, exit := b.cli("backup", "create"); exit != 0 {
		t.Fatalf("backup create：%s", stderr)
	}
	b.cancel()
	<-b.stopped
	f, err := os.OpenFile(filepath.Join(dataDir, db.SQLiteFile), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt(bytes.Repeat([]byte{0xAB}, 100), 0)
	f.Close()
	b2 := bootFrom(t, dataDir)
	if n := userCount(t, b2); n != 1 {
		t.Fatalf("自动恢复后应当有 admin：%d", n)
	}
	if bad, _ := filepath.Glob(filepath.Join(dataDir, db.BackupsDir, "corrupt-*")); len(bad) != 1 {
		t.Fatal("坏库应当留在 backups/corrupt-*/")
	}
}

// master-setup-wizard「空库恢复备份」：新主控经本机 socket setup restore 一份备份，重启后是备份里的账号；已有用户时 conflict。
func TestSetupRestoreEndToEnd(t *testing.T) {
	src := shortTempDir(t)
	a := bootFrom(t, src)
	setupAdmin(t, a.tcpURL)
	if _, stderr, exit := a.cli("backup", "create"); exit != 0 {
		t.Fatalf("backup create：%s", stderr)
	}
	list, _ := archive.List(src)
	file := filepath.Join(src, db.BackupsDir, list[0].Name)
	if _, stderr, exit := a.cli("setup", "restore", "--file", file, "--json"); exit == 0 || !strings.Contains(stderr, "conflict") {
		t.Fatalf("已有用户时应当 conflict：%d %s", exit, stderr)
	}

	dst := shortTempDir(t)
	b := bootFrom(t, dst)
	if stdout, stderr, exit := b.cli("setup", "restore", "--file", file, "--json"); exit != 0 || !strings.Contains(stdout, "restarting") {
		t.Fatalf("空库的 setup restore：%d %s %s", exit, stdout, stderr)
	}
	b.waitStopped()
	b2 := bootFrom(t, dst)
	if stdout, _, _ := b2.cli("setup", "status", "--json"); !strings.Contains(stdout, `"initialized": true`) && !strings.Contains(stdout, `"initialized":true`) {
		t.Fatalf("恢复后应当已初始化：%s", stdout)
	}
	if n := userCount(t, b2); n != 1 {
		t.Fatalf("应当是备份里的 admin：%d", n)
	}
}
