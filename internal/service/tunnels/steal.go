package tunnels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// StealInboundSummary 是 server steal status 展示的活跃入站绑定摘要。
type StealInboundSummary struct {
	ID        int64  `json:"id"`
	Tag       string `json:"tag"`
	Protocol  string `json:"protocol"`
	Port      int64  `json:"port"`
	Listen    string `json:"listen"`
	Transport string `json:"transport"`
	Security  string `json:"security"`
	SNI       string `json:"sni,omitempty"`
	Handshake string `json:"handshake,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// StealRuleSummary 是 server steal status 展示的 tunnel-in 绑定路由规则摘要。
type StealRuleSummary struct {
	ID              int64    `json:"id"`
	SortOrder       int64    `json:"sort_order"`
	Domains         []string `json:"domains,omitempty"`
	OverrideAddress string   `json:"override_address,omitempty"`
	OverridePort    int      `json:"override_port,omitempty"`
	OutboundTag     string   `json:"outbound_tag"`
	Enabled         bool     `json:"enabled"`
}

// StealNginxPreview 是 server steal status 在 use_443=true 时展示的偷自己本地 nginx 预览。
type StealNginxPreview struct {
	ConfPath string `json:"conf_path"`
	Conf     string `json:"conf"`
}

// StealStatusView 是 server steal status 的响应结构。
type StealStatusView struct {
	ServerID          int64                 `json:"server_id"`
	ServerName        string                `json:"server_name"`
	SameHostAsMaster  bool                  `json:"same_host_as_master"`
	ProtectMaster443  bool                  `json:"protect_master_443"`
	Use443            bool                  `json:"use_443"`
	StealMode         string                `json:"steal_mode"`
	Domain            string                `json:"domain"`
	SiteType          string                `json:"site_type"`
	SiteValue         string                `json:"site_value"`
	PortRangeMin      int64                 `json:"port_range_min"`
	PortRangeMax      int64                 `json:"port_range_max"`
	TunnelInbound     *StealInboundSummary  `json:"tunnel_inbound,omitempty"`
	TunnelRules       []StealRuleSummary    `json:"tunnel_rules"`
	Inbounds          []StealInboundSummary `json:"inbounds"`
	NextFreePort      int                   `json:"next_free_port,omitempty"`
	StealNginxPreview *StealNginxPreview    `json:"steal_nginx_preview,omitempty"`
}

func (s *Service) stealStatus(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	ibs, err := s.deps.Inbounds.ListByServer(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	rrs, err := s.deps.RoutingRules.ListByServer(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}

	protect443 := s.isSameHost443Protected(ctx, srv, "")
	var tunnelIn *StealInboundSummary
	summaries := make([]StealInboundSummary, 0, len(ibs))
	inSpecs := make([]v1.InboundSpec, 0, len(ibs))
	for _, ib := range ibs {
		inSpecs = append(inSpecs, ib.Spec)
		sum := summarizeInbound(ib)
		summaries = append(summaries, sum)
		if ib.Spec.Tag == singbox.StealTunnelInboundTag {
			cp := sum
			tunnelIn = &cp
		}
	}

	tunnelRules := make([]StealRuleSummary, 0)
	for _, rr := range rrs {
		pr := parseOverrideRule(rr.Spec.Match)
		if !ruleIncludesTag(pr, singbox.StealTunnelInboundTag) {
			continue
		}
		tunnelRules = append(tunnelRules, StealRuleSummary{
			ID:              rr.Metadata.ID,
			SortOrder:       rr.Spec.SortOrder,
			Domains:         append(append([]string(nil), pr.Domains...), pr.DomainSuffixes...),
			OverrideAddress: pr.OverrideAddress,
			OverridePort:    pr.OverridePort,
			OutboundTag:     rr.Spec.OutboundTag,
			Enabled:         rr.Spec.Enabled,
		})
	}
	sort.Slice(tunnelRules, func(i, j int) bool {
		if tunnelRules[i].SortOrder != tunnelRules[j].SortOrder {
			return tunnelRules[i].SortOrder < tunnelRules[j].SortOrder
		}
		return tunnelRules[i].ID < tunnelRules[j].ID
	})

	used := activeUsedPorts(ibs, srv.Spec.Use443)
	nextPort, _ := singbox.NextFreePort(used, int(srv.Spec.PortRangeMin), int(srv.Spec.PortRangeMax), protect443)

	var preview *StealNginxPreview
	if srv.Spec.Use443 && !protect443 {
		if rs, rerr := nginx.RenderStealSite(nginx.StealRenderInput{
			ServerID:     srv.Metadata.ID,
			Domain:       srv.Spec.Domain,
			StealMode:    srv.Spec.StealMode,
			SiteType:     srv.Spec.SiteType,
			SiteValue:    srv.Spec.SiteValue,
			Ipv6Enabled:  srv.Spec.Ipv6Enabled,
			SameHost:     false,
			WSSLocations: nginx.ExtractWSSLocations(inSpecs),
		}); rerr == nil {
			preview = &StealNginxPreview{
				ConfPath: rs.ConfPath,
				Conf:     rs.Conf,
			}
		}
	}

	return &StealStatusView{
		ServerID:          srv.Metadata.ID,
		ServerName:        srv.Spec.Name,
		SameHostAsMaster:  srv.Status.SameHostAsMaster,
		ProtectMaster443:  protect443,
		Use443:            srv.Spec.Use443,
		StealMode:         srv.Spec.StealMode,
		Domain:            srv.Spec.Domain,
		SiteType:          srv.Spec.SiteType,
		SiteValue:         srv.Spec.SiteValue,
		PortRangeMin:      srv.Spec.PortRangeMin,
		PortRangeMax:      srv.Spec.PortRangeMax,
		TunnelInbound:     tunnelIn,
		TunnelRules:       tunnelRules,
		Inbounds:          summaries,
		NextFreePort:      nextPort,
		StealNginxPreview: preview,
	}, nil
}

func (s *Service) stealPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	mode := strings.ToLower(strings.TrimSpace(inv.String("mode", "")))
	switch mode {
	case "tunnel", "fallback", "off":
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "--mode %q 不合法（必须为 tunnel、fallback 或 off）", inv.String("mode", ""))
	}

	ibs, err := s.deps.Inbounds.ListByServer(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	rrs, err := s.deps.RoutingRules.ListByServer(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}

	// 1. 关闭偷自己模式：将 use_443 置为 false 并清理 tunnel-in 及其绑定规则。
	if mode == "off" {
		ops := []map[string]any{
			{
				"op":              "update",
				"kind":            coreservers.KindName,
				"id":              srv.Metadata.ID,
				"resourceVersion": srv.Metadata.ResourceVersion,
				"spec":            map[string]any{"use_443": false},
			},
		}
		for _, ib := range ibs {
			if ib.Spec.Tag == singbox.StealTunnelInboundTag {
				ops = append(ops, map[string]any{
					"op":              "delete",
					"kind":            coreinbounds.KindName,
					"id":              ib.Metadata.ID,
					"resourceVersion": ib.Metadata.ResourceVersion,
				})
			}
		}
		for _, rr := range rrs {
			if ruleOnlyBindsTag(parseOverrideRule(rr.Spec.Match), singbox.StealTunnelInboundTag) {
				ops = append(ops, map[string]any{
					"op":              "delete",
					"kind":            corerouting.KindName,
					"id":              rr.Metadata.ID,
					"resourceVersion": rr.Metadata.ResourceVersion,
				})
			}
		}
		return s.submitPlanOps(ctx, ops)
	}

	// 2. tunnel / fallback 模式公共前置校验。
	rawDomain := strings.TrimSpace(inv.String("domain", ""))
	if rawDomain == "" {
		rawDomain = strings.TrimSpace(srv.Spec.Domain)
	}
	if s.isSameHost443Protected(ctx, srv, rawDomain) {
		return nil, v1.New(v1.CodeConflict, "主控同机节点禁止开启 443 偷取模式（防止抢占主控 HTTPS 443 端口）")
	}
	normDomain, err := nginx.ValidateDomain(rawDomain)
	if err != nil {
		return nil, v1.New(v1.CodeBadRequest, "开启偷取模式必须为服务器配置合法非空域名（可通过 --domain 传入）："+v1.AsError(err).Reason)
	}

	siteType := strings.ToLower(strings.TrimSpace(inv.String("site-type", "")))
	if siteType == "" {
		siteType = strings.ToLower(strings.TrimSpace(srv.Spec.SiteType))
	}
	if siteType == "" {
		siteType = nginx.TypeDecoy
	}
	siteValue := strings.TrimSpace(inv.String("site-value", ""))
	if !flagProvided(inv, "site-value") && siteValue == "" {
		siteValue = strings.TrimSpace(srv.Spec.SiteValue)
	}
	if siteType == nginx.TypeDecoy && siteValue == "" {
		siteValue = "landing"
	}
	if err := nginx.ValidateStealInput(nginx.StealRenderInput{
		ServerID:  srv.Metadata.ID,
		Domain:    normDomain,
		StealMode: mode,
		SiteType:  siteType,
		SiteValue: siteValue,
		SameHost:  false,
	}); err != nil {
		return nil, err
	}

	// 校验不与同机活跃 Website 域名冲突。
	if s.deps.Websites != nil {
		ws, err := s.deps.Websites.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(w.Spec.Domain), "."), normDomain) {
				return nil, v1.Newf(v1.CodeConflict, "服务器 %s 上已存在同名 Website %q，不能与偷自己域名冲突", srv.Spec.Name, normDomain)
			}
		}
	}

	// 挑选需要重映射的目标非 WS Reality 入站（WS 入站保持在 127.0.0.1 绝不改动）。
	targetInboundTag := strings.TrimSpace(inv.String("inbound-tag", ""))
	targetIB, err := selectStealRealityInbound(ibs, rrs, targetInboundTag)
	if err != nil {
		return nil, err
	}

	ops := []map[string]any{
		{
			"op":              "update",
			"kind":            coreservers.KindName,
			"id":              srv.Metadata.ID,
			"resourceVersion": srv.Metadata.ResourceVersion,
			"spec": map[string]any{
				"use_443":    true,
				"steal_mode": mode,
				"domain":     normDomain,
				"site_type":  siteType,
				"site_value": siteValue,
			},
		},
	}

	if mode == "fallback" {
		// 清理已有 tunnel-in 入站及关联路由规则。
		for _, ib := range ibs {
			if ib.Spec.Tag == singbox.StealTunnelInboundTag {
				ops = append(ops, map[string]any{
					"op":              "delete",
					"kind":            coreinbounds.KindName,
					"id":              ib.Metadata.ID,
					"resourceVersion": ib.Metadata.ResourceVersion,
				})
			}
		}
		for _, rr := range rrs {
			if ruleOnlyBindsTag(parseOverrideRule(rr.Spec.Match), singbox.StealTunnelInboundTag) {
				ops = append(ops, map[string]any{
					"op":              "delete",
					"kind":            corerouting.KindName,
					"id":              rr.Metadata.ID,
					"resourceVersion": rr.Metadata.ResourceVersion,
				})
			}
		}
		// 将目标非 WS Reality 入站重映射至对外 443，handshake 指向 127.0.0.1:8001。
		if targetIB != nil {
			updatedTLS, err := patchRealityTLSForSteal(targetIB.Spec.TLS.Reveal(), normDomain)
			if err != nil {
				return nil, err
			}
			ops = append(ops, map[string]any{
				"op":              "update",
				"kind":            coreinbounds.KindName,
				"id":              targetIB.Metadata.ID,
				"resourceVersion": targetIB.Metadata.ResourceVersion,
				"spec": map[string]any{
					"port":   443,
					"listen": "",
					"tls":    json.RawMessage(updatedTLS),
				},
			})
		}
		return s.submitPlanOps(ctx, ops)
	}

	// mode == "tunnel"
	reqInternalPort := inv.Int("internal-port", 0)
	if reqInternalPort < 0 || reqInternalPort > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--internal-port %d 必须在 1..65535 之间", reqInternalPort)
	}
	if reqInternalPort == 443 || reqInternalPort == nginx.StealListenPort {
		return nil, v1.Newf(v1.CodeBadRequest, "--internal-port 不能为 443 或本地伪装站端口 %d", nginx.StealListenPort)
	}

	// 计算除 tunnel-in 与 targetIB 之外已占用的端口，避免端口冲突。
	otherUsed := map[int]bool{443: true, nginx.StealListenPort: true}
	for _, ib := range ibs {
		if !ib.Spec.Enabled || ib.Spec.Tag == singbox.StealTunnelInboundTag {
			continue
		}
		if targetIB != nil && ib.Metadata.ID == targetIB.Metadata.ID {
			continue
		}
		otherUsed[int(ib.Spec.Port)] = true
	}

	internalPort := reqInternalPort
	if internalPort > 0 {
		if otherUsed[internalPort] {
			return nil, v1.Newf(v1.CodeConflict, "指定的内部端口 %d 已被同机其它活跃入站占用", internalPort)
		}
	} else {
		if targetIB != nil && targetIB.Spec.Port != 443 && targetIB.Spec.Port != nginx.StealListenPort && !otherUsed[int(targetIB.Spec.Port)] {
			internalPort = int(targetIB.Spec.Port)
		} else if existingPort := findExistingTunnelFallbackPort(rrs); existingPort > 0 && !otherUsed[existingPort] {
			internalPort = existingPort
		} else {
			p, err := singbox.AllocateFreePort(otherUsed, int(srv.Spec.PortRangeMin), int(srv.Spec.PortRangeMax), true)
			if err != nil {
				return nil, err
			}
			internalPort = p
		}
	}

	// 处理 tunnel-in 入站：不存在则 create，已软删则 restore（避免自然键占坑冲突），活跃有差异则 update。
	existingTunnelIn, err := s.deps.Inbounds.ByServerTagIncludeDeleted(ctx, srv.Metadata.ID, singbox.StealTunnelInboundTag)
	switch {
	case errors.Is(err, coreinbounds.ErrNotFound):
		ops = append(ops, map[string]any{
			"op":   "create",
			"kind": coreinbounds.KindName,
			"spec": map[string]any{
				"server_id":  srv.Metadata.ID,
				"tag":        singbox.StealTunnelInboundTag,
				"protocol":   "direct",
				"port":       443,
				"listen":     "",
				"settings":   json.RawMessage(`{"network":"tcp"}`),
				"enabled":    true,
				"sort_order": 1,
			},
		})
	case err != nil:
		return nil, err
	case existingTunnelIn.Metadata.DeletedAt != nil:
		ops = append(ops, map[string]any{
			"op":              "restore",
			"kind":            coreinbounds.KindName,
			"id":              existingTunnelIn.Metadata.ID,
			"resourceVersion": existingTunnelIn.Metadata.ResourceVersion,
		})
	default:
		needUpdate := existingTunnelIn.Spec.Protocol != "direct" ||
			existingTunnelIn.Spec.Port != 443 ||
			strings.TrimSpace(existingTunnelIn.Spec.Listen) != "" ||
			!existingTunnelIn.Spec.Enabled ||
			singbox.DirectNetworkLabel(existingTunnelIn.Spec.Settings.Reveal()) != "tcp"
		if needUpdate {
			ops = append(ops, map[string]any{
				"op":              "update",
				"kind":            coreinbounds.KindName,
				"id":              existingTunnelIn.Metadata.ID,
				"resourceVersion": existingTunnelIn.Metadata.ResourceVersion,
				"spec": map[string]any{
					"protocol": "direct",
					"port":     443,
					"listen":   "",
					"settings": json.RawMessage(`{"network":"tcp"}`),
					"enabled":  true,
				},
			})
		}
	}

	// 重建 tunnel-in 的两条分流规则（先删除旧的 tunnel-in 专属规则，再创建 sort_order 10 与 20 两条规范规则）。
	for _, rr := range rrs {
		if ruleOnlyBindsTag(parseOverrideRule(rr.Spec.Match), singbox.StealTunnelInboundTag) {
			ops = append(ops, map[string]any{
				"op":              "delete",
				"kind":            corerouting.KindName,
				"id":              rr.Metadata.ID,
				"resourceVersion": rr.Metadata.ResourceVersion,
			})
		}
	}
	ops = append(ops,
		map[string]any{
			"op":   "create",
			"kind": corerouting.KindName,
			"spec": map[string]any{
				"server_id":  srv.Metadata.ID,
				"sort_order": 10,
				"match": map[string]any{
					"inbound":          []string{singbox.StealTunnelInboundTag},
					"domain":           []string{normDomain},
					"override_address": "127.0.0.1",
					"override_port":    nginx.StealListenPort,
				},
				"outbound_tag": "direct",
				"enabled":      true,
				"catch_all":    false,
			},
		},
		map[string]any{
			"op":   "create",
			"kind": corerouting.KindName,
			"spec": map[string]any{
				"server_id":  srv.Metadata.ID,
				"sort_order": 20,
				"match": map[string]any{
					"inbound":          []string{singbox.StealTunnelInboundTag},
					"override_address": "127.0.0.1",
					"override_port":    internalPort,
				},
				"outbound_tag": "direct",
				"enabled":      true,
				"catch_all":    false,
			},
		},
	)

	// 重映射目标非 WS Reality 入站至 127.0.0.1:<internalPort>，handshake 指向 127.0.0.1:8001。
	if targetIB != nil {
		updatedTLS, err := patchRealityTLSForSteal(targetIB.Spec.TLS.Reveal(), normDomain)
		if err != nil {
			return nil, err
		}
		ops = append(ops, map[string]any{
			"op":              "update",
			"kind":            coreinbounds.KindName,
			"id":              targetIB.Metadata.ID,
			"resourceVersion": targetIB.Metadata.ResourceVersion,
			"spec": map[string]any{
				"port":   internalPort,
				"listen": "127.0.0.1",
				"tls":    json.RawMessage(updatedTLS),
			},
		})
	}

	return s.submitPlanOps(ctx, ops)
}

func (s *Service) submitPlanOps(ctx context.Context, ops []map[string]any) (any, error) {
	raw, err := json.Marshal(ops)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码计划变更清单失败", err)
	}
	return s.deps.Planner.CreatePlanFromJSON(ctx, string(raw), "", "")
}

func selectStealRealityInbound(ibs []*coreinbounds.Inbound, rrs []*corerouting.RoutingRule, explicitTag string) (*coreinbounds.Inbound, error) {
	if explicitTag != "" {
		if explicitTag == singbox.StealTunnelInboundTag {
			return nil, v1.New(v1.CodeBadRequest, "--inbound-tag 不能指向基础设施入站 tunnel-in")
		}
		for _, ib := range ibs {
			if ib.Spec.Tag == explicitTag {
				if isWSInbound(ib.Spec) {
					return nil, v1.Newf(v1.CodeBadRequest, "入站 %q 使用 WebSocket 传输层（由本地 nginx 反代），不能作为偷自己 Reality 入口重映射", explicitTag)
				}
				if !isRealityInbound(ib.Spec) {
					return nil, v1.Newf(v1.CodeBadRequest, "入站 %q 未启用 Reality，不能作为偷自己 Reality 入口重映射", explicitTag)
				}
				return ib, nil
			}
		}
		return nil, v1.Newf(v1.CodeNotFound, "服务器上没有名为 %q 的活跃入站", explicitTag)
	}

	var candidates []*coreinbounds.Inbound
	for _, ib := range ibs {
		if !ib.Spec.Enabled || ib.Spec.Tag == singbox.StealTunnelInboundTag {
			continue
		}
		if isWSInbound(ib.Spec) || !isRealityInbound(ib.Spec) {
			continue
		}
		candidates = append(candidates, ib)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// 优先选择已在 443 端口的 Reality 入站，其次选择当前 tunnel-in 兜底指向的内部端口入站，否则取排序第一条。
	for _, ib := range candidates {
		if ib.Spec.Port == 443 {
			return ib, nil
		}
	}
	if prevPort := findExistingTunnelFallbackPort(rrs); prevPort > 0 {
		for _, ib := range candidates {
			if int(ib.Spec.Port) == prevPort {
				return ib, nil
			}
		}
	}
	return candidates[0], nil
}

func findExistingTunnelFallbackPort(rrs []*corerouting.RoutingRule) int {
	for _, rr := range rrs {
		if !rr.Spec.Enabled || rr.Spec.CatchAll {
			continue
		}
		pr := parseOverrideRule(rr.Spec.Match)
		if !ruleIncludesTag(pr, singbox.StealTunnelInboundTag) || !pr.HasOverride {
			continue
		}
		if pr.OtherCondCount > 0 {
			continue
		}
		if pr.OverrideAddress == "127.0.0.1" && pr.OverridePort != nginx.StealListenPort && pr.OverridePort != 443 {
			return pr.OverridePort
		}
	}
	return 0
}

func isWSInbound(spec v1.InboundSpec) bool {
	trimmed := bytes.TrimSpace(spec.Transport)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var obj map[string]any
	if json.Unmarshal(trimmed, &obj) != nil {
		return false
	}
	t, _ := obj["type"].(string)
	return strings.EqualFold(strings.TrimSpace(t), "ws")
}

func isRealityInbound(spec v1.InboundSpec) bool {
	trimmed := bytes.TrimSpace(spec.TLS.Reveal())
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var obj map[string]any
	if json.Unmarshal(trimmed, &obj) != nil {
		return false
	}
	if en, _ := obj["enabled"].(bool); !en {
		return false
	}
	rm, ok := obj["reality"].(map[string]any)
	if !ok {
		return false
	}
	ren, _ := rm["enabled"].(bool)
	return ren
}

func patchRealityTLSForSteal(rawTLS json.RawMessage, domain string) ([]byte, error) {
	var obj map[string]any
	trimmed := bytes.TrimSpace(rawTLS)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, v1.New(v1.CodeBadRequest, "目标入站缺少合法的 Reality TLS 配置")
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, v1.Wrap(v1.CodeBadRequest, "解析目标入站 TLS 配置失败", err)
	}
	rm, ok := obj["reality"].(map[string]any)
	if !ok {
		return nil, v1.New(v1.CodeBadRequest, "目标入站未配置 reality 段")
	}
	obj["enabled"] = true
	obj["server_name"] = domain
	rm["enabled"] = true
	rm["handshake"] = map[string]any{
		"server":      "127.0.0.1",
		"server_port": nginx.StealListenPort,
	}
	delete(rm, "public_key")
	obj["reality"] = rm
	return json.Marshal(obj)
}

func summarizeInbound(ib *coreinbounds.Inbound) StealInboundSummary {
	tr := "tcp"
	if raw := bytes.TrimSpace(ib.Spec.Transport); len(raw) > 0 && raw[0] == '{' {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) == nil {
			if t, _ := obj["type"].(string); strings.TrimSpace(t) != "" {
				tr = strings.ToLower(strings.TrimSpace(t))
			}
		}
	}
	sec := "none"
	sni := ""
	hs := ""
	if raw := bytes.TrimSpace(ib.Spec.TLS.Reveal()); len(raw) > 0 && raw[0] == '{' {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) == nil {
			if en, _ := obj["enabled"].(bool); en {
				sec = "tls"
				if s, _ := obj["server_name"].(string); strings.TrimSpace(s) != "" {
					sni = strings.TrimSpace(s)
				}
				if rm, ok := obj["reality"].(map[string]any); ok {
					if ren, _ := rm["enabled"].(bool); ren {
						sec = "reality"
					}
					if hsm, ok := rm["handshake"].(map[string]any); ok {
						hHost, _ := hsm["server"].(string)
						hPort := 443
						if fp, ok := hsm["server_port"].(float64); ok && fp > 0 {
							hPort = int(fp)
						}
						if hHost != "" {
							hs = fmt.Sprintf("%s:%d", hHost, hPort)
						}
					}
				}
			}
		}
	}
	return StealInboundSummary{
		ID:        ib.Metadata.ID,
		Tag:       ib.Spec.Tag,
		Protocol:  ib.Spec.Protocol,
		Port:      ib.Spec.Port,
		Listen:    ib.Spec.Listen,
		Transport: tr,
		Security:  sec,
		SNI:       sni,
		Handshake: hs,
		Enabled:   ib.Spec.Enabled,
	}
}
