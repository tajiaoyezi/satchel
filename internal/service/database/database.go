// Package database 是业务层的数据库设置与在线迁移（master-db-migration）：database show / test / migrate 三条命令。
// 迁移把主控从 SQLite 搬到一个空的 PostgreSQL：拿着 SQLite 的写锁拷贝、在一个目标事务里写完并核对、改写 database.json 作为提交点、
// 然后让 serve 退出由服务管理器拉起；提交点之前任何一步失败都退回 SQLite、把目标库清空。拷贝本身在 base/db。
package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/bun"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/command"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 连目标库的时限（试连与迁移前的检查）；受理请求结束的最长等待。
const (
	connectTimeout = 15 * time.Second
	acceptWait     = time.Minute
)

// stopDelay 是迁移成功之后过多久才让 serve 停止：留出时间让跟着这个 job 的 CLI（每秒查一次）与 MCP（每半秒）看到 done，
// 不至于下一次查的时候主控已经停了、把成功报成连不上。这段时间拦截一直开着。变量只为测试能调小。
var stopDelay = 3 * time.Second

// drainTimeout 是暂停写入之后等已经进门的请求走完的上限。变量只为测试能调小。
var drainTimeout = 30 * time.Second

// Deps 是本服务要的东西。Lock、Unlock、StartJob、RequestStop 由装配根注入：业务层的模块之间不互相引用。
type Deps struct {
	DataDir  string
	DB       *bun.DB   // 当前的库
	Config   db.Config // 当前的数据库配置（含环境变量覆盖之后的）
	Gate     *db.WriteGate
	Registry *schema.Registry
	Logger   *slog.Logger
	// Lock 与 Unlock 是与备份、恢复共用的「同一时刻只有一次」（service/backup 的 Begin / End）。
	Lock   func(what string) error
	Unlock func()
	// StartJob 受理一个长任务（service/jobs 的 Start）。
	StartJob func(ctx context.Context, inv *command.Invocation, run func(context.Context) (any, error), done func()) (any, error)
	// RequestStop 让 serve 优雅停止（提交点之后）。
	RequestStop func()
	// OpenTarget 打开目标库，返回库与关闭它的函数；nil 时用 db.Open。测试替换它，接到自己的随机 schema 上。
	OpenTarget func(ctx context.Context, cfg db.Config) (*bun.DB, func(), error)
}

// Service 持有依赖。
type Service struct {
	d   Deps
	now func() time.Time
}

// New 建服务。
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.OpenTarget == nil {
		d.OpenTarget = func(ctx context.Context, cfg db.Config) (*bun.DB, func(), error) {
			bdb, err := db.Open(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return bdb, func() { bdb.Close() }, nil
		}
	}
	return &Service{d: d, now: time.Now}
}

// SetOpenTarget 换掉打开目标库的方式，只给测试用（端到端测试要接到自己的随机 schema 上）。
func (s *Service) SetOpenTarget(f func(ctx context.Context, cfg db.Config) (*bun.DB, func(), error)) {
	s.d.OpenTarget = f
}

// Bindings 是三条 database 命令的处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{"database show": s.show, "database test": s.test, "database migrate": s.migrate}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "数据库设置只对管理员开放")
	}
	return nil
}

// Info 是 database show 的输出。
type Info struct {
	Driver             db.Driver `json:"driver"`
	Path               string    `json:"path,omitempty"`
	Size               int64     `json:"size,omitempty"`
	Host               string    `json:"host,omitempty"`
	Port               int       `json:"port,omitempty"`
	Name               string    `json:"name,omitempty"`
	User               string    `json:"user,omitempty"`
	SSLMode            string    `json:"sslmode,omitempty"`
	PasswordConfigured bool      `json:"password_configured"`
	EnvOverride        bool      `json:"env_override"`
}

func (s *Service) show(ctx context.Context, _ *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	c := s.d.Config
	info := Info{Driver: c.Driver, EnvOverride: db.EnvOverride()}
	if c.Driver == db.DriverSQLite {
		info.Path = c.Path
		for _, suffix := range []string{"", "-wal"} {
			if fi, err := os.Stat(c.Path + suffix); err == nil {
				info.Size += fi.Size()
			}
		}
		return info, nil
	}
	info.Host, info.Port, info.Name, info.User, info.SSLMode, info.PasswordConfigured = c.Host, c.Port, c.Name, c.User, c.SSLMode, c.Password != ""
	return info, nil
}

// driverReason 取驱动报的原因给 reason 用（认证失败、拒绝连接、TLS 错误分得清），去掉密码与连接串。
func driverReason(err error, cfg db.Config) string {
	var b strings.Builder
	for e := err; e != nil; e = errors.Unwrap(e) {
		if _, ok := e.(*v1.Error); !ok {
			b.WriteString(e.Error())
			break
		}
	}
	msg := b.String()
	if cfg.Password != "" {
		msg = strings.ReplaceAll(msg, cfg.Password, "***")
	}
	return dsnRe.ReplaceAllString(msg, "postgres://***")
}

