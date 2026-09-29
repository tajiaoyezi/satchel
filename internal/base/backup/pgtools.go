package backup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// PGConn 是交给 pg_dump 与 psql 的连接参数，经 PG* 环境变量传给子进程（密码不进命令行参数）。
type PGConn struct {
	Host, User, Password, Database, SSLMode string
	Port                                    int
}

// PGConnFromConfig 从数据库配置取连接参数。
func PGConnFromConfig(cfg db.Config) *PGConn {
	return &PGConn{Host: cfg.Host, Port: cfg.Port, User: cfg.User, Password: cfg.Password, Database: cfg.Name, SSLMode: cfg.SSLMode}
}

func (c *PGConn) env() []string {
	env := append(os.Environ(), "PGHOST="+c.Host, "PGUSER="+c.User, "PGPASSWORD="+c.Password, "PGDATABASE="+c.Database)
	if c.Port != 0 {
		env = append(env, "PGPORT="+strconv.Itoa(c.Port))
	}
	if c.SSLMode != "" {
		env = append(env, "PGSSLMODE="+c.SSLMode)
	}
	return env
}

var versionRe = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

// serverMajor 读服务器的主版本（SHOW server_version_num 除以 10000）。
func serverMajor(ctx context.Context, bdb *bun.DB) (int, error) {
	var num string
	if err := bdb.QueryRowContext(ctx, "SHOW server_version_num").Scan(&num); err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "读取 PostgreSQL 服务器版本失败", err)
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, v1.Newf(v1.CodeDatabase, "PostgreSQL 服务器版本 %q 读不懂", num)
	}
	return n / 10000, nil
}

func installHint(major int) string {
	return fmt.Sprintf("在主控机器上安装 PostgreSQL %d 或更新的客户端，例如 apt install postgresql-client-%d（Debian 系先加 PostgreSQL 官方的 apt 源）；Docker 镜像已预装", major, major)
}

// findPGDump 找 pg_dump 并检查它的主版本不低于服务器（master-backup「PostgreSQL 的客户端工具」）；不满足是 unavailable。
func findPGDump(ctx context.Context, bdb *bun.DB) (string, error) {
	major, err := serverMajor(ctx, bdb)
	if err != nil {
		return "", err
	}
	return findTool(ctx, "pg_dump", "备份", major)
}

// findPsql 找 psql 并检查主版本不低于服务器：新的 pg_dump 导出的 SQL 里有旧 psql 不认识的元命令（如 \restrict）。
func findPsql(ctx context.Context, major int) (string, error) {
	return findTool(ctx, "psql", "恢复备份", major)
}

// findTool 从 PATH 找一个 PostgreSQL 客户端工具并检查它的主版本不低于 major；找不到、读不懂版本、版本太低都是 unavailable，
// next 按服务器主版本给出安装命令。主控不会自己安装。
func findTool(ctx context.Context, name, purpose string, major int) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", v1.Newf(v1.CodeUnavailable, "找不到 %s：PostgreSQL 驱动下%s要用它（服务器主版本 %d）", name, purpose, major).WithNext(installHint(major))
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", v1.Wrap(v1.CodeUnavailable, "运行 "+name+" --version 失败", err).WithNext(installHint(major))
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return "", v1.Newf(v1.CodeUnavailable, "读不懂 %s 的版本：%s", name, bytes.TrimSpace(out)).WithNext(installHint(major))
	}
	if have, _ := strconv.Atoi(string(m[1])); have < major {
		return "", v1.Newf(v1.CodeUnavailable, "%s 的主版本是 %d，低于服务器的 %d", name, have, major).
			WithState(name, have).WithState("server", major).WithNext(installHint(major))
	}
	return path, nil
}

// currentSchema 是这个连接当前的 schema（备份与恢复都只动它）。
func currentSchema(ctx context.Context, bdb *bun.DB) (string, error) {
	var s string
	if err := bdb.QueryRowContext(ctx, "SELECT current_schema()").Scan(&s); err != nil {
		return "", v1.Wrap(v1.CodeDatabase, "读取当前 schema 失败", err)
	}
	return s, nil
}

// CheckClientTools 检查这台主控上 pg_dump 与 psql 是否存在、主版本不低于 major（database test 报告迁过去之后备份与恢复能不能用）。
func CheckClientTools(ctx context.Context, major int) error {
	if _, err := findTool(ctx, "pg_dump", "备份", major); err != nil {
		return err
	}
	_, err := findPsql(ctx, major)
	return err
}
