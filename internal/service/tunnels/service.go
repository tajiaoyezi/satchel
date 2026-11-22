// Package tunnels 是业务层的偷取模式、任意门单跳/复用隧道与 N 台链式隧道编排服务（node-steal-and-tunnels）：
// 零新增数据库表，基于既有 Server / Inbound / RoutingRule 仓储与 PlanCreator 接口提供全部 9 条管理员专属命令。
// 严守六层架构单向依赖纪律：仅依赖 internal/core/*、internal/base/* 与 internal/command，不 import 其它 service/*。
package tunnels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"unicode"

	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// PlanCreator 是复用 service/apply 计划校验与持久化能力的最小接口，避免 service/* 跨包引用。
type PlanCreator interface {
	CreatePlanFromJSON(ctx context.Context, specJSON, batchKind, batchID string) (any, error)
}

// Deps 聚合偷取模式与隧道编排服务所需的仓储与接口。
type Deps struct {
	Servers      *coreservers.Repo
	Inbounds     *coreinbounds.Repo
	RoutingRules *corerouting.Repo
	Websites     *corewebsites.Repo
	Settings     *coresettings.Repo
	Planner      PlanCreator
}

// Service 是偷取模式与隧道管理服务。
type Service struct {
	deps Deps
}

// New 构造服务实例。
func New(deps Deps) *Service {
	return &Service{deps: deps}
}

// Bindings 返回全部 9 条管理员专属命令绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"server steal status":      s.stealStatus,
		"server steal plan":        s.stealPlan,
		"tunnel list":              s.tunnelList,
		"tunnel plan":              s.tunnelPlan,
		"tunnel multiplex plan":    s.tunnelMultiplexPlan,
		"tunnel delete-plan":       s.tunnelDeletePlan,
		"tunnel chain list":        s.tunnelChainList,
		"tunnel chain plan":        s.tunnelChainPlan,
		"tunnel chain delete-plan": s.tunnelChainDeletePlan,
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "偷取模式与隧道编排命令只对管理员开放")
	}
	return nil
}

func (s *Service) masterURL(ctx context.Context) string {
	if s.deps.Settings == nil {
		return ""
	}
	st, err := s.deps.Settings.Load(ctx)
	if err != nil || st == nil {
		return ""
	}
	return st.Gates().MasterURL
}

func (s *Service) isSameHost443Protected(ctx context.Context, srv *coreservers.Server, overrideDomain string) bool {
	dom := srv.Spec.Domain
	if strings.TrimSpace(overrideDomain) != "" {
		dom = overrideDomain
	}
	return nginx.IsSameHost443Protected(srv.Status.SameHostAsMaster, srv.Status.Status, dom, s.masterURL(ctx))
}

func (s *Service) resolveServer(ctx context.Context, ref string) (*coreservers.Server, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, v1.New(v1.CodeBadRequest, "缺少目标服务器名或 ID")
	}
	if id, err := strconv.ParseInt(trimmed, 10, 64); err == nil && id > 0 {
		srv, err := s.deps.Servers.ByID(ctx, id)
		if err == nil {
			return srv, nil
		}
		if !errors.Is(err, coreservers.ErrNotFound) {
			return nil, err
		}
	}
	srv, err := s.deps.Servers.ByName(ctx, trimmed)
	if errors.Is(err, coreservers.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 %q 的服务器", trimmed)
	}
	return srv, err
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// serverEntryHost 返回服务器用于隧道上游连接的首选可达地址（IPv4 > domain > pull_address > IPv6 > domain_v6 > pull_address_v6）。
func serverEntryHost(srv *coreservers.Server) string {
	for _, cand := range []string{
		derefStr(srv.Status.IPAddress),
		srv.Spec.Domain,
		srv.Spec.PullAddress,
		srv.Status.IPAddressV6,
		srv.Spec.DomainV6,
		srv.Spec.PullAddressV6,
	} {
		if h := strings.TrimSpace(cand); h != "" {
			return h
		}
	}
	return ""
}

type resolvedTarget struct {
	Address          string
	Port             int
	DefaultNetwork   string
	TargetServerID   int64
	TargetServerName string
	TargetInboundTag string
}

