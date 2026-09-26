package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/base/db"
)

func logsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := db.EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// master-logs「日志同时进文件」：stderr 与文件内容相同，文件 0600。
func TestNewWritesBoth(t *testing.T) {
	dir := logsDir(t)
	var stderr bytes.Buffer
	logger, closer, err := newWith(slog.LevelInfo, dir, &stderr, maxSizeMB)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("主控已启动", "listen", "127.0.0.1:12889")
	logger.Debug("看不到")
	closer.Close()
	path := filepath.Join(dir, db.LogsDir, db.LogFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stderr.String() || !strings.Contains(string(data), "msg=主控已启动") || strings.Contains(string(data), "看不到") {
		t.Fatalf("文件与 stderr 应当相同且只有 info 那一条：\n文件 %q\nstderr %q", data, stderr.String())
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("日志文件权限应当是 0600，得到 %o", perm)
	}
}

// groupValuer 是一个 LogValue 返回属性组的值。
type groupValuer struct{}

func (groupValuer) LogValue() slog.Value { return slog.GroupValue(slog.String("inner", "leaked")) }

// master-logs「属性值打码」。
func TestRedact(t *testing.T) {
	dir := logsDir(t)
	var stderr bytes.Buffer
	logger, closer, err := newWith(slog.LevelInfo, dir, &stderr, maxSizeMB)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	logger.Info("x", "password", "abc", "turnstile_secret", "xyz", "bearer_token", "sat_1", "token_id", 7,
		slog.Group("req", "Password", "p2", "path", "/a"))
	// 组名本身敏感：组里的属性都打码（审查第 2 条）。
	logger.Info("y", slog.Group("password", slog.String("plain", "hunter2")))
	logger.WithGroup("turnstile_secret").Info("z", "v", "xyz2")
	logger.Info("w", "session_token", groupValuer{})
	out := stderr.String()
	for _, want := range []string{"password.plain=***", "turnstile_secret.v=***", "session_token.inner=***"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q：%s", want, out)
		}
	}
	for _, leak := range []string{"abc", "xyz", "sat_1", "p2", "hunter2", "leaked"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q 应当被打码：%s", leak, out)
		}
	}
	for _, want := range []string{"password=***", "turnstile_secret=***", "bearer_token=***", "token_id=7", "req.Password=***", "req.path=/a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q：%s", want, out)
		}
	}
}

func TestNewFailsWhenUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root 或 Windows 下目录权限拦不住")
	}
	dir := logsDir(t)
	logs := filepath.Join(dir, db.LogsDir)
	if err := os.Chmod(logs, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(logs, 0o700)
	if _, _, err := New(slog.LevelInfo, dir); err == nil || !strings.Contains(err.Error(), db.LogFile) {
		t.Fatalf("日志目录只读时应当报错并点名文件，得到 %v", err)
	}
}

// master-logs「轮转后最多四个文件」：测试里把上限调到 1 MB。
func TestRotation(t *testing.T) {
	dir := logsDir(t)
	logger, closer, err := newWith(slog.LevelInfo, dir, &bytes.Buffer{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 1000)
	for i := 0; i < 6000; i++ { // 约 6 MB
		logger.Info(line, "i", i)
	}
	closer.Close()
	// lumberjack 在后台 goroutine 里删多出来的旧文件，等它删完。
	var files []File
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		files, err = Files(filepath.Join(dir, db.LogsDir))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) <= 1+maxBackups || time.Now().After(deadline) {
			break
		}
	}
	if len(files) != 1+maxBackups || !files[0].Active || files[0].Name != db.LogFile {
		t.Fatalf("应当是一个当前文件加 %d 个旧文件，得到 %+v", maxBackups, files)
	}
	for _, f := range files {
		if f.Size > 1<<20 {
			t.Fatalf("%s 超过上限：%d", f.Name, f.Size)
		}
	}
}
