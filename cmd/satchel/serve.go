package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/buildinfo"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/logging"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/cli"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// serveCommand 是 satchel serve 的本地处理函数（master-serve「启动平台与启动顺序」）：
// 平台检查 → 读配置 → 确保数据目录 → 日志 → 待恢复与启动检查、开库、迁移、装配、恢复之后的收尾（openApp）→ 监听 → 等信号 → 优雅停止。
func serveCommand(ctx context.Context, inv *command.Invocation) (any, error) {
	if runtime.GOOS != "linux" {
		return nil, v1.Newf(v1.CodeUnsupportedPlatform, "主控只在 Linux 上运行；%s 上的 satchel 只保证客户端子命令可用（第 09 章）", runtime.GOOS).
			WithNext("在 Linux 服务器上安装主控（install.sh 或 Docker），本机用 satchel --server … 连它")
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	dataDir := cli.DataDir(ctx)
	cfg, err := db.LoadServeConfig(dataDir, inv.String("config", ""))
	if err != nil {
		return nil, err
	}
	dbCfg, err := db.LoadConfig(dataDir)
	if err != nil {
		return nil, err
	}
	if err := db.EnsureDataDir(dataDir); err != nil {
		return nil, err
	}
	logger, logFile, err := setupLogging(cfg, dataDir)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	return serveWith(ctx, dataDir, dbCfg, cfg, logger, logFile, buildinfo.Version)
}

// serveWith 是 serve 从处理升级标记起的部分：version 是本二进制的版本（测试里换成升级标记里的新旧版本）。
func serveWith(ctx context.Context, dataDir string, dbCfg db.Config, cfg db.ServeConfig, logger *slog.Logger, logFile io.Closer, version string) (any, error) {
	// 同一个数据目录上只能有一个主控：拿不到数据目录的锁、或 socket 上已有主控在听时，不碰升级与恢复的状态
	// （否则会对着正在跑的那个做回退）。锁拿到进程退出；自升级 exec 时交给新进程。
	lock, err := acquireServeLock(dataDir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := ensureNotRunning(dataDir); err != nil {
		return nil, err
	}
	// 升级标记（master-self-update）：新版本连续启动失败或回退中途崩溃时，不再往下启动，直接回退。
	up, rollbackTo, err := prepareUpgrade(dataDir, version, logger)
	if err != nil {
		return nil, err
	}
	if rollbackTo != "" {
		return nil, execNow(rollbackTo, logFile, lock)
	}
	a, err := openApp(ctx, dataDir, dbCfg, cfg, logger)
	if err != nil {
		return nil, up.withUpgradeHint(up.failStart(ctx, err, logFile, lock))
	}
	if err := up.finish(ctx, a); err != nil {
		a.db.Close()
		return nil, up.withUpgradeHint(err)
	}
	// 刚升级上来、还没过健康检查的新版本：从开始监听起暂停写入（只放行查 job），过了健康检查、标记改成 committed 才放开——
	// 这之前随时可能回退，回退换回升级前的库，这期间收下的写入会被丢掉（例如刚吊销的令牌又生效）。
	if up != nil && up.isNew {
		a.writeGate.Suspend(fmt.Sprintf("主控刚升级到 %s，正在做健康检查，这期间只能查长任务", up.m.ToVersion),
			"稍等几秒：健康检查通过后放开，不过就自动回退到 "+up.m.FromVersion)
	}
	tcp, unix, err := a.listen(cfg.Listen)
	if err != nil {
		a.db.Close()
		return nil, up.failStart(ctx, err, logFile, lock)
	}
	hctx, stopHealth := context.WithCancel(ctx)
	healthDone := make(chan struct{})
	go func() { defer close(healthDone); up.watchHealth(hctx, a, tcp.Addr()) }()
	err = a.serve(ctx, tcp, unix)
	stopHealth()
	<-healthDone             // 健康检查与收尾都停下、标记不再被改写之后，才按磁盘上的标记判断
	up.uncountIfStopped(ctx) // 健康检查通过之前收到停止信号：这次启动不算
	if err != nil {
		return nil, err
	}
	// 回退失败而停下：以错误退出（非 0），服务管理器拉起之后按升级标记接着处理。
	if err := a.stopError(); err != nil {
		return nil, err
	}
	// 自升级或升级回退：优雅停止之后 exec 目标路径上的二进制（master-serve「优雅停止」）。同时收到了停止信号时不 exec：
	// 信号已经在这个进程里用掉，exec 出来的新进程不知道有人要它停，会被服务管理器 SIGKILL；目标路径上已经是该跑的二进制，
	// 升级标记与待恢复标记会驱动下一次启动。
	if path := a.pendingExec(); path != "" {
		if ctx.Err() != nil {
			logger.Warn("收到停止信号，不再原地重启；下次启动按升级标记接着处理", "path", path)
			return map[string]any{"stopped": true}, nil
		}
		return nil, execNow(path, logFile, lock)
	}
	return map[string]any{"stopped": true}, nil
}

// openApp 是开始监听之前的那一段（master-serve「启动平台与启动顺序」）：有待恢复标记就先换库、SQLite 做启动检查与自动恢复
// （base/backup.Startup，这时库还没打开）→ 开库 → 迁移并比对结构 → 装配（里面确保设置单例行、收尾上次没结束的任务与长任务）
// → 恢复之后的收尾（作废并重发恢复码、last_restore、审计；做完才开始监听，也就「最后才放开登录」）。
func openApp(ctx context.Context, dataDir string, dbCfg db.Config, cfg db.ServeConfig, logger *slog.Logger) (*app, error) {
	var pg *archive.PGConn
	if dbCfg.Driver == db.DriverPostgres {
		pg = archive.PGConnFromConfig(dbCfg)
	}
	marker, err := archive.Startup(ctx, archive.Target{DataDir: dataDir, Config: dbCfg, PG: pg, Version: buildinfo.Version}, logger)
	if err != nil {
		return nil, err
	}
	// 升级回退时换回升级前的库失败：不能照常用升级之后的库跑旧二进制（master-self-update「失败回退」要求二者一起回来）。
	// 拒绝启动、不收尾，待恢复标记与升级标记都留着，下次启动还是这里。
	if marker != nil && marker.Source == archive.SourceUpgradeRollback && marker.Phase == archive.PhaseFailed {
		return nil, v1.Newf(v1.CodeDatabase, "升级回退时换回升级前的库失败：%s", marker.Error).WithState("backup", marker.Backup).
			WithNext("处理好失败的原因后，把数据目录里 " + db.RestorePendingFile + " 的 phase 改回 pending 再启动，主控会重试换回 backups/" +
				marker.Backup + "；不要删这两个标记，也不要删库")
	}
	bdb, err := db.Open(ctx, dbCfg)
	if err != nil {
		return nil, err
	}
	if applied, err := db.Migrate(ctx, bdb); err != nil {
		bdb.Close()
		return nil, err
	} else if len(applied) > 0 {
		logger.Info("已应用迁移", "migrations", applied)
	}
	a, err := newApp(dataDir, bdb, logger, cfg)
	if err != nil {
		bdb.Close()
		return nil, err
	}
	if err := a.backups.Finalize(ctx, marker); err != nil {
		bdb.Close()
		return nil, err
	}
	return a, nil
}

// setupLogging 建 serve 的日志（master-logs「serve 的日志输出」）：stderr 加 logs/satchel.log，并设成进程的默认 logger，
// 让直接调 slog 包函数的代码也走同一个输出与级别。数据目录（含 logs/）要先建好。
func setupLogging(cfg db.ServeConfig, dataDir string) (*slog.Logger, io.Closer, error) {
	logger, closer, err := logging.New(cfg.SlogLevel(), dataDir)
	if err != nil {
		return nil, nil, err
	}
	slog.SetDefault(logger)
	return logger, closer, nil
}
