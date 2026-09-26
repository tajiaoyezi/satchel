// satchel 的装配根：new 各层、把命令表里的命令绑到 service 的处理函数、串起横切层、起两个监听。
// 业务不在这里写；这里只有「谁持有谁」。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/captcha"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	coresecurity "github.com/satchel/satchel/internal/core/security"
	"github.com/satchel/satchel/internal/core/sessions"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/core/taskruns"
	coretokens "github.com/satchel/satchel/internal/core/tokens"
	"github.com/satchel/satchel/internal/core/users"
	mwaudit "github.com/satchel/satchel/internal/middleware/audit"
	"github.com/satchel/satchel/internal/middleware/authn"
	"github.com/satchel/satchel/internal/middleware/authz"
	"github.com/satchel/satchel/internal/middleware/gate"
	"github.com/satchel/satchel/internal/projection/cli"
	"github.com/satchel/satchel/internal/projection/mcp"
	"github.com/satchel/satchel/internal/projection/rest"
	"github.com/satchel/satchel/internal/projection/scheduler"
	"github.com/satchel/satchel/internal/projection/web"
	svcaudit "github.com/satchel/satchel/internal/service/audit"
	"github.com/satchel/satchel/internal/service/auth"
	svclogs "github.com/satchel/satchel/internal/service/logs"
	"github.com/satchel/satchel/internal/service/schedule"
	svcsecurity "github.com/satchel/satchel/internal/service/security"
	svcsettings "github.com/satchel/satchel/internal/service/settings"
	svctokens "github.com/satchel/satchel/internal/service/tokens"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// shutdownTimeout 是优雅停止等进行中请求的上限（master-serve「优雅停止」）。
const shutdownTimeout = 10 * time.Second

// entryRoutes 是入口归属表（master-access-gates「每个入口在三道门表里的归属」）：顶层 mux 的每个挂载点在这里都有一行
// （测试守着），healthz 在 REST 里面、单独一行。M2 的节点通道、M3 的订阅入口加进来时各加一行；没有匹配的路径算面板。
var entryRoutes = []gate.Route{
	{Pattern: command.APIPrefix + "healthz", Entry: gate.EntryMachine},
	{Pattern: web.PublicPrefix, Entry: gate.EntryMachine},
	{Pattern: mcp.Path, Entry: gate.EntryMCP},
	{Pattern: command.APIPrefix, Entry: gate.EntryPanel},
	{Pattern: "/", Entry: gate.EntryPanel},
}

// app 是装配好的主控：执行链、HTTP 处理器、内置任务与要关的资源。gate、guard、identity 留着给端到端测试换时钟与验证服务。
type app struct {
	table     *command.Table
	runner    command.Runner
	handler   http.Handler
	mounts    []string
	db        *bun.DB
	dataDir   string
	logger    *slog.Logger
	gate      *gate.Gate
	guard     *svcsecurity.Service
	identity  *auth.Service
	sched     *scheduler.Scheduler
	schedules *schedule.Service
}