// resolveTunnelTarget 解析「固定目标 (--target-address + --target-port)」或「已有服务器入站 (--target-server + --target-inbound)」二选一目标。
func (s *Service) resolveTunnelTarget(ctx context.Context, inv *command.Invocation) (resolvedTarget, error) {
	addr := strings.TrimSpace(inv.String("target-address", ""))
	port := inv.Int("target-port", 0)
	tgtSrvRef := strings.TrimSpace(inv.String("target-server", ""))
	tgtInTag := strings.TrimSpace(inv.String("target-inbound", ""))

	hasFixed := addr != "" || port != 0
	hasRef := tgtSrvRef != "" || tgtInTag != ""
	if hasFixed && hasRef {
		return resolvedTarget{}, v1.New(v1.CodeBadRequest, "--target-address/--target-port 与 --target-server/--target-inbound 只能二选一")
	}
	if !hasFixed && !hasRef {
		return resolvedTarget{}, v1.New(v1.CodeBadRequest, "必须指定转发目标：--target-address + --target-port 或 --target-server + --target-inbound")
	}

	if hasFixed {
		if addr == "" {
			return resolvedTarget{}, v1.New(v1.CodeBadRequest, "指定 --target-port 时必须同时提供 --target-address")
		}
		for _, r := range addr {
			if unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
				return resolvedTarget{}, v1.Newf(v1.CodeBadRequest, "--target-address %q 不能包含空白或控制字符", addr)
			}
		}
		if port < 1 || port > 65535 {
			return resolvedTarget{}, v1.Newf(v1.CodeBadRequest, "--target-port %d 必须在 1..65535 之间", port)
		}
		return resolvedTarget{
			Address:        addr,
			Port:           port,
			DefaultNetwork: "tcp,udp",
		}, nil
	}

	if tgtSrvRef == "" || tgtInTag == "" {
		return resolvedTarget{}, v1.New(v1.CodeBadRequest, "--target-server 与 --target-inbound 必须成对提供")
	}
	tgtSrv, err := s.resolveServer(ctx, tgtSrvRef)
	if err != nil {
		return resolvedTarget{}, err
	}
	host := serverEntryHost(tgtSrv)
	if host == "" {
		return resolvedTarget{}, v1.Newf(v1.CodeBadRequest, "目标服务器 %s 尚未配置任何可达地址（ip_address / domain / pull_address 均为空）", tgtSrv.Spec.Name)
	}
	tgtIn, err := s.deps.Inbounds.ByServerTag(ctx, tgtSrv.Metadata.ID, tgtInTag)
	if errors.Is(err, coreinbounds.ErrNotFound) {
		return resolvedTarget{}, v1.Newf(v1.CodeNotFound, "目标服务器 %s 下没有名为 %q 的活跃入站", tgtSrv.Spec.Name, tgtInTag)
	}
	if err != nil {
		return resolvedTarget{}, err
	}
	return resolvedTarget{
		Address:          host,
		Port:             int(tgtIn.Spec.Port),
		DefaultNetwork:   singbox.DefaultDirectNetworkForProtocol(tgtIn.Spec.Protocol),
		TargetServerID:   tgtSrv.Metadata.ID,
		TargetServerName: tgtSrv.Spec.Name,
		TargetInboundTag: tgtIn.Spec.Tag,
	}, nil
}

// checkNoSelfLoop 防止单跳或出口跳将流量转发回本机同一监听端口导致自环死循环。
func checkNoSelfLoop(entrySrv *coreservers.Server, listenPort int, tgt resolvedTarget) error {
	if tgt.Port != listenPort {
		return nil
	}
	if tgt.TargetServerID > 0 && tgt.TargetServerID == entrySrv.Metadata.ID {
		return v1.Newf(v1.CodeBadRequest, "禁止自环转发：入口服务器 %s 端口 %d 不能转发至本机同一端口", entrySrv.Spec.Name, listenPort)
	}
	normAddr := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(tgt.Address), "."))
	if normAddr == "localhost" {
		return v1.Newf(v1.CodeBadRequest, "禁止自环转发：入口端口 %d 不能指向本机 %s:%d", listenPort, tgt.Address, tgt.Port)
	}
	if ip := net.ParseIP(normAddr); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return v1.Newf(v1.CodeBadRequest, "禁止自环转发：入口端口 %d 不能指向本机 %s:%d", listenPort, tgt.Address, tgt.Port)
	}
	for _, selfAddr := range []string{
		derefStr(entrySrv.Status.IPAddress),
		entrySrv.Status.IPAddressV6,
		entrySrv.Spec.Domain,
		entrySrv.Spec.DomainV6,
		entrySrv.Spec.PullAddress,
		entrySrv.Spec.PullAddressV6,
	} {
		if s := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(selfAddr), ".")); s != "" && s == normAddr {
			return v1.Newf(v1.CodeBadRequest, "禁止自环转发：服务器 %s 端口 %d 不能指向自身地址 %s:%d", entrySrv.Spec.Name, listenPort, tgt.Address, tgt.Port)
		}
	}
	return nil
}