var dsnRe = regexp.MustCompile(`postgres(ql)?://\S+`)

var sslModes = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}

// targetConfig 从 flag 取目标 PostgreSQL 的连接参数。
func targetConfig(inv *command.Invocation) (db.Config, error) {
	c := db.Config{Driver: db.DriverPostgres, Host: inv.String("host", ""), Port: inv.Int("port", 5432), Name: inv.String("name", ""),
		User: inv.String("user", ""), Password: inv.String("password", ""), SSLMode: inv.String("sslmode", "prefer")}
	for flag, v := range map[string]string{"host": c.Host, "name": c.Name, "user": c.User} {
		if v == "" {
			return c, v1.Newf(v1.CodeBadRequest, "要用 --%s 指定目标库的%s", flag, map[string]string{"host": "主机", "name": "库名", "user": "用户"}[flag])
		}
	}
	if !sslModes[c.SSLMode] {
		return c, v1.Newf(v1.CodeBadRequest, "--sslmode 不认识 %q", c.SSLMode).WithNext("写 disable、allow、prefer、require、verify-ca、verify-full 之一")
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, v1.Newf(v1.CodeBadRequest, "--port %d 不是端口号", c.Port)
	}
	return c, nil
}

// connect 打开目标库并读概况；连不上是 unavailable（reason 带驱动的原因，不含密码）。
func (s *Service) connect(ctx context.Context, cfg db.Config) (*bun.DB, func(), db.PGServer, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	target, closeFn, err := s.d.OpenTarget(cctx, cfg)
	if err != nil {
		return nil, nil, db.PGServer{}, v1.Wrap(v1.CodeUnavailable, "连不上目标 PostgreSQL "+cfg.Host+"："+driverReason(err, cfg), err).
			WithNext("先用 database test 试连，检查地址、端口、库名、用户与密码")
	}
	info, err := db.PGInfo(cctx, target)
	if err != nil {
		closeFn()
		return nil, nil, info, err
	}
	return target, closeFn, info, nil
}

// TestResult 是 database test 的输出。
type TestResult struct {
	ServerVersion string   `json:"server_version"`
	Empty         bool     `json:"empty"`
	Tables        []string `json:"tables,omitempty"`
	BackupTools   Tools    `json:"backup_tools"`
}

// Tools 是这台主控上 pg_dump 与 psql 够不够用（迁过去之后备份与恢复要用）。
type Tools struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Next  string `json:"next,omitempty"`
}

func (s *Service) test(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	cfg, err := targetConfig(inv)
	if err != nil {
		return nil, err
	}
	_, closeFn, info, err := s.connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	res := TestResult{ServerVersion: info.Version, Empty: len(info.Tables) == 0, BackupTools: Tools{OK: true}}
	res.Tables = info.Tables
	if len(res.Tables) > 20 {
		res.Tables = res.Tables[:20]
	}
	if err := archive.CheckClientTools(ctx, info.Major); err != nil {
		e := v1.AsError(err)
		res.BackupTools = Tools{Error: e.Reason, Next: e.Next}
	}
	return res, nil
}

// Progress 是迁移报告的进度（master-db-migration「进度」）。
type Progress struct {
	Phase string `json:"phase"`
	db.CopyProgress
}

