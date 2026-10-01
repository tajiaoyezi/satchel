package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	for _, d := range []string{dir, filepath.Join(dir, SubscribesDir), filepath.Join(dir, RuleTemplatesDir), filepath.Join(dir, PublicDir), filepath.Join(dir, LogsDir),
		filepath.Join(dir, BackupsDir), filepath.Join(dir, RecoveryCodesDir)} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("%s 应当是目录", d)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s 权限应当是 0700，得到 %o", d, perm)
		}
	}
	if len(DataSubDirs) != 6 {
		t.Fatalf("数据目录只有六个子目录，得到 %v", DataSubDirs)
	}
}

func TestLoadConfigPostgresWithoutDataDir(t *testing.T) {
	clearEnv(t)
	dir := filepath.Join(t.TempDir(), "never-created")
	t.Setenv("SATCHEL_DATABASE_DRIVER", "postgres")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("postgres 模式不该要求数据目录存在：%v", err)
	}
	if cfg.Driver != DriverPostgres {
		t.Fatalf("驱动应当是 postgres，得到 %s", cfg.Driver)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("LoadConfig 不该创建数据目录")
	}
}

// SaveConfig 原子写入：0600、读得回、不留临时文件。
func TestSaveConfigAtomic(t *testing.T) {
	dir := t.TempDir()
	want := Config{Driver: DriverPostgres, Host: "db.example", Port: 5432, Name: "satchel", User: "u", Password: "p", SSLMode: "require"}
	if err := SaveConfig(dir, want); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, ConfigFile))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("权限应当 0600，得到 %o", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("不应当留下临时文件：%v", entries)
	}
	got, err := LoadConfig(dir)
	if err != nil || got.Driver != DriverPostgres || got.Host != "db.example" || got.Password != "p" {
		t.Fatalf("读回：%+v %v", got, err)
	}
	if err := SaveConfig(filepath.Join(dir, "missing"), want); err == nil {
		t.Fatal("目录不存在时应当失败")
	}
	var g *WriteGate
	if g.Suspended() {
		t.Fatal("nil 的开关当作关着")
	}
	g = &WriteGate{}
	g.Suspend("正在升级", "等一会儿")
	if reason, next := g.Why(); !g.Suspended() || reason != "正在升级" || next != "等一会儿" {
		t.Fatal("打开后应当挡着，并带上原因与下一步")
	}
	g.Resume()
	if g.Suspended() {
		t.Fatal("关掉后不挡")
	}
}

// 审查第 2 条：改名之后目录落盘失败时返回 ErrConfigNotSynced（新配置已经生效），不是普通的失败。
func TestSaveConfigDirSyncFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o300) // 能在里面建文件、改名，但打不开目录本身去落盘
	defer os.Chmod(dir, 0o700)
	err := SaveConfig(dir, Config{Driver: DriverPostgres, Host: "h"})
	if !errors.Is(err, ErrConfigNotSynced) {
		t.Fatalf("应当是 ErrConfigNotSynced：%v", err)
	}
	os.Chmod(dir, 0o700)
	if got, _ := LoadConfig(dir); got.Driver != DriverPostgres {
		t.Fatal("新配置应当已经生效")
	}
}

// 审查：开关打开之前进门的请求，Drain 等它们走完；开关开着时 Enter 不放行。
func TestWriteGateDrain(t *testing.T) {
	g := &WriteGate{}
	if !g.Enter() {
		t.Fatal("开关关着时应当能进门")
	}
	g.Suspend("正在升级", "")
	if g.Enter() {
		t.Fatal("开关开着时不应当再放行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := g.Drain(ctx); err == nil {
		t.Fatal("还有请求没走完，Drain 应当等到超时")
	}
	g.Leave()
	if err := g.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var nilGate *WriteGate
	if !nilGate.Enter() {
		t.Fatal("nil 当作关着")
	}
	nilGate.Leave()
}