type parsedOverrideRule struct {
	Inbounds        []string
	OverrideAddress string
	OverridePort    int
	Domains         []string
	DomainSuffixes  []string
	IPCIDRs         []string
	SourceIPCIDRs   []string
	OtherCondCount  int
	HasOverride     bool
}

func parseOverrideRule(raw json.RawMessage) parsedOverrideRule {
	var out parsedOverrideRule
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return out
	}
	out.Inbounds = toStringSlice(obj["inbound"])
	if addr, ok := obj["override_address"].(string); ok {
		out.OverrideAddress = strings.TrimSpace(addr)
	}
	if rawPort, ok := obj["override_port"]; ok {
		switch p := rawPort.(type) {
		case json.Number:
			if n, err := strconv.Atoi(strings.TrimSpace(string(p))); err == nil {
				out.OverridePort = n
			}
		case float64:
			out.OverridePort = int(p)
		}
	}
	out.HasOverride = out.OverrideAddress != "" && out.OverridePort > 0
	out.Domains = toStringSlice(obj["domain"])
	out.DomainSuffixes = toStringSlice(obj["domain_suffix"])
	out.IPCIDRs = toStringSlice(obj["ip_cidr"])
	out.SourceIPCIDRs = toStringSlice(obj["source_ip_cidr"])
	for k := range obj {
		switch k {
		case "inbound", "action", "rule_set_defs", "override_address", "override_port", "outbound":
			continue
		default:
			out.OtherCondCount++
		}
	}
	return out
}

func ruleOnlyBindsTag(pr parsedOverrideRule, tag string) bool {
	return len(pr.Inbounds) == 1 && pr.Inbounds[0] == tag
}

func ruleIncludesTag(pr parsedOverrideRule, tag string) bool {
	for _, t := range pr.Inbounds {
		if t == tag {
			return true
		}
	}
	return false
}

func toStringSlice(v any) []string {
	switch x := v.(type) {
	case string:
		if s := strings.TrimSpace(x); s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if t := strings.TrimSpace(s); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

func splitStringsFlag(inv *command.Invocation, name string) []string {
	if inv == nil || inv.Flags == nil {
		return nil
	}
	v, ok := inv.Flags[name]
	if !ok || v == nil {
		return nil
	}
	var raw []string
	switch x := v.(type) {
	case []string:
		raw = x
	case string:
		raw = []string{x}
	case []any:
		for _, item := range x {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	var out []string
	for _, entry := range raw {
		for _, part := range strings.Split(entry, ",") {
			if t := strings.TrimSpace(part); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func flagProvided(inv *command.Invocation, name string) bool {
	if inv == nil || inv.Flags == nil {
		return false
	}
	_, ok := inv.Flags[name]
	return ok
}

func pageLimitOffset(inv *command.Invocation) (limit, offset int, err error) {
	limit = 50
	if inv.Page != nil {
		if inv.Page.Limit > 0 {
			limit = inv.Page.Limit
		}
		if cur := strings.TrimSpace(inv.Page.Cursor); cur != "" {
			n, perr := strconv.Atoi(cur)
			if perr != nil || n < 0 {
				return 0, 0, v1.Newf(v1.CodeBadRequest, "无效的分页游标 %q", cur)
			}
			offset = n
		}
	}
	return limit, offset, nil
}

func nextOffsetCursor(offset, count, total int) string {
	if offset+count < total {
		return strconv.Itoa(offset + count)
	}
	return ""
}

func activeUsedPorts(ibs []*coreinbounds.Inbound, includeSteal8001 bool) map[int]bool {
	used := make(map[int]bool, len(ibs)+1)
	for _, ib := range ibs {
		if ib.Spec.Enabled {
			used[int(ib.Spec.Port)] = true
		}
	}
	if includeSteal8001 {
		used[nginx.StealListenPort] = true
	}
	return used
}
