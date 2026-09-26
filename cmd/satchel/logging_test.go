package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
)

// master-logs「默认 logger 同一个级别」：setupLogging 之后，直接调 slog 包函数的日志也按配置的级别进文件。
func TestLoggingDefaultLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	dataDir := t.TempDir()
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	logger, closer, err := setupLogging(db.ServeConfig{LogLevel: "error"}, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	slog.Warn("默认 logger 的 warn")
	slog.Error("默认 logger 的 error")
	logger.Error("serve logger 的 error")
	closer.Close()
	data, err := os.ReadFile(filepath.Join(dataDir, db.LogsDir, db.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if strings.Contains(out, "默认 logger 的 warn") || !strings.Contains(out, "默认 logger 的 error") || !strings.Contains(out, "serve logger 的 error") {
		t.Fatalf("日志文件内容不对：%s", out)
	}
}

// 日志文件打不开时 serve 启动失败（setupLogging 返回错误），不只写 stderr 地跑下去。
func TestLoggingFailsWithoutLogsDir(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, _, err := setupLogging(db.ServeConfig{LogLevel: "info"}, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("日志目录不存在时应当报错")
	}
	if slog.Default() != prev {
		t.Fatal("失败时不应当改默认 logger")
	}
}
