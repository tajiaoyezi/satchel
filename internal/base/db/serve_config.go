package db

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// serve 自身的配置（master-serve「配置文件与环境变量」）：数据目录下的 config.yaml，只认两个键，环境变量覆盖文件。
// 数据库仍由 database.json 与 SATCHEL_DATABASE_* 决定（m1-07 的在线迁移要改写它，两个文件职责分开）。
const (
	EnvConfigPath = "SATCHEL_CONFIG"
	EnvListen     = "SATCHEL_LISTEN"
	EnvLogLevel   = "SATCHEL_LOG_LEVEL"

	DefaultListen   = "0.0.0.0:12889"
	DefaultLogLevel = "info"
)

// ServeConfig 是 config.yaml 的内容。
type ServeConfig struct {
	Listen   string `yaml:"listen"`
	LogLevel string `yaml:"log_level"`
}

var logLevels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

// SlogLevel 把 log_level 换成 slog 的级别。
func (c ServeConfig) SlogLevel() slog.Level {
	return logLevels[c.LogLevel]
}

// LoadServeConfig 读配置文件（override 非空用它，否则 SATCHEL_CONFIG，再否则数据目录下的 config.yaml；不存在时用默认值），
// 再用 SATCHEL_LISTEN / SATCHEL_LOG_LEVEL 覆盖，最后校验。未知键、非法的 listen 或 log_level 都是 config 错误。
func LoadServeConfig(dataDir, override string) (ServeConfig, error) {
	cfg := ServeConfig{Listen: DefaultListen, LogLevel: DefaultLogLevel}
	path := override
	if path == "" {
		path = os.Getenv(EnvConfigPath)
	}
	if path == "" {
		path = filepath.Join(dataDir, ConfigYAMLFile)
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		var file ServeConfig
		// 空文件 Decode 返回 io.EOF，当作「没有键」。
		if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
			return cfg, v1.Wrap(v1.CodeConfig, "解析 "+path+" 失败："+yamlReason(err), err)
		}
		if file.Listen != "" {
			cfg.Listen = file.Listen
		}
		if file.LogLevel != "" {
			cfg.LogLevel = file.LogLevel
		}
	case errors.Is(err, fs.ErrNotExist) && override == "" && os.Getenv(EnvConfigPath) == "":
		// 默认位置没有文件是正常的。
	case errors.Is(err, fs.ErrNotExist):
		return cfg, v1.Newf(v1.CodeConfig, "配置文件 %s 不存在", path)
	default:
		return cfg, v1.Wrap(v1.CodeConfig, "读取 "+path+" 失败", err)
	}
	if v := os.Getenv(EnvListen); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv(EnvLogLevel); v != "" {
		cfg.LogLevel = v
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c ServeConfig) validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return v1.Newf(v1.CodeConfig, "listen（或环境变量 %s）的值 %q 不是 host:port", EnvListen, c.Listen)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return v1.Newf(v1.CodeConfig, "listen（或环境变量 %s）的端口 %q 不在 1 到 65535 之间", EnvListen, port)
	}
	if host != "" && net.ParseIP(host) == nil {
		return v1.Newf(v1.CodeConfig, "listen（或环境变量 %s）的地址 %q 不是 IP", EnvListen, host)
	}
	if _, ok := logLevels[c.LogLevel]; !ok {
		return v1.Newf(v1.CodeConfig, "log_level（或环境变量 %s）的值 %q 不认识，只认 debug、info、warn、error", EnvLogLevel, c.LogLevel)
	}
	return nil
}

// yamlReason 把 yaml.v3 的错误文案里最有用的一句拿出来：未知键的错误形如
// "yaml: unmarshal errors:\n  line 3: field port not found in type db.ServeConfig"，reason 里点名那个键。
func yamlReason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "field "); i >= 0 {
		if name, _, ok := strings.Cut(msg[i+len("field "):], " "); ok {
			return "不认识的键 " + name + "（只认 listen 与 log_level）"
		}
	}
	return "不是合法的 YAML"
}
