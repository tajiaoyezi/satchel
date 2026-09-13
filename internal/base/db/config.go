package db

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Driver 是数据库驱动名，也是 database.json 里 driver 字段的值。
type Driver string

const (
	DriverSQLite   Driver = "sqlite"
	DriverPostgres Driver = "postgres"
)

const (
	// DefaultDataDir 是数据目录的默认位置。
	DefaultDataDir = "/var/lib/satchel"
	// EnvDataDir 指定数据目录。
	EnvDataDir = "SATCHEL_DATA_DIR"
	// ConfigFile 是数据目录里的数据库配置文件名。
	ConfigFile = "database.json"
	// SQLiteFile 是默认的 SQLite 库文件名。
	SQLiteFile = "satchel.db"
	// MasterKeyFile 是主控通信密钥（第 06 章的 Ed25519 身份）的文件名。
	MasterKeyFile = "master.key"
	// SubscribesDir 是订阅文件目录，RuleTemplatesDir 是规则模板目录；两者都在数据目录下（第 08 章的数据目录布局与备份内容表）。
	SubscribesDir    = "subscribes"
	RuleTemplatesDir = "rule_templates"
)

// DataSubDirs 是数据目录下要随目录一起创建的子目录。
var DataSubDirs = []string{SubscribesDir, RuleTemplatesDir}

// Config 是 database.json 的内容；环境变量 SATCHEL_DATABASE_* 逐项覆盖它。
type Config struct {
	Driver   Driver `json:"driver"`
	Path     string `json:"path,omitempty"` // SQLite 库文件
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Name     string `json:"name,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	SSLMode  string `json:"sslmode,omitempty"`
}

// DataDir 解析数据目录：flag 非空用 flag，否则用环境变量 SATCHEL_DATA_DIR，再否则 /var/lib/satchel。
func DataDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv(EnvDataDir); v != "" {
		return v
	}
	return DefaultDataDir
}

// EnsureDataDir 创建数据目录与它的子目录（DataSubDirs），权限 0700。会写的命令（迁移、serve）两种驱动都调它：
// postgres 模式下库在别处，但主控密钥、订阅文件、规则模板仍在数据目录里。读配置与只读命令不调它。
func EnsureDataDir(dir string) error {
	for _, d := range append([]string{dir}, subDirs(dir)...) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return v1.Wrap(v1.CodeDatabase, "创建数据目录 "+d+" 失败", err)
		}
	}
	return nil
}

func subDirs(dir string) []string {
	out := make([]string, len(DataSubDirs))
	for i, d := range DataSubDirs {
		out[i] = filepath.Join(dir, d)
	}
	return out
}

// LoadConfig 读数据目录里的 database.json，再用环境变量覆盖；没有文件时默认 SQLite。
func LoadConfig(dataDir string) (Config, error) {
	var cfg Config
	path := filepath.Join(dataDir, ConfigFile)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, v1.Wrap(v1.CodeConfig, "解析 "+path+" 失败：不是合法的 JSON", err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return cfg, v1.Wrap(v1.CodeDatabase, "读取 "+path+" 失败", err)
	}
	if err := cfg.applyEnv(); err != nil {
		return cfg, err
	}
	if cfg.Driver == "" {
		cfg.Driver = DriverSQLite
	}
	switch cfg.Driver {
	case DriverSQLite:
		if cfg.Path == "" {
			cfg.Path = filepath.Join(dataDir, SQLiteFile)
		}
	case DriverPostgres:
		if cfg.Host == "" {
			cfg.Host = "localhost"
		}
		if cfg.Port == 0 {
			cfg.Port = 5432
		}
		if cfg.Name == "" {
			cfg.Name = "satchel"
		}
		if cfg.User == "" {
			cfg.User = "satchel"
		}
	default:
		return cfg, v1.Newf(v1.CodeConfig, "不支持的数据库驱动 %q，只认 sqlite 与 postgres", cfg.Driver)
	}
	return cfg, nil
}

// SaveConfig 把配置写到数据目录的 database.json，权限 0600。
func SaveConfig(dataDir string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, ConfigFile)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return v1.Wrap(v1.CodeDatabase, "写入 "+path+" 失败", err)
	}
	return nil
}

func (c *Config) applyEnv() error {
	set := func(env string, dst *string) {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("SATCHEL_DATABASE_DRIVER"); v != "" {
		c.Driver = Driver(v)
	}
	set("SATCHEL_DATABASE_PATH", &c.Path)
	set("SATCHEL_DATABASE_HOST", &c.Host)
	if v := os.Getenv("SATCHEL_DATABASE_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return v1.Newf(v1.CodeConfig, "SATCHEL_DATABASE_PORT=%q 不是端口号", v)
		}
		c.Port = port
	}
	set("SATCHEL_DATABASE_NAME", &c.Name)
	set("SATCHEL_DATABASE_USER", &c.User)
	set("SATCHEL_DATABASE_PASSWORD", &c.Password)
	set("SATCHEL_DATABASE_SSLMODE", &c.SSLMode)
	return nil
}

// DSN 返回驱动的连接串。
func (c Config) DSN() string {
	switch c.Driver {
	case DriverPostgres:
		u := url.URL{
			Scheme: "postgres",
			Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
			Path:   "/" + c.Name,
		}
		if c.User != "" {
			if c.Password != "" {
				u.User = url.UserPassword(c.User, c.Password)
			} else {
				u.User = url.User(c.User)
			}
		}
		if c.SSLMode != "" {
			u.RawQuery = url.Values{"sslmode": {c.SSLMode}}.Encode()
		}
		return u.String()
	default:
		return SQLiteDSN(c.Path)
	}
}
