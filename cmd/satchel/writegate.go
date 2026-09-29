package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/rest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// migratingError 是在线迁移期间被拦下的回应（master-db-migration「拷贝期间阻塞写入」）。
func migratingError() error {
	return v1.New(v1.CodeUnavailable, "正在把数据库迁移到 PostgreSQL，这期间主控只能查长任务").
		WithNext("用 satchel job get <job_id> 看迁移进度；迁移结束后主控会重启")
}

// migratingAllowed 是迁移期间放行的命令：只读 job，看迁移进度。
var migratingAllowed = map[string]bool{"job get": true, "job list": true}

// gatedRunner 套在执行链最外层（审计之外，被拒的不写审计）：「写入暂停」开着时，除 job get、job list 之外一律 unavailable。
func gatedRunner(gate *db.WriteGate, next command.Runner) command.Runner {
	return command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
		if gate.Suspended() && !migratingAllowed[inv.Name()] {
			return nil, migratingError()
		}
		return next.Run(ctx, inv)
	})
}

// gatedSessions 在「写入暂停」开着时拦下四个会话入口（登录会写会话）。
func gatedSessions(gate *db.WriteGate, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gate.Suspended() && (r.URL.Path == command.APIPrefix+"session" || strings.HasPrefix(r.URL.Path, command.APIPrefix+"session/")) {
			rest.WriteError(w, migratingError())
			return
		}
		next.ServeHTTP(w, r)
	})
}