// newApp 装配各层。bdb 已打开且已迁移；数据目录已存在；cfg 是 serve 的配置（这里用自救开关与允许跨域的来源）。
func newApp(dataDir string, bdb *bun.DB, logger *slog.Logger, cfg db.ServeConfig) (*app, error) {
	table := command.Catalog()
	st := store.New(bdb, schema.Default())
	audits := svcaudit.New(coreaudit.New(bdb, st))
	// 安全事件与封禁：service/security 管令牌猜测的计数与封禁（authn 计数、门查封禁），service/auth 直接写登录的安全事件。
	events := coresecurity.New(bdb)
	guard := svcsecurity.New(events, logger)
	if err := guard.Restore(context.Background()); err != nil {
		return nil, err
	}
	// 身份：用户与会话两个仓储归 service/auth 持有；它同时是 authn 的会话解析器、authz 的当场验证器、REST 会话入口的业务，
	// 也管登录限流与 Turnstile（master-login-protection）。
	accounts := users.New(bdb, st)
	identity := auth.New(accounts, sessions.New(bdb))
	identity.SetEvents(events, logger)
	identity.SetCaptcha(captcha.New())
	// API 令牌：service/tokens 同时是 authn 的令牌解析器（按签发者当下的角色取交集，要读用户仓储）。
	tokens := svctokens.New(coretokens.New(bdb, st), accounts, logger)
	// 系统设置：迁移之后、监听之前确保单例行存在（master-settings「单例行的建立」），读命令不建行。
	settingsRepo := coresettings.New(bdb, st, schema.Default())
	if err := settingsRepo.EnsureSingleton(context.Background()); err != nil {
		return nil, err
	}
	settings := svcsettings.New(settingsRepo)
	// 内置任务的运行记录：上次主控停止时还在跑的记录先收尾，再装配任务（master-scheduler）。
	schedules := schedule.New(taskruns.New(bdb), logger)
	if err := schedules.MarkInterrupted(context.Background()); err != nil {
		return nil, err
	}
	sched := scheduler.New(scheduler.Tasks(scheduler.Deps{DB: bdb, Auth: identity, Audit: audits, Security: guard, Schedule: schedules, Logger: logger}),
		schedules, logger)
	schedules.SetTasks(sched.Infos())

	bindings := command.Bindings{
		"whoami":     func(ctx context.Context, _ *command.Invocation) (any, error) { return v1.IdentityFrom(ctx), nil },
		"audit list": audits.ListHandler(),
		"explain": func(_ context.Context, inv *command.Invocation) (any, error) {
			return command.Explain(table, inv.Arg(0))
		},
	}
	for name, h := range identity.Bindings() {
		bindings[name] = h
	}
	for name, h := range settings.Bindings() {
		bindings[name] = h
	}
	for name, h := range tokens.Bindings() {
		bindings[name] = h
	}
	for name, h := range guard.Bindings() {
		bindings[name] = h
	}
	for name, h := range svclogs.New(dataDir).Bindings() {
		bindings[name] = h
	}
	for name, h := range schedules.Bindings() {
		bindings[name] = h
	}
	if err := table.CheckBindings(bindings); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "命令表与处理函数的绑定不一致", err)
	}
	// 执行链：留痕在最外层，权限在里面，最里面按绑定分发；身份在 HTTP 层由 authn 放进 ctx。
	runner := mwaudit.Wrap(audits, table, logger, authz.Wrap(table, identity.Verifier(), command.Dispatch(bindings)))

	// CLI 的选项在主控进程里也要一份：MCP 的 satchel_run 用它解析命令数组、用进程内的执行链执行。
	opts := cli.DefaultOptions()
	opts.Table = table
	opts.Remote = func(string) command.Runner { return runner }
	opts.ServerSide = true

	mux := http.NewServeMux()
	mounts := []struct {
		pattern string
		handler http.Handler
	}{
		{command.APIPrefix, rest.NewHandler(table, runner, identity)},
		{mcp.Path, mcp.NewHandler(opts)},
		{web.PublicPrefix, web.PublicHandler(filepath.Join(dataDir, db.PublicDir))},
		{"/", http.HandlerFunc(notFoundPath)},
	}
	var patterns []string
	for _, m := range mounts {
		mux.Handle(m.pattern, m.handler)
		patterns = append(patterns, m.pattern)
	}

	// 门：入口归属表、封禁查询、静默模式锁定时用的「不存在的路径」回应、启动时读到的自救开关。
	gates := gate.New(entryRoutes, guard, http.HandlerFunc(hiddenPath), cfg.ForcePublicAccess)
	if cfg.ForcePublicAccess {
		logger.Warn("环境变量 " + db.EnvForcePublicAccess + " 已打开：本进程跳过「关闭公网访问」这一道门（静默模式与封禁照常）；" +
			"进来之后用 satchel settings gates set --set master_local_only=false 关掉，再去掉这个环境变量")
	}
	// 门这一组的设置：启动时加载一次，之后每次设置写成功就推给门、登录限流、令牌猜测的封禁与 Turnstile，不用重启。
	apply := func(s *coresettings.State) {
		g := s.Gates()
		gates.Configure(gate.ConfigFrom(g))
		identity.SetLoginLimits(auth.LoginLimits{MaxAttempts: g.LoginRateMaxAttempts, Window: g.LoginRateWindow, Lock: g.LoginRateLock, SkipLocalIP: g.SkipLocalIP})
		identity.SetTurnstileKeys(g.TurnstileSiteKey, g.TurnstileSecretKey)
		guard.Configure(svcsecurity.Config{Enabled: g.BruteForceEnabled, MaxFailures: g.BruteForceMaxFailures, Window: g.BruteForceWindow,
			Block: g.BruteForceBlock, SkipLocalIP: g.SkipLocalIP})
	}
	current, err := settingsRepo.Load(context.Background())
	if err != nil {
		return nil, err
	}
	apply(current)
	settings.OnWrite(apply)

	// 顺序（从外到内）：门（来源 → 关闭公网访问 → 静默模式 → 封禁）→ CORS → authn（判身份，无效凭据计一次令牌校验失败）
	// → SameOrigin（管住所有浏览器发来的写请求：REST、/mcp、会话入口）→ mux。
	handler := gates.Wrap(rest.CORS(cfg.AllowedOrigins, authn.Middleware(identity, tokens, guard, rest.SameOrigin(mux))))
	return &app{table: table, runner: runner, handler: handler, mounts: patterns, db: bdb, dataDir: dataDir, logger: logger,
		gate: gates, guard: guard, identity: identity, sched: sched, schedules: schedules}, nil
}

