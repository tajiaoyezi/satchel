// Package network 是业务层的节点网络探测、纯 Go WARP 闭环与返程路由查询/测路服务（node-network-warp-and-return-routes）：
// 严守六层架构单向依赖纪律：仅依赖 internal/core/*、internal/base/* 与 internal/command，不 import 其它 service/*。
package network

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	corereturnroutes "github.com/satchel/satchel/internal/core/returnroutes"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// NodeClient 是调节点网络探测、WARP 与返程测路管理口 RPC 的最小接口（由 service/servers 在装配根注入）。
type NodeClient interface {
	CallNetworkPortScan(ctx context.Context, serverID int64, params xrpc.NetworkPortScanParams) (*xrpc.NetworkPortScanResult, error)
	CallNetworkValidateSite(ctx context.Context, serverID int64, params xrpc.NetworkValidateSiteParams) (*xrpc.NetworkValidateSiteResult, error)
	CallNetworkDomainLatency(ctx context.Context, serverID int64, params xrpc.NetworkDomainLatencyParams) (*xrpc.NetworkDomainLatencyResult, error)
	CallNetworkReturnRouteTest(ctx context.Context, serverID int64, params xrpc.NetworkReturnRouteTestParams) (*xrpc.NetworkReturnRouteTestResult, error)
	CallWarpStatus(ctx context.Context, serverID int64) (*xrpc.WarpStatusResult, error)
	CallWarpInstall(ctx context.Context, serverID int64, params xrpc.WarpInstallParams) (*xrpc.WarpInstallResult, error)
	CallWarpAuthorize(ctx context.Context, serverID int64, params xrpc.WarpAuthorizeParams) (*xrpc.WarpAuthorizeResult, error)
	CallWarpRemove(ctx context.Context, serverID int64) (*xrpc.WarpRemoveResult, error)
	CallWarpCredentials(ctx context.Context, serverID int64) (*xrpc.WarpCredentialsResult, error)
}

// PlanCreator 是复用 service/apply 计划校验与持久化能力的最小接口。
type PlanCreator interface {
	CreatePlanFromJSON(ctx context.Context, specJSON, batchKind, batchID string) (any, error)
}

// JobStarter 启动关联服务器的长任务（由 service/jobs.StartForServer 注入）。
type JobStarter func(ctx context.Context, serverID int64, inv *command.Invocation, run func(context.Context) (any, error), done func()) (any, error)

// Deps 聚合网络探测、WARP 与返程路由服务所需依赖。
type Deps struct {
	Servers      *coreservers.Repo
	Inbounds     *coreinbounds.Repo
	Outbounds    *coreoutbounds.Repo
	RoutingRules *corerouting.Repo
	ReturnRoutes *corereturnroutes.Repo
	Nodes        NodeClient
	Planner      PlanCreator
	StartJob     JobStarter
}

// Service 是节点网络探测、WARP 与返程路由服务。
type Service struct {
	deps Deps
}

// New 构造服务实例。
func New(deps Deps) *Service {
	return &Service{deps: deps}
}

// Bindings 返回全部 11 条管理员专属命令绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"server scan":               s.serverScan,
		"website validate":          s.websiteValidate,
		"server domain-latency":     s.serverDomainLatency,
		"server warp status":        s.warpStatus,
		"server warp install":       s.warpInstall,
		"server warp authorize":     s.warpAuthorize,
		"server warp remove":        s.warpRemove,
		"server warp outbound-plan": s.warpOutboundPlan,
		"return-route list":         s.returnRouteList,
		"return-route get":          s.returnRouteGet,
		"return-route test":         s.returnRouteTest,
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "网络探测、WARP 与返程路由命令只对管理员开放")
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
	return nil, v1.Newf(v1.CodeNotFound, "没有叫 %q 的服务器", ref)
}

func pageLimitOffset(inv *command.Invocation) (int, int, error) {
	limit := 50
	offset := 0
	if inv != nil && inv.Page != nil {
		if inv.Page.Limit > 0 {
			limit = inv.Page.Limit
		}
		if limit > 200 {
			limit = 200
		}
		if c := strings.TrimSpace(inv.Page.Cursor); c != "" {
			n, err := strconv.Atoi(c)
			if err != nil || n < 0 {
				return 0, 0, v1.Newf(v1.CodeBadRequest, "cursor %q 不合法", c)
			}
			offset = n
		}
	}
	return limit, offset, nil
}

func nextOffsetCursor(offset, count, total int) string {
	next := offset + count
	if next >= total || count == 0 {
		return ""
	}
	return strconv.Itoa(next)
}

func validateHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", v1.New(v1.CodeBadRequest, "目标主机或域名不能为空")
	}
	if len(h) > 253 {
		return "", v1.Newf(v1.CodeBadRequest, "目标主机 %q 过长", h)
	}
	for _, r := range h {
		if r <= 0x20 || r == 0x7f || r == '/' || r == '\\' || r == '?' || r == '#' {
			return "", v1.Newf(v1.CodeBadRequest, "目标主机 %q 含非法字符", h)
		}
	}
	if strings.Contains(h, "..") {
		return "", v1.Newf(v1.CodeBadRequest, "目标主机 %q 不能包含 ..", h)
	}
	return h, nil
}
