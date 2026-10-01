package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/rest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// suspendedError 是写入暂停期间被拦下的回应（master-db-migration「拷贝期间阻塞写入」、master-self-update）：原因与下一步
// 由打开开关的一方给（迁移数据库或升级主控）。
func suspendedError(gate *db.WriteGate) error {
	reason, next := gate.Why()
	return v1.New(v1.CodeUnavailable, reason).WithNext(next)
}

// migratingAllowed 是写入暂停期间放行的命令：只读 job，看迁移或升级的进度。
var migratingAllowed = map[string]bool{"job get": true, "job list": true}

// gatedRunner 套在执行链最外层（审计之外，被拒的不写审计）：「写入暂停」开着时，除 job get、job list 之外一律 unavailable。
// 非 read 类别的命令进门时登记、处理完才离开，并在 ctx 上标出「已进门」：开关打开之后，迁移与自升级等这些已经进门的命令都走完
// （WriteGate.Drain）再拿写锁或做备份，它们的审计也照常写库。read 类别的命令不登记（它们不改业务数据，update check 这类
// 还可能慢到几十秒，不该拖住 Drain），开关开着时同样被拒。
func gatedRunner(gate *db.WriteGate, table *command.Table, next command.Runner) command.Runner {
	return command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
		if migratingAllowed[inv.Name()] {
			return next.Run(ctx, inv)
		}
		if cmd, ok := table.Lookup(inv.Name()); ok && cmd.Class == command.ClassRead {
			if gate.Suspended() {
				return nil, suspendedError(gate)
			}
			return next.Run(ctx, inv)
		}
		if !gate.Enter() {
			return nil, suspendedError(gate)
		}
		defer gate.Leave()
		return next.Run(db.WithEntered(ctx), inv)
	})
}

// gatedSessions 在「写入暂停」开着时拦下四个会话入口（登录会写会话）与初始化向导的建管理员（执行链跑完之后 REST 层还会写会话），
// 开关没开时登记进门，与 gatedRunner 一样让 Drain 等它们走完。登记之前先把请求体读完（上限与 REST 相同）：
// 慢慢发请求体的客户端（这几个入口不要身份）不能占着登记，拖住迁移或升级的 Drain。
func gatedSessions(gate *db.WriteGate, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == command.APIPrefix+"session" || strings.HasPrefix(r.URL.Path, command.APIPrefix+"session/") ||
			r.URL.Path == command.APIPrefix+"setup/init" {
			if gate.Suspended() {
				rest.WriteError(w, suspendedError(gate))
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, rest.MaxBodyBytes+1))
			if err != nil {
				rest.WriteError(w, v1.Wrap(v1.CodeBadRequest, "读取请求体失败", err))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body)) // 超过上限的由 REST 层照常报错
			if !gate.Enter() {
				rest.WriteError(w, suspendedError(gate))
				return
			}
			defer gate.Leave()
		}
		next.ServeHTTP(w, r)
	})
}