func (s *Service) migrate(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.d.Config.Driver != db.DriverSQLite {
		return nil, v1.New(v1.CodeConflict, "主控用的已经是 PostgreSQL，只支持从 SQLite 迁移到 PostgreSQL")
	}
	if db.EnvOverride() {
		return nil, v1.New(v1.CodeConflict, "数据库配置被 SATCHEL_DATABASE_* 环境变量覆盖了：迁移改写 database.json 不会生效").
			WithNext("去掉这些环境变量（或直接把它们改成 PostgreSQL 的参数并自行搬数据）后再迁移")
	}
	cfg, err := targetConfig(inv)
	if err != nil {
		return nil, err
	}
	if err := s.d.Lock("迁移"); err != nil {
		return nil, err
	}
	target, closeFn, info, err := s.connect(ctx, cfg)
	if err != nil {
		s.d.Unlock()
		return nil, err
	}
	if len(info.Tables) > 0 {
		closeFn()
		s.d.Unlock()
		return nil, v1.Newf(v1.CodeConflict, "目标库的当前 schema 不是空的，已有表：%v", firstN(info.Tables, 20)).
			WithNext("换一个空库，或先清空目标 schema")
	}
	accepted := ctx.Done() // 受理它的请求结束（含写完审计）时关闭
	job, err := s.d.StartJob(ctx, inv, func(jctx context.Context) (any, error) {
		return s.run(jctx, accepted, target, cfg)
	}, func() {
		closeFn()
		s.d.Unlock()
	})
	if err != nil {
		closeFn()
		s.d.Unlock()
	}
	return job, err
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// run 是迁移这个长任务的工作（master-db-migration「拷贝期间阻塞写入」到「失败处理」，design 第 1 到 4 条）。
func (s *Service) run(ctx context.Context, accepted <-chan struct{}, target *bun.DB, cfg db.Config) (res any, err error) {
	report := func(p Progress) { corejobs.Report(ctx, p) }
	report(Progress{Phase: "waiting"})
	select {
	case <-accepted:
	case <-time.After(acceptWait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 从这里起挡住写入：进门的地方提前拒绝，SQLite 的写锁兜底。开关打开之前已经进门的命令先等它们走完，它们的写入与审计都进快照，
	// 不会堵在写锁上等满 busy_timeout 失败。
	s.d.Gate.Suspend("正在把数据库迁移到 PostgreSQL，这期间主控只能查长任务", "用 satchel job get <job_id> 看迁移进度；迁移结束后主控会重启")
	dctx, cancel := context.WithTimeout(ctx, drainTimeout)
	err = s.d.Gate.Drain(dctx)
	cancel()
	if err != nil {
		s.d.Gate.Resume()
		if ctx.Err() != nil {
			return nil, ctx.Err() // 主控在停止
		}
		return nil, v1.Wrap(v1.CodeUnavailable, fmt.Sprintf("暂停写入之前进门的请求 %s 内没有走完", drainTimeout), err).WithNext("稍后重试")
	}
	committed := false
	src, err := s.d.DB.BeginTx(ctx, nil) // DSN 带 _txlock=immediate：这就是 BEGIN IMMEDIATE，拿到写锁
	if err != nil {
		s.d.Gate.Resume()
		return nil, v1.Wrap(v1.CodeDatabase, "拿 SQLite 的写锁失败", err)
	}
	defer func() {
		// 成功时这个只读事务一直留到进程退出：写锁不放，放开的话，拦截之前就进了处理函数、正堵在锁上的写入会提交进
		// 马上要弃用的 SQLite，客户端还拿到成功（审查第 1 条）。它们会等满 busy_timeout 失败。
		if !committed {
			src.Rollback() // 只读过，回滚就是放开写锁
			s.d.Gate.Resume()
		}
	}()
	jobID := corejobs.CurrentID(ctx)
	fail := func(err error) error {
		if derr := db.DropCreated(context.WithoutCancel(ctx), target, s.d.Registry); derr != nil {
			return v1.Newf(v1.CodeDatabase, "%s；清理目标库也失败了（%v），请手工清空目标 schema 后再试", v1.AsError(err).Reason, derr)
		}
		return err
	}
	report(Progress{Phase: "migrating_schema"})
	if _, err := db.Migrate(ctx, target); err != nil {
		return nil, fail(err)
	}
	dst, err := target.BeginTx(ctx, nil)
	if err != nil {
		return nil, fail(v1.Wrap(v1.CodeDatabase, "在目标库上开事务失败", err))
	}
	rep, err := db.CopyToPostgres(ctx, src, dst, s.d.Registry, func(p db.CopyProgress) { report(Progress{Phase: "copying", CopyProgress: p}) })
	if err == nil {
		report(Progress{Phase: "verifying"})
		err = db.VerifyCounts(ctx, src, dst, s.d.Registry)
	}
	if err == nil {
		report(Progress{Phase: "committing"})
		out, _ := json.Marshal(rep)
		err = corejobs.MarkDoneIn(ctx, dst, jobID, string(out), s.now())
	}
	if err == nil {
		err = dst.Commit()
	}
	if err != nil {
		dst.Rollback() // Commit 失败后再回滚一次无害（返回 ErrTxDone），保证连接不再占着目标库上的锁
	}
	if err != nil {
		return nil, fail(err)
	}
	// 提交点：database.json 改名。之后放开 SQLite 的写锁（defer），拦截保持到进程退出。
	if err := db.SaveConfig(s.d.DataDir, cfg); err != nil {
		if !errors.Is(err, db.ErrConfigNotSynced) {
			return nil, fail(err) // 改名没发生：新配置不生效，退回 SQLite
		}
		// 改名已经发生，新配置已经生效：只是目录没能落盘，不能再清空目标库（审查第 2 条）。
		s.d.Logger.Error("database.json 已经改成 PostgreSQL，但数据目录没能落盘；断电前请确认它已经写到磁盘上", "error", err)
	}
	committed = true
	s.d.Logger.Warn("已迁移到 PostgreSQL，主控现在停止，由服务管理器拉起后连接新库", "host", cfg.Host, "name", cfg.Name, "tables", rep.Tables, "rows", rep.Rows)
	if s.d.RequestStop != nil {
		time.AfterFunc(stopDelay, s.d.RequestStop)
	}
	return rep, nil
}
