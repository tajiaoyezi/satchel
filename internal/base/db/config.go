package db

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// ConfigYAMLFile 是主控自身的配置文件（listen、log_level），可不存在；SocketFile 是 serve 运行时的 unix socket，本机 CLI 经它连主控。
	ConfigYAMLFile = "config.yaml"
	SocketFile     = "satchel.sock"
	// SubscribesDir 是订阅文件目录，RuleTemplatesDir 是规则模板目录，PublicDir 是 /public/ 对外提供的静态文件目录；
	// LogsDir 是 serve 的日志目录，LogFile 是其中当前在写的日志文件（master-logs）；BackupsDir 是本机备份目录，
	// RecoveryCodesDir 放恢复之后新恢复码的明文，RestorePendingFile 是待恢复标记（master-backup）；
	// 都在数据目录下（第 08 章的数据目录布局与备份内容表；socket、public/、logs/、backups/、recovery-codes/ 不进备份）。
	SubscribesDir      = "subscribes"
	RuleTemplatesDir   = "rule_templates"
	PublicDir          = "public"
	LogsDir            = "logs"
	LogFile            = "satchel.log"
	BackupsDir         = "backups"
	RecoveryCodesDir   = "recovery-codes"
	RestorePendingFile = "restore-pending.json"
)

// DataSubDirs 是数据目录下要随目录一起创建的子目录。
var DataSubDirs = []string{SubscribesDir, RuleTemplatesDir, PublicDir, LogsDir, BackupsDir, RecoveryCodesDir}

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
// postgres 模式下库在别处，但主控密钥、订阅文件、规则模板、静态文件与日志仍在数据目录里。读配置与只读命令不调它。
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

// SaveConfig 把配置写到数据目录的 database.json（0600）：先写同目录的临时文件并落盘，再改名，再把目录落盘。
// 改名是原子的，所以任何时刻读到的要么是旧配置、要么是新配置（在线迁移的提交点就是这次改名，master-db-migration）。
func SaveConfig(dataDir string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, ConfigFile)
	fail := func(err error) error { return v1.Wrap(v1.CodeDatabase, "写入 "+path+" 失败", err) }
	tmp, err := os.CreateTemp(dataDir, "."+ConfigFile+".*")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(data, '\n')); err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fail(err)
	}
	// 改名已经发生：之后目录落盘失败时新配置已经生效（下次启动就读它），不能当作「没写成」。
	d, err := os.Open(dataDir)
	if err == nil {
		err = d.Sync()
		d.Close()
	}
	if err != nil {
		return fmt.Errorf("%w：%v", ErrConfigNotSynced, err)
	}
	return nil
}

// ErrConfigNotSynced 表示 SaveConfig 已经把新配置改名到位（它已经生效），只是目录没能落盘。调用方要把它当作「已经写成」。
var ErrConfigNotSynced = errors.New("database.json 已经改名到位，但数据目录没能落盘")

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

// dbEnvVars 是覆盖 database.json 的环境变量。
var dbEnvVars = []string{"SATCHEL_DATABASE_DRIVER", "SATCHEL_DATABASE_PATH", "SATCHEL_DATABASE_HOST", "SATCHEL_DATABASE_PORT",
	"SATCHEL_DATABASE_NAME", "SATCHEL_DATABASE_USER", "SATCHEL_DATABASE_PASSWORD", "SATCHEL_DATABASE_SSLMODE"}

// EnvOverride 报告数据库配置有没有被 SATCHEL_DATABASE_* 环境变量覆盖（有任何一个非空就算）：覆盖时改 database.json 不生效，
// 在线迁移据此拒绝（master-db-migration「迁移的前提」）。
func EnvOverride() bool {
	for _, k := range dbEnvVars {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}
