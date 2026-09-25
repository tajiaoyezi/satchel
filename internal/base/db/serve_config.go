package db

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
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
	// 两个只认环境变量、不进 config.yaml 的开关（master-access-gates），只在启动时读。
	EnvForcePublicAccess = "SATCHEL_FORCE_PUBLIC_ACCESS"
	EnvAllowedOrigins    = "SATCHEL_ALLOWED_ORIGINS"

	DefaultListen   = "0.0.0.0:12889"
	DefaultLogLevel = "info"
)

// ServeConfig 是 config.yaml 的内容，加两个只认环境变量的开关（yaml:"-"，写进文件是未知键）。
type ServeConfig struct {
	Listen   string `yaml:"listen"`
	LogLevel string `yaml:"log_level"`
	// ForcePublicAccess 是关闭公网访问的自救开关：为真时本进程跳过这一道门，不改设置（master-access-gates）。
	ForcePublicAccess bool `yaml:"-"`
	// AllowedOrigins 是允许跨域调用的来源：归一后的 origin 列表，或单独一个 *；空表示只允许同源。
	AllowedOrigins []string `yaml:"-"`
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
	force, err := parseSwitch(EnvForcePublicAccess, os.Getenv(EnvForcePublicAccess))
	if err != nil {
		return cfg, err
	}
	cfg.ForcePublicAccess = force
	origins, err := parseOrigins(os.Getenv(EnvAllowedOrigins))
	if err != nil {
		return cfg, err
	}
	cfg.AllowedOrigins = origins
	return cfg, nil
}

// parseSwitch 解析开关类环境变量：1 / true / yes / on 为真，0 / false / no / off 与空为假，不分大小写；别的值是 config。
func parseSwitch(name, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "", "0", "false", "no", "off":
		return false, nil
	}
	return false, v1.Newf(v1.CodeConfig, "环境变量 %s 的值 %q 不认识，只认 1 / true / yes / on 与 0 / false / no / off", name, raw)
}

// parseOrigins 解析 SATCHEL_ALLOWED_ORIGINS：逗号分隔的 http / https origin（归一成小写、去掉默认端口、去重），
// 或单独一个 *。缺 scheme、带路径（单个 / 除外）、查询、片段或用户信息，端口不对，* 与别的项混写，都是 config。
func parseOrigins(raw string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		origin := "*"
		if item != "*" {
			o, err := normalizeOrigin(item)
			if err != nil {
				return nil, v1.Newf(v1.CodeConfig, "环境变量 %s 里的 %q 不是 origin：%s", EnvAllowedOrigins, item, err.Error()).
					WithNext("写成 https://dash.example.com 这样的形状，多个用逗号分开；要允许任何来源就只写 *")
			}
			origin = o
		}
		if !seen[origin] {
			seen[origin] = true
			out = append(out, origin)
		}
	}
	if seen["*"] && len(out) > 1 {
		return nil, v1.Newf(v1.CodeConfig, "环境变量 %s 里的 * 只能单独出现，不能与具体的来源混写", EnvAllowedOrigins)
	}
	return out, nil
}

func normalizeOrigin(item string) (string, error) {
	u, err := url.Parse(item)
	if err != nil {
		return "", errors.New("解析不了")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("scheme 必须是 http 或 https")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(item, "#") || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return "", errors.New("origin 只有 scheme、主机与端口，不能带路径、查询、片段或用户信息")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("没有主机")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("端口是空的")
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", errors.New("端口不在 1 到 65535 之间")
		}
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
	}
	if port == "" {
		return scheme + "://" + host, nil
	}
	return scheme + "://" + host + ":" + port, nil
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
