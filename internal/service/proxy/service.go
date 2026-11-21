// Package proxy 是业务层的代理资源与路由管理服务（node-proxy-resources）：
// 提供 Inbound / Outbound / RoutingRule 的只读查询、协议矩阵与示例库、密钥生成、无状态入站向导、
// 实时出站探测、整节点落地规划、路由预设与多机批量规划共 20 条管理员专属命令。
// 所有配置写操作均经由 Planner 接口交由 service/apply 统一走 plan / apply 唯一写路径，严守分层纪律。
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/command"
	corebatchin "github.com/satchel/satchel/internal/core/batchinbounds"
	corebatchout "github.com/satchel/satchel/internal/core/batchoutbounds"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	corepresets "github.com/satchel/satchel/internal/core/routingpresets"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	coreusers "github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// PlanCreator 是复用 service/apply 计划生成能力的最小接口，避免 service/* 之间互相 import。
type PlanCreator interface {
	CreatePlanFromJSON(ctx context.Context, specJSON, batchKind, batchID string) (any, error)
}

// NodeProber 是调用节点 outbound.probe 管理 RPC 的最小接口（由 service/servers 实现）。
type NodeProber interface {
	CallOutboundProbe(ctx context.Context, serverID int64, params xrpc.OutboundProbeParams) (*xrpc.OutboundProbeResult, error)
}

// Deps 聚合代理资源服务所需仓储与接口依赖。
type Deps struct {
	Servers        *coreservers.Repo
	Inbounds       *coreinbounds.Repo
	Outbounds      *coreoutbounds.Repo
	RoutingRules   *corerouting.Repo
	BatchInbounds  *corebatchin.Repo
	BatchOutbounds *corebatchout.Repo
	RoutingPresets *corepresets.Repo
	Users          *coreusers.Repo
	Settings       *coresettings.Repo
	Planner        PlanCreator
	Nodes          NodeProber
	Now            func() time.Time
}

// Service 是代理资源与路由管理服务。
type Service struct {
	deps Deps
}

// New 建服务。
func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{deps: deps}
}

// Bindings 返回全部 20 条管理员专属命令绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"inbound list":           s.inboundList,
		"inbound get":            s.inboundGet,
		"inbound protocols":      s.inboundProtocols,
		"inbound examples":       s.inboundExamples,
		"inbound keygen":         s.inboundKeygen,
		"inbound build":          s.inboundBuild,
		"inbound batch list":     s.inboundBatchList,
		"inbound batch plan":     s.inboundBatchPlan,
		"outbound list":          s.outboundList,
		"outbound get":           s.outboundGet,
		"outbound probe":         s.outboundProbe,
		"outbound landing plan":  s.outboundLandingPlan,
		"outbound batch list":    s.outboundBatchList,
		"outbound batch plan":    s.outboundBatchPlan,
		"routing list":           s.routingList,
		"routing get":            s.routingGet,
		"routing preset builtin": s.routingPresetBuiltin,
		"routing preset list":    s.routingPresetList,
		"routing preset save":    s.routingPresetSave,
		"routing preset delete":  s.routingPresetDelete,
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "代理资源与路由命令只对管理员开放")
	}
	return nil
}

func (s *Service) now() time.Time {
	return s.deps.Now().UTC().Truncate(time.Microsecond)
}

// resolveServer 支持按服务器名或正整数 ID 查找未软删除服务器。
func (s *Service) resolveServer(ctx context.Context, ref string) (*coreservers.Server, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, v1.New(v1.CodeBadRequest, "缺少服务器名或 ID（--server-name）")
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

// resolveTargetServers 解析 --servers（逗号或可重复列表）与 --all 二选一，返回按 ID 升序去重的目标服务器列表。
func (s *Service) resolveTargetServers(ctx context.Context, rawList []string, all bool) ([]*coreservers.Server, error) {
	var names []string
	for _, entry := range rawList {
		for _, part := range strings.Split(entry, ",") {
			if t := strings.TrimSpace(part); t != "" {
				names = append(names, t)
			}
		}
	}
	if all && len(names) > 0 {
		return nil, v1.New(v1.CodeBadRequest, "--servers 与 --all 不能同时指定")
	}
	if !all && len(names) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "必须指定 --servers 或 --all 选择目标服务器")
	}
	if all {
		srvs, err := s.deps.Servers.ListAll(ctx)
		if err != nil {
			return nil, err
		}
		if len(srvs) == 0 {
			return nil, v1.New(v1.CodeConflict, "当前主控没有任何可用服务器")
		}
		sort.Slice(srvs, func(i, j int) bool { return srvs[i].Metadata.ID < srvs[j].Metadata.ID })
		return srvs, nil
	}
	seen := map[int64]bool{}
	out := make([]*coreservers.Server, 0, len(names))
	for _, ref := range names {
		srv, err := s.resolveServer(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !seen[srv.Metadata.ID] {
			seen[srv.Metadata.ID] = true
			out = append(out, srv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.ID < out[j].Metadata.ID })
	return out, nil
}

// resolvePresetOwner 为 routing_rule_presets 解析满足 users.username 外键约束的归属账号。
func (s *Service) resolvePresetOwner(ctx context.Context, explicitOwner string) (string, error) {
	id := v1.IdentityFrom(ctx)
	if owner := strings.TrimSpace(explicitOwner); owner != "" {
		acc, err := s.deps.Users.GetByUsername(ctx, owner)
		if errors.Is(err, coreusers.ErrNotFound) || (err == nil && acc.Deleted) {
			return "", v1.Newf(v1.CodeNotFound, "账号 %q 不存在", owner)
		}
		if err != nil {
			return "", err
		}
		return acc.Username, nil
	}
	switch id.ActorKind {
	case v1.ActorUser, v1.ActorToken:
		return id.Actor, nil
	case v1.ActorLocalAdmin:
		if s.deps.Settings != nil {
			if st, err := s.deps.Settings.Load(ctx); err == nil {
				if primary, _ := st.Values["primary_admin_username"].(string); strings.TrimSpace(primary) != "" {
					if acc, err := s.deps.Users.GetByUsername(ctx, strings.TrimSpace(primary)); err == nil && !acc.Deleted {
						return acc.Username, nil
					}
				}
			}
		}
		if s.deps.Users != nil {
			if acc, err := s.deps.Users.FirstActiveAdmin(ctx); err == nil {
				return acc.Username, nil
			}
		}
		return "", v1.New(v1.CodeConflict, "尚未初始化任何管理员账号，无法保存归属于用户的路由预设").
			WithNext("请先执行 satchel setup init 初始化管理员账号，或通过 --owner 指定已有账号")
	default:
		return "", v1.New(v1.CodeForbidden, "当前身份无法管理路由预设")
	}
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

func parsePositiveID(raw, label string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, v1.Newf(v1.CodeBadRequest, "%s id 必须是正整数，得到 %q", label, raw)
	}
	return id, nil
}

func newBatchID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "batch-" + hex.EncodeToString(b[:])
}

func stringSliceFlag(inv *command.Invocation, name string) []string {
	if inv == nil || inv.Flags == nil {
		return nil
	}
	v, ok := inv.Flags[name]
	if !ok || v == nil {
		return nil
	}
	switch x := v.(type) {
	case []string:
		return x
	case string:
		if strings.TrimSpace(x) != "" {
			return []string{x}
		}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
