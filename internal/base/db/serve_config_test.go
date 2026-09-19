package db

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func clearServeEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvConfigPath, EnvListen, EnvLogLevel} {
		t.Setenv(name, "")
	}
}

// master-serve「配置文件与环境变量」。
func TestLoadServeConfigDefaults(t *testing.T) {
	clearServeEnv(t)
	cfg, err := LoadServeConfig(t.TempDir(), "")
	if err != nil || cfg.Listen != DefaultListen || cfg.LogLevel != DefaultLogLevel || cfg.SlogLevel() != slog.LevelInfo {
		t.Fatalf("没有文件应当用默认值：%+v %v", cfg, err)
	}
}

func TestLoadServeConfigFileAndEnv(t *testing.T) {
	clearServeEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigYAMLFile), []byte("listen: 127.0.0.1:9000\nlog_level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServeConfig(dir, "")
	if err != nil || cfg.Listen != "127.0.0.1:9000" || cfg.SlogLevel() != slog.LevelDebug {
		t.Fatalf("文件应当生效：%+v %v", cfg, err)
	}
	t.Setenv(EnvListen, "127.0.0.1:9100")
	t.Setenv(EnvLogLevel, "warn")
	cfg, err = LoadServeConfig(dir, "")
	if err != nil || cfg.Listen != "127.0.0.1:9100" || cfg.LogLevel != "warn" {
		t.Fatalf("环境变量应当覆盖文件：%+v %v", cfg, err)
	}
	// 显式路径与 SATCHEL_CONFIG。
	other := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(other, []byte("log_level: error\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvLogLevel, "")
	if cfg, err := LoadServeConfig(dir, other); err != nil || cfg.LogLevel != "error" || cfg.Listen != "127.0.0.1:9100" {
		t.Fatalf("显式路径：%+v %v", cfg, err)
	}
	t.Setenv(EnvConfigPath, other)
	if cfg, err := LoadServeConfig(dir, ""); err != nil || cfg.LogLevel != "error" {
		t.Fatalf("SATCHEL_CONFIG：%+v %v", cfg, err)
	}
	t.Setenv(EnvConfigPath, filepath.Join(dir, "missing.yaml"))
	if _, err := LoadServeConfig(dir, ""); err == nil || v1.AsError(err).Code != v1.CodeConfig {
		t.Fatalf("指定的文件不存在应当 config：%v", err)
	}
	// 空文件也行。
	clearServeEnv(t)
	if err := os.WriteFile(filepath.Join(dir, ConfigYAMLFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadServeConfig(dir, ""); err != nil || cfg.Listen != DefaultListen {
		t.Fatalf("空文件应当用默认值：%+v %v", cfg, err)
	}
}

func TestLoadServeConfigRejects(t *testing.T) {
	clearServeEnv(t)
	dir := t.TempDir()
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, ConfigYAMLFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("listen: 0.0.0.0:12889\nport: 8080\n")
	_, err := LoadServeConfig(dir, "")
	if e := v1.AsError(err); e.Code != v1.CodeConfig || !strings.Contains(e.Reason, "port") {
		t.Fatalf("未知键应当 config 并点名：%v", err)
	}
	write(": not yaml [")
	if _, err := LoadServeConfig(dir, ""); err == nil || v1.AsError(err).Code != v1.CodeConfig {
		t.Fatalf("坏 YAML 应当 config：%v", err)
	}
	write("")
	cases := map[string]map[string]string{
		"listen 不是地址":    {EnvListen: "not-an-address"},
		"listen 端口越界":    {EnvListen: "0.0.0.0:99999"},
		"listen 地址不是 IP": {EnvListen: "example.com:80"},
		"log_level 不认识":  {EnvLogLevel: "verbose"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			clearServeEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			_, err := LoadServeConfig(dir, "")
			e := v1.AsError(err)
			if e.Code != v1.CodeConfig {
				t.Fatalf("应当 config：%v", err)
			}
			for k := range env {
				if !strings.Contains(e.Reason, k) {
					t.Fatalf("reason 应当点名 %s：%s", k, e.Reason)
				}
			}
		})
	}
}