// notFoundPath 是顶层没有登记的路径的回应（四字段的 not_found）。
func notFoundPath(w http.ResponseWriter, r *http.Request) {
	rest.WriteError(w, v1.Newf(v1.CodeNotFound, "没有这个路径：%s", r.URL.Path))
}

// hiddenPath 是静默模式锁定时面板入口的回应：与请求一个不存在的路径完全相同——/api/v1/ 下用 REST 自己的 404，其余用上面的兜底。
func hiddenPath(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, command.APIPrefix) {
		rest.NotFound(w, r)
		return
	}
	notFoundPath(w, r)
}

// listen 建两个监听：TCP 在 listenAddr，unix socket 在数据目录下（权限 0600）。socket 文件已存在时先试着连它：
// 连得上说明另一个主控还在跑，拒绝启动而不是把它的 socket 删掉；连不上才是残留文件，删掉重建。
func (a *app) listen(listenAddr string) (tcp, unix net.Listener, err error) {
	sock := filepath.Join(a.dataDir, db.SocketFile)
	if _, statErr := os.Stat(sock); statErr == nil {
		if conn, dialErr := net.DialTimeout("unix", sock, time.Second); dialErr == nil {
			conn.Close()
			return nil, nil, v1.Newf(v1.CodeConflict, "数据目录 %s 已有一个主控在运行（%s 有进程在监听）", a.dataDir, sock).
				WithNext("先停掉那个主控，或给这个实例另指定数据目录")
		}
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, v1.Wrap(v1.CodeInternal, "清理残留的 socket 文件 "+sock+" 失败", err)
	}
	unix, err = net.Listen("unix", sock)
	if err != nil {
		return nil, nil, v1.Wrap(v1.CodeInternal, "监听 unix socket "+sock+" 失败", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		unix.Close()
		return nil, nil, v1.Wrap(v1.CodeInternal, "设置 socket 文件权限失败", err)
	}
	tcp, err = net.Listen("tcp", listenAddr)
	if err != nil {
		unix.Close()
		return nil, nil, v1.Wrap(v1.CodeConfig, "监听 "+listenAddr+" 失败", err).WithNext("换一个 listen 地址（config.yaml 或 SATCHEL_LISTEN），或先停掉占用该端口的进程")
	}
	return tcp, unix, nil
}

// serve 在两个监听上跑同一套处理器、开始内置任务，直到 ctx 取消；然后优雅停止（HTTP 与内置任务同时停，各自最多等
// shutdownTimeout）、补写没写进库的任务记录、关库、删 socket。
func (a *app) serve(ctx context.Context, tcp, unix net.Listener) error {
	srv := &http.Server{
		Handler:           a.handler,
		ConnContext:       authn.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.logger.Handler(), slog.LevelWarn),
	}
	errCh := make(chan error, 2)
	for _, ln := range []net.Listener{tcp, unix} {
		go func(ln net.Listener) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(ln)
	}
	a.sched.Start(ctx)
	a.logger.Info("主控已启动", "listen", tcp.Addr().String(), "socket", unix.Addr().String())
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}
	a.logger.Info("开始优雅停止", "timeout", shutdownTimeout)
	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// 内置任务与 HTTP 同时停：不排在 HTTP 后面，免得 HTTP 用完时限后任务一点时间都没有；都在关库之前停下。
	schedDone := make(chan struct{})
	go func() {
		a.sched.Stop(stopCtx)
		close(schedDone)
	}()
	if err := srv.Shutdown(stopCtx); err != nil {
		a.logger.Warn("优雅停止超时，仍在进行的请求被中断", "error", err)
		_ = srv.Close()
	}
	<-schedDone
	// 请求都停了，占着写锁的事务也就没了：把结束时没改成功的 running 行再写一次。
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	a.schedules.Flush(flushCtx)
	cancelFlush()
	if err := a.db.Close(); err != nil {
		a.logger.Warn("关闭数据库失败", "error", err)
	}
	_ = os.Remove(filepath.Join(a.dataDir, db.SocketFile))
	a.logger.Info("主控已停止")
	if serveErr != nil {
		return v1.Wrap(v1.CodeInternal, "HTTP 服务异常退出", serveErr)
	}
	return nil
}

// listenAddrOf 给日志与测试用：拿到 TCP 实际绑定的地址（随机端口时才知道）。
func listenAddrOf(ln net.Listener) string {
	return fmt.Sprintf("http://%s", ln.Addr().String())
}
