package network

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/command"
	corereturnroutes "github.com/satchel/satchel/internal/core/returnroutes"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// ReturnRouteTestItem 描述单条返程路由在测试任务中的实测结果。
type ReturnRouteTestItem struct {
	ID        int64                 `json:"id"`
	Carrier   string                `json:"carrier"`
	TargetIP  string                `json:"target_ip,omitempty"`
	RouteType string                `json:"route_type"`
	Region    string                `json:"region,omitempty"`
	EntryIP   string                `json:"entry_ip,omitempty"`
	EntryASN  string                `json:"entry_asn,omitempty"`
	Reason    string                `json:"reason,omitempty"`
	Hops      []xrpc.ReturnRouteHop `json:"hops,omitempty"`
	TestedAt  time.Time             `json:"tested_at"`
}

// ReturnRouteTestView 是 return-route test 长任务的终态输出。
type ReturnRouteTestView struct {
	Server   string                `json:"server"`
	ServerID int64                 `json:"server_id"`
	Total    int                   `json:"total"`
	Results  []ReturnRouteTestItem `json:"results"`
}

func (s *Service) returnRouteList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.ReturnRoutes == nil {
		return nil, v1.New(v1.CodeInternal, "ReturnRoute 仓储未装配")
	}
	limit, offset, err := pageLimitOffset(inv)
	if err != nil {
		return nil, err
	}

	var f corereturnroutes.ListFilter
	if srvRef := strings.TrimSpace(inv.String("server-name", "")); srvRef != "" {
		srv, serr := s.resolveServer(ctx, srvRef)
		if serr != nil {
			return nil, serr
		}
		f.ServerID = srv.Metadata.ID
	}
	if carrier := strings.TrimSpace(inv.String("carrier", "")); carrier != "" {
		norm, cerr := corereturnroutes.NormalizeCarrier(carrier)
		if cerr != nil {
			return nil, cerr
		}
		f.Carrier = norm
	}
	if inv != nil && inv.Flags != nil {
		if _, hasEnabled := inv.Flags["enabled"]; hasEnabled {
			en := inv.Bool("enabled")
			f.Enabled = &en
		}
	}
	f.IncludeDeleted = inv.Bool("include-deleted")

	rows, total, err := s.deps.ReturnRoutes.List(ctx, f, limit, offset)
	if err != nil {
		return nil, err
	}
	items := make([]any, len(rows))
	for i := range rows {
		items[i] = rows[i]
	}
	return &command.PageResult{
		Items:      items,
		Total:      total,
		NextCursor: nextOffsetCursor(offset, len(rows), total),
	}, nil
}

func (s *Service) returnRouteGet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.ReturnRoutes == nil {
		return nil, v1.New(v1.CodeInternal, "ReturnRoute 仓储未装配")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(inv.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "返程路由 ID %q 必须是正整数", inv.Arg(0))
	}
	rr, err := s.deps.ReturnRoutes.ByID(ctx, id)
	if errors.Is(err, corereturnroutes.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "没有 id 为 %d 的 ReturnRoute", id)
	}
	if err != nil {
		return nil, err
	}
	return rr, nil
}

