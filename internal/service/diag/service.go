// Package diag 是业务层的三层无 SSH 远程排障服务（node-remote-diag-exec-and-evidence）：
// 固定只读诊断集（server diag）、受控任意命令执行（server exec，含四重闸门/并发队列/WS 流式与 HTTP 轮询/断连 unknown 与重连补报）、
// 以及一键取证与证据包查询（server evidence、evidence list、evidence get 与 CollectEvidence 复用入口）。
// 严守六层架构单向依赖：仅依赖 internal/core/*、internal/base/* 与 internal/command，不 import 其它 service/*。
package diag

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	coreevidence "github.com/satchel/satchel/internal/core/evidence"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// NodeClient 是调节点诊断/受控执行管理口 RPC 与读取运行态快照的最小接口（由 service/servers 在装配根直接满足）。
type NodeClient interface {
	CallDiagRun(ctx context.Context, serverID int64, params xrpc.DiagRunParams) (*xrpc.DiagRunResult, error)
	CallExecStart(ctx context.Context, serverID int64, params xrpc.ExecStartParams) (*xrpc.ExecStartResult, error)
	CallExecStatus(ctx context.Context, serverID int64, params xrpc.ExecStatusParams) (*xrpc.ExecStatusResult, error)
	CallExecCancel(ctx context.Context, serverID int64, params xrpc.ExecCancelParams) (*xrpc.ExecCancelResult, error)
	CallAgentInfo(ctx context.Context, serverID int64) (*xrpc.AgentInfo, error)
	ConnectedWS(serverID int64) bool
	NodeRuntime(serverID int64) (agentconn.NodeRuntime, bool)
	MasterMachineID() string
}

// JobStarter 启动关联服务器的长任务（由 service/jobs.StartForServer 注入）。
type JobStarter func(ctx context.Context, serverID int64, inv *command.Invocation, run func(context.Context) (any, error), done func()) (*corejobs.Job, error)

// JobReconciler 将 unknown 态长任务收敛为终态 done 或 failed（由 service/jobs.ReconcileUnknown 注入）。
type JobReconciler func(ctx context.Context, jobID, status string, exitCode int64, output string, truncated bool) error

// UnknownJobLister 列出指定服务器处于 unknown 态的指定类型长任务（由 service/jobs.ListUnknownByServer 注入）。
type UnknownJobLister func(ctx context.Context, serverID int64, kind string) ([]*corejobs.Job, error)

// Deps 聚合诊断、受控执行与一键取证服务所需依赖。
type Deps struct {
	Servers          *coreservers.Repo
	Settings         *coresettings.Repo
	Evidence         *coreevidence.Repo
	JobsRepo         *corejobs.Repo
	AuditRepo        *coreaudit.Repo
	Nodes            NodeClient
	StartJob         JobStarter
	ReconcileUnknown JobReconciler
	ListUnknownJobs  UnknownJobLister
	Logger           *slog.Logger
	Now              func() time.Time
	PollInterval     time.Duration // HTTP/pull 模式轮询间隔，默认 200ms
	WSFallbackPoll   time.Duration // WS 模式下兜底轮询间隔，默认 2s
}

// Service 是节点远程诊断、受控执行与一键取证服务。
type Service struct {
	deps Deps

	execMu        sync.Mutex
	runningCnt    int
	queue         []*queuedExec
	activeJobs    map[string]*execTrack
	byServer      map[int64]map[string]*execTrack
	recentOffline map[int64]time.Time
	reconciling   map[string]bool
}

// New 构造服务实例。
func New(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PollInterval <= 0 {
		deps.PollInterval = 200 * time.Millisecond
	}
	if deps.WSFallbackPoll <= 0 {
		deps.WSFallbackPoll = 2 * time.Second
	}
	return &Service{
		deps:          deps,
		activeJobs:    map[string]*execTrack{},
		byServer:      map[int64]map[string]*execTrack{},
		recentOffline: map[int64]time.Time{},
		reconciling:   map[string]bool{},
	}
}

// Bindings 返回全部 5 条管理员专属命令绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"server diag":     s.serverDiag,
		"server exec":     s.serverExec,
		"server evidence": s.serverEvidence,
		"evidence list":   s.evidenceList,
		"evidence get":    s.evidenceGet,
	}
}

func (s *Service) now() time.Time {
	return s.deps.Now().UTC()
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "远程诊断、受控执行与取证命令只对管理员开放")
	}
	return nil
}

func (s *Service) resolveServer(ctx context.Context, ref string) (*coreservers.Server, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, v1.New(v1.CodeBadRequest, "必须指定服务器名或 ID")
	}
	srv, err := s.deps.Servers.ByName(ctx, ref)
	if err == nil {
		return srv, nil
	}
	if !errors.Is(err, coreservers.ErrNotFound) {
		return nil, err
	}
	if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil && id > 0 {
		srv, err = s.deps.Servers.ByID(ctx, id)
		if err == nil {
			return srv, nil
		}
		if !errors.Is(err, coreservers.ErrNotFound) {
			return nil, err
		}
	}
	return nil, v1.Newf(v1.CodeNotFound, "没有叫 %q 的服务器", ref).WithNext("用 server list 查看已有的服务器")
}

func (s *Service) loadDiagLimits(ctx context.Context) coresettings.DiagLimits {
	if s.deps.Settings == nil {
		return coresettings.DefaultDiagLimits()
	}
	lim, err := s.deps.Settings.LoadDiagLimits(ctx)
	if err != nil {
		return coresettings.DefaultDiagLimits()
	}
	return lim
}

func (s *Service) loadMasterURL(ctx context.Context) string {
	if s.deps.Settings == nil {
		return ""
	}
	st, err := s.deps.Settings.Load(ctx)
	if err != nil || st == nil {
		return ""
	}
	return strings.TrimSpace(st.Gates().MasterURL)
}
