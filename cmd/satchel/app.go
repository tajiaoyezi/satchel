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
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	"github.com/satchel/satchel/internal/core/sessions"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/core/users"
	mwaudit "github.com/satchel/satchel/internal/middleware/audit"
	"github.com/satchel/satchel/internal/middleware/authn"
	"github.com/satchel/satchel/internal/middleware/authz"
	"github.com/satchel/satchel/internal/projection/cli"
	"github.com/satchel/satchel/internal/projection/mcp"
	"github.com/satchel/satchel/internal/projection/rest"
	"github.com/satchel/satchel/internal/projection/web"
	svcaudit "github.com/satchel/satchel/internal/service/audit"
	"github.com/satchel/satchel/internal/service/auth"
	svcsettings "github.com/satchel/satchel/internal/service/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// shutdownTimeout 是优雅停止等进行中请求的上限（master-serve「优雅停止」）。
const shutdownTimeout = 10 * time.Second

// app 是装配好的主控：执行链、HTTP 处理器与要关的资源。
type app struct {
	table   *command.Table
	runner  command.Runner
	handler http.Handler
	db      *bun.DB
	dataDir string
	logger  *slog.Logger
}

// newApp 装配各层。bdb 已打开且已迁移；数据目录已存在。
func newApp(dataDir string, bdb *bun.DB, logger *slog.Logger) (*app, error) {
	table := command.Catalog()
	st := store.New(bdb, schema.Default())
	audits := svcaudit.New(coreaudit.New(bdb, st))
	// 身份：用户与会话两个仓储归 service/auth 持有；它同时是 authn 的会话解析器、authz 的当场验证器、REST 会话入口的业务。
	identity := auth.New(users.New(bdb, st), sessions.New(bdb))
	// 系统设置：迁移之后、监听之前确保单例行存在（master-settings「单例行的建立」），读命令不建行。
	settingsRepo := coresettings.New(bdb, st, schema.Default())
	if err := settingsRepo.EnsureSingleton(context.Background()); err != nil {
		return nil, err
	}
	settings := svcsettings.New(settingsRepo)

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
	mux.Handle(command.APIPrefix, rest.NewHandler(table, runner, identity))
	mux.Handle(mcp.Path, mcp.NewHandler(opts))
	mux.Handle(web.PublicPrefix, web.PublicHandler(filepath.Join(dataDir, db.PublicDir)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rest.WriteError(w, v1.Newf(v1.CodeNotFound, "没有这个路径：%s", r.URL.Path))
	})

	// 顺序：authn 先判身份（同源检查要看身份来源），SameOrigin 管住所有浏览器发来的写请求（REST、/mcp、会话入口）。
	return &app{table: table, runner: runner, handler: authn.Middleware(identity, rest.SameOrigin(mux)), db: bdb, dataDir: dataDir, logger: logger}, nil
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

// serve 在两个监听上跑同一套处理器，直到 ctx 取消；然后优雅停止（等进行中的请求，上限 shutdownTimeout）、关库、删 socket。
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
	a.logger.Info("主控已启动", "listen", tcp.Addr().String(), "socket", unix.Addr().String())
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}
	a.logger.Info("开始优雅停止", "timeout", shutdownTimeout)
	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(stopCtx); err != nil {
		a.logger.Warn("优雅停止超时，仍在进行的请求被中断", "error", err)
		_ = srv.Close()
	}
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

// newLogger 建 serve 的日志：文本、写 stderr、级别按配置（m1-06 再接文件与内存环）。
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// listenAddrOf 给日志与测试用：拿到 TCP 实际绑定的地址（随机端口时才知道）。
func listenAddrOf(ln net.Listener) string {
	return fmt.Sprintf("http://%s", ln.Addr().String())
}