func (s *Service) returnRouteTest(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.ReturnRoutes == nil || s.deps.Nodes == nil || s.deps.StartJob == nil {
		return nil, v1.New(v1.CodeInternal, "返程路由测路所需服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}

	carrierFilter := strings.TrimSpace(inv.String("carrier", ""))
	var targets []*corereturnroutes.ReturnRoute
	if carrierFilter != "" {
		normCarrier, cerr := corereturnroutes.NormalizeCarrier(carrierFilter)
		if cerr != nil {
			return nil, cerr
		}
		rr, ferr := s.deps.ReturnRoutes.ByServerCarrierIncludeDeleted(ctx, srv.Metadata.ID, normCarrier)
		if errors.Is(ferr, corereturnroutes.ErrNotFound) || (ferr == nil && rr.Metadata.DeletedAt != nil) {
			return nil, v1.Newf(v1.CodeNotFound, "服务器 %q 上没有活跃的返程路由 %q", srv.Spec.Name, normCarrier)
		}
		if ferr != nil {
			return nil, ferr
		}
		targets = []*corereturnroutes.ReturnRoute{rr}
	} else {
		all, lerr := s.deps.ReturnRoutes.ListByServer(ctx, srv.Metadata.ID)
		if lerr != nil {
			return nil, lerr
		}
		for _, rr := range all {
			if rr.Spec.Enabled {
				targets = append(targets, rr)
			}
		}
		if len(targets) == 0 {
			return nil, v1.Newf(v1.CodeBadRequest, "服务器 %q 上没有已启用的 ReturnRoute（请先通过 plan / apply 配置，或显式传 --carrier 指定单条线路）", srv.Spec.Name)
		}
	}

	return s.deps.StartJob(ctx, srv.Metadata.ID, inv, func(jobCtx context.Context) (any, error) {
		results := make([]ReturnRouteTestItem, 0, len(targets))
		for _, rr := range targets {
			probeCfg, _, _ := corereturnroutes.ParseProbe(rr.Spec.Probe)
			rpcRes, cerr := s.deps.Nodes.CallNetworkReturnRouteTest(jobCtx, srv.Metadata.ID, xrpc.NetworkReturnRouteTestParams{
				Carrier:   rr.Spec.Carrier,
				TargetIP:  probeCfg.TargetIP,
				Port:      probeCfg.Port,
				MaxHops:   probeCfg.MaxHops,
				TimeoutMS: probeCfg.TimeoutMS,
			})
			now := time.Now().UTC()
			if cerr != nil {
				// 节点离线/不可达直接失败；单条目标或探测层错误则回写 Unknown 并继续其余线路。
				if ae := v1.AsError(cerr); ae != nil && (ae.Code == v1.CodeUnavailable || ae.Code == v1.CodeConflict || ae.Code == v1.CodeNotFound) {
					return nil, cerr
				}
				patch := corereturnroutes.ProbeStatusPatch{
					RouteType: "Unknown",
					Reason:    cerr.Error(),
					TestedAt:  now,
				}
				_ = s.deps.ReturnRoutes.UpdateProbeStatus(jobCtx, rr.Metadata.ID, patch)
				results = append(results, ReturnRouteTestItem{
					ID:        rr.Metadata.ID,
					Carrier:   rr.Spec.Carrier,
					TargetIP:  probeCfg.TargetIP,
					RouteType: "Unknown",
					Reason:    cerr.Error(),
					TestedAt:  now,
				})
				continue
			}
			testedAt := rpcRes.TestedAt
			if testedAt.IsZero() {
				testedAt = now
			}
			patch := corereturnroutes.ProbeStatusPatch{
				RouteType: rpcRes.RouteType,
				Region:    rpcRes.Region,
				EntryIP:   rpcRes.EntryIP,
				EntryASN:  rpcRes.EntryASN,
				Reason:    rpcRes.Reason,
				TestedAt:  testedAt,
			}
			if uerr := s.deps.ReturnRoutes.UpdateProbeStatus(jobCtx, rr.Metadata.ID, patch); uerr != nil {
				return nil, uerr
			}
			results = append(results, ReturnRouteTestItem{
				ID:        rr.Metadata.ID,
				Carrier:   rr.Spec.Carrier,
				TargetIP:  rpcRes.TargetIP,
				RouteType: rpcRes.RouteType,
				Region:    rpcRes.Region,
				EntryIP:   rpcRes.EntryIP,
				EntryASN:  rpcRes.EntryASN,
				Reason:    rpcRes.Reason,
				Hops:      rpcRes.Hops,
				TestedAt:  testedAt,
			})
		}
		return &ReturnRouteTestView{
			Server:   srv.Spec.Name,
			ServerID: srv.Metadata.ID,
			Total:    len(results),
			Results:  results,
		}, nil
	}, nil)
}
