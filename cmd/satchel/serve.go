package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/cli"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// serveCommand 是 satchel serve 的本地处理函数（master-serve「启动平台与启动顺序」）：
// 平台检查 → 读配置 → 确保数据目录、开库 → 迁移并比对结构 → 装配 → 监听 → 等信号 → 优雅停止。
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
	logger := newLogger(cfg.SlogLevel())
	bdb, err := cli.OpenForWrite(ctx, dataDir)
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
	tcp, unix, err := a.listen(cfg.Listen)
	if err != nil {
		bdb.Close()
		return nil, err
	}
	if err := a.serve(ctx, tcp, unix); err != nil {
		return nil, err
	}
	return map[string]any{"stopped": true}, nil
}
