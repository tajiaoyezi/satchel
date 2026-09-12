package db

import (
	"os"
	"path/filepath"
	"testing"
)

var envVars = []string{
	"SATCHEL_DATABASE_DRIVER", "SATCHEL_DATABASE_PATH", "SATCHEL_DATABASE_HOST", "SATCHEL_DATABASE_PORT",
	"SATCHEL_DATABASE_NAME", "SATCHEL_DATABASE_USER", "SATCHEL_DATABASE_PASSWORD", "SATCHEL_DATABASE_SSLMODE", EnvDataDir,
}

// clearEnv 把本包关心的环境变量清空，免得开发机上的设置漏进测试。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range envVars {
		t.Setenv(name, "")
	}
}

func TestLoadConfigDefaultsToSQLite(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Driver != DriverSQLite {
		t.Fatalf("没有 database.json 时驱动应当是 sqlite，得到 %q", cfg.Driver)
	}
	if want := filepath.Join(dir, SQLiteFile); cfg.Path != want {
		t.Fatalf("库文件应当是 %s，得到 %s", want, cfg.Path)
	}
}

func TestLoadConfigFromFilePostgres(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	want := Config{Driver: DriverPostgres, Host: "db.internal", Port: 5433, Name: "satchel", User: "u", Password: "p", SSLMode: "require"}
	if err := SaveConfig(dir, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("database.json 权限应当是 0600，得到 %o", perm)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != want {
		t.Fatalf("读回的配置不对：\n得到 %+v\n想要 %+v", cfg, want)
	}
	if dsn := cfg.DSN(); dsn != "postgres://u:p@db.internal:5433/satchel?sslmode=require" {
		t.Fatalf("DSN 不对：%s", dsn)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	if err := SaveConfig(dir, Config{Driver: DriverSQLite}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SATCHEL_DATABASE_DRIVER", "postgres")
	t.Setenv("SATCHEL_DATABASE_PORT", "6543")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Driver != DriverPostgres || cfg.Port != 6543 || cfg.Host != "localhost" {
		t.Fatalf("环境变量应当覆盖文件并补默认值，得到 %+v", cfg)
	}
	t.Setenv("SATCHEL_DATABASE_PORT", "abc")
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("端口不是数字应当报错")
	}
	t.Setenv("SATCHEL_DATABASE_PORT", "")
	t.Setenv("SATCHEL_DATABASE_DRIVER", "mysql")
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("不支持的驱动应当报错")
	}
}

func TestDataDir(t *testing.T) {
	clearEnv(t)
	if got := DataDir(""); got != DefaultDataDir {
		t.Fatalf("默认数据目录应当是 %s，得到 %s", DefaultDataDir, got)
	}
	t.Setenv(EnvDataDir, "/tmp/from-env")
	if got := DataDir(""); got != "/tmp/from-env" {
		t.Fatalf("环境变量应当生效，得到 %s", got)
	}
	if got := DataDir("/tmp/from-flag"); got != "/tmp/from-flag" {
		t.Fatalf("flag 应当优先，得到 %s", got)
	}
}

func TestEnsureDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	if err := EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("数据目录权限应当是 0700，得到 %o", perm)
	}
}
