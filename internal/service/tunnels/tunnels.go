package tunnels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// TunnelEntry 是 tunnel list 聚合展示的单条任意门隧道（单跳入站或复用分流规则）。
type TunnelEntry struct {
	Kind          string   `json:"kind"` // "inbound" | "multiplex"
	ServerID      int64    `json:"server_id"`
	ServerName    string   `json:"server_name"`
	InboundID     int64    `json:"inbound_id,omitempty"`
	RuleID        int64    `json:"rule_id,omitempty"`
	Tag           string   `json:"tag,omitempty"`
	InboundTag    string   `json:"inbound_tag,omitempty"`
	Listen        string   `json:"listen,omitempty"`
	ListenPort    int64    `json:"listen_port"`
	Network       string   `json:"network,omitempty"`
	MatchDomain   []string `json:"match_domain,omitempty"`
	MatchIP       []string `json:"match_ip,omitempty"`
	TargetAddress string   `json:"target_address,omitempty"`
	TargetPort    int      `json:"target_port,omitempty"`
	SortOrder     int64    `json:"sort_order,omitempty"`
	Enabled       bool     `json:"enabled"`
}

func (s *Service) tunnelList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	kindFilter := strings.ToLower(strings.TrimSpace(inv.String("kind", "")))
	switch kindFilter {
	case "", "inbound", "multiplex":
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "--kind %q 不合法（仅支持 inbound 或 multiplex）", kindFilter)
	}
	limit, offset, err := pageLimitOffset(inv)
	if err != nil {
		return nil, err
	}

	var targetSrvs []struct {
		id   int64
		name string
	}
	if ref := strings.TrimSpace(inv.String("server-name", "")); ref != "" {
		srv, err := s.resolveServer(ctx, ref)
		if err != nil {
			return nil, err
		}
		targetSrvs = append(targetSrvs, struct {
			id   int64
			name string
		}{id: srv.Metadata.ID, name: srv.Spec.Name})
	} else {
		srvs, err := s.deps.Servers.ListAll(ctx)
		if err != nil {
			return nil, err
		}
		sort.Slice(srvs, func(i, j int) bool { return srvs[i].Metadata.ID < srvs[j].Metadata.ID })
		for _, srv := range srvs {
			targetSrvs = append(targetSrvs, struct {
				id   int64
				name string
			}{id: srv.Metadata.ID, name: srv.Spec.Name})
		}
	}

	var all []TunnelEntry
	for _, sv := range targetSrvs {
		ibs, err := s.deps.Inbounds.ListByServer(ctx, sv.id)
		if err != nil {
			return nil, err
		}
		rrs, err := s.deps.RoutingRules.ListByServer(ctx, sv.id)
		if err != nil {
			return nil, err
		}

		directByTag := map[string]*coreinbounds.Inbound{}
		var directList []*coreinbounds.Inbound
		for _, ib := range ibs {
			if !strings.EqualFold(strings.TrimSpace(ib.Spec.Protocol), "direct") {
				continue
			}
			// 排除偷自己基础设施 tunnel-in 与链式隧道 tunnel-<label>-h<i>。
			if ib.Spec.Tag == singbox.StealTunnelInboundTag {
				continue
			}
			if _, _, isChain := singbox.ParseChainTag(ib.Spec.Tag); isChain {
				continue
			}
			directByTag[ib.Spec.Tag] = ib
			directList = append(directList, ib)
		}
		if len(directList) == 0 {
			continue
		}

		// 按 tag 归类兜底覆盖规则与复用分流规则。
		defaultRuleByTag := map[string]*corerouting.RoutingRule{}
		defaultParsedByTag := map[string]parsedOverrideRule{}
		var multiplexEntries []TunnelEntry

		for _, rr := range rrs {
			if rr.Spec.CatchAll {
				continue
			}
			pr := parseOverrideRule(rr.Spec.Match)
			if !pr.HasOverride {
				continue
			}
			for _, tag := range pr.Inbounds {
				ib, ok := directByTag[tag]
				if !ok {
					continue
				}
				if pr.OtherCondCount == 0 {
					if _, exists := defaultRuleByTag[tag]; !exists {
						defaultRuleByTag[tag] = rr
						defaultParsedByTag[tag] = pr
					}
				} else {
					domains := append(append([]string(nil), pr.Domains...), pr.DomainSuffixes...)
					ips := append(append([]string(nil), pr.IPCIDRs...), pr.SourceIPCIDRs...)
					multiplexEntries = append(multiplexEntries, TunnelEntry{
						Kind:          "multiplex",
						ServerID:      sv.id,
						ServerName:    sv.name,
						InboundID:     ib.Metadata.ID,
						RuleID:        rr.Metadata.ID,
						InboundTag:    ib.Spec.Tag,
						ListenPort:    ib.Spec.Port,
						MatchDomain:   domains,
						MatchIP:       ips,
						TargetAddress: pr.OverrideAddress,
						TargetPort:    pr.OverridePort,
						SortOrder:     rr.Spec.SortOrder,
						Enabled:       rr.Spec.Enabled && ib.Spec.Enabled,
					})
				}
			}
		}

		if kindFilter == "" || kindFilter == "inbound" {
			for _, ib := range directList {
				entry := TunnelEntry{
					Kind:       "inbound",
					ServerID:   sv.id,
					ServerName: sv.name,
					InboundID:  ib.Metadata.ID,
					Tag:        ib.Spec.Tag,
					Listen:     ib.Spec.Listen,
					ListenPort: ib.Spec.Port,
					Network:    singbox.DirectNetworkLabel(ib.Spec.Settings.Reveal()),
					SortOrder:  ib.Spec.SortOrder,
					Enabled:    ib.Spec.Enabled,
				}
				if dr, ok := defaultRuleByTag[ib.Spec.Tag]; ok {
					pr := defaultParsedByTag[ib.Spec.Tag]
					entry.RuleID = dr.Metadata.ID
					entry.TargetAddress = pr.OverrideAddress
					entry.TargetPort = pr.OverridePort
				}
				all = append(all, entry)
			}
		}
		if kindFilter == "" || kindFilter == "multiplex" {
			all = append(all, multiplexEntries...)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].ServerID != all[j].ServerID {
			return all[i].ServerID < all[j].ServerID
		}
		if all[i].ListenPort != all[j].ListenPort {
			return all[i].ListenPort < all[j].ListenPort
		}
		if all[i].Kind != all[j].Kind {
			return all[i].Kind < all[j].Kind
		}
		if all[i].SortOrder != all[j].SortOrder {
			return all[i].SortOrder < all[j].SortOrder
		}
		if all[i].InboundID != all[j].InboundID {
			return all[i].InboundID < all[j].InboundID
		}
		return all[i].RuleID < all[j].RuleID
	})

	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	slice := all[offset:end]
	items := make([]any, len(slice))
	for i := range slice {
		items[i] = slice[i]
	}
	return &command.PageResult{
		Items:      items,
		Total:      total,
		NextCursor: nextOffsetCursor(offset, len(slice), total),
	}, nil
}

func (s *Service) tunnelPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.String("server-name", ""))
	if err != nil {
		return nil, err
	}
	tgt, err := s.resolveTunnelTarget(ctx, inv)
	if err != nil {
		return nil, err
	}

	protect443 := s.isSameHost443Protected(ctx, srv, "")
	listenPort := inv.Int("listen-port", 0)
	if listenPort < 0 || listenPort > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--listen-port %d 必须在 1..65535 之间（或省略/0 表示自动分配）", listenPort)
	}
	if listenPort == 443 && protect443 {
		return nil, v1.New(v1.CodeConflict, "主控同机节点禁止监听 443 端口创建任意门隧道")
	}
	if listenPort == 0 {
		ibs, err := s.deps.Inbounds.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return nil, err
		}
		used := activeUsedPorts(ibs, srv.Spec.Use443)
		if tgt.TargetServerID == srv.Metadata.ID {
			used[tgt.Port] = true
		}
		p, err := singbox.AllocateFreePort(used, int(srv.Spec.PortRangeMin), int(srv.Spec.PortRangeMax), protect443)
		if err != nil {
			return nil, err
		}
		listenPort = p
	}

	if err := checkNoSelfLoop(srv, listenPort, tgt); err != nil {
		return nil, err
	}

	tag := strings.TrimSpace(inv.String("tag", ""))
	if tag == "" {
		tag = fmt.Sprintf("tunnel-%d", listenPort)
	}
	if tag == singbox.StealTunnelInboundTag {
		return nil, v1.New(v1.CodeBadRequest, "tag 不能使用保留的偷自己入站名 tunnel-in")
	}
	if _, _, isChain := singbox.ParseChainTag(tag); isChain {
		return nil, v1.Newf(v1.CodeBadRequest, "tag %q 符合链式隧道保留命名 tunnel-<label>-h<i>，请改用其它名称或使用 tunnel chain plan", tag)
	}

	netRaw := strings.TrimSpace(inv.String("network", ""))
	if netRaw == "" {
		netRaw = tgt.DefaultNetwork
	}
	netSetting, _, err := singbox.NormalizeDirectNetwork(netRaw)
	if err != nil {
		return nil, err
	}
	sortOrder := inv.Int("sort-order", 50)
	if !flagProvided(inv, "sort-order") {
		sortOrder = 50
	}

	ops := []map[string]any{
		{
			"op":   "create",
			"kind": coreinbounds.KindName,
			"spec": map[string]any{
				"server_id":  srv.Metadata.ID,
				"tag":        tag,
				"protocol":   "direct",
				"port":       listenPort,
				"listen":     strings.TrimSpace(inv.String("listen", "")),
				"settings":   singbox.DirectNetworkSettingsJSON(netSetting),
				"enabled":    true,
				"sort_order": sortOrder,
			},
		},
		{
			"op":   "create",
			"kind": corerouting.KindName,
			"spec": map[string]any{
				"server_id":  srv.Metadata.ID,
				"sort_order": sortOrder,
				"match": map[string]any{
					"inbound":          []string{tag},
					"override_address": tgt.Address,
					"override_port":    tgt.Port,
				},
				"outbound_tag": "direct",
				"enabled":      true,
				"catch_all":    false,
			},
		},
	}
	return s.submitPlanOps(ctx, ops)
}

func (s *Service) tunnelMultiplexPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.String("server-name", ""))
	if err != nil {
		return nil, err
	}
	inTag := strings.TrimSpace(inv.String("inbound-tag", ""))
	if inTag == "" {
		return nil, v1.New(v1.CodeBadRequest, "缺少必填参数 --inbound-tag")
	}
	if inTag == singbox.StealTunnelInboundTag {
		return nil, v1.New(v1.CodeBadRequest, "不能向偷自己基础设施入站 tunnel-in 添加手工复用分流规则")
	}
	ib, err := s.deps.Inbounds.ByServerTag(ctx, srv.Metadata.ID, inTag)
	if errors.Is(err, coreinbounds.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "服务器 %s 下没有名为 %q 的活跃入站", srv.Spec.Name, inTag)
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(ib.Spec.Protocol), "direct") {
		return nil, v1.Newf(v1.CodeBadRequest, "入站 %q 协议为 %s，复用分流只能挂载在 direct 入站上", inTag, ib.Spec.Protocol)
	}

	domains := splitStringsFlag(inv, "domain")
	domainSuffixes := splitStringsFlag(inv, "domain-suffix")
	ipCIDRs := splitStringsFlag(inv, "ip-cidr")
	srcIPCIDRs := splitStringsFlag(inv, "source-ip-cidr")
	if len(domains) == 0 && len(domainSuffixes) == 0 && len(ipCIDRs) == 0 && len(srcIPCIDRs) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "复用分流至少提供 --domain、--domain-suffix、--ip-cidr 或 --source-ip-cidr 之一")
	}

	tgt, err := s.resolveTunnelTarget(ctx, inv)
	if err != nil {
		return nil, err
	}
	if err := checkNoSelfLoop(srv, int(ib.Spec.Port), tgt); err != nil {
		return nil, err
	}

	sortOrder := inv.Int("sort-order", 0)
	if !flagProvided(inv, "sort-order") || sortOrder == 0 {
		// 自动计算严格小于该 direct 入站现有默认兜底规则的 sort_order，防止被兜底规则遮蔽。
		rrs, err := s.deps.RoutingRules.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return nil, err
		}
		minCatchAll := int64(50)
		found := false
		for _, rr := range rrs {
			if !rr.Spec.Enabled || rr.Spec.CatchAll {
				continue
			}
			pr := parseOverrideRule(rr.Spec.Match)
			if ruleIncludesTag(pr, inTag) && pr.OtherCondCount == 0 {
				if !found || rr.Spec.SortOrder < minCatchAll {
					minCatchAll = rr.Spec.SortOrder
					found = true
				}
			}
		}
		sortOrder = int(minCatchAll) - 10
	}

	match := map[string]any{
		"inbound":          []string{inTag},
		"override_address": tgt.Address,
		"override_port":    tgt.Port,
	}
	if len(domains) > 0 {
		match["domain"] = domains
	}
	if len(domainSuffixes) > 0 {
		match["domain_suffix"] = domainSuffixes
	}
	if len(ipCIDRs) > 0 {
		match["ip_cidr"] = ipCIDRs
	}
	if len(srcIPCIDRs) > 0 {
		match["source_ip_cidr"] = srcIPCIDRs
	}

	ops := []map[string]any{
		{
			"op":   "create",
			"kind": corerouting.KindName,
			"spec": map[string]any{
				"server_id":    srv.Metadata.ID,
				"sort_order":   sortOrder,
				"match":        match,
				"outbound_tag": "direct",
				"enabled":      true,
				"catch_all":    false,
			},
		},
	}
	return s.submitPlanOps(ctx, ops)
}

func (s *Service) tunnelDeletePlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}

	ruleID := int64(inv.Int("rule-id", 0))
	srvRef := strings.TrimSpace(inv.String("server-name", ""))
	tag := strings.TrimSpace(inv.String("tag", ""))

	hasRuleMode := ruleID > 0
	hasTagMode := srvRef != "" || tag != ""
	if hasRuleMode && hasTagMode {
		return nil, v1.New(v1.CodeBadRequest, "--rule-id 与 --server-name/--tag 只能二选一")
	}
	if !hasRuleMode && !hasTagMode {
		return nil, v1.New(v1.CodeBadRequest, "必须指定 --server-name + --tag（删除单跳隧道）或 --rule-id（删除单条复用分流规则）")
	}

	if hasRuleMode {
		rr, err := s.deps.RoutingRules.ByID(ctx, ruleID)
		if errors.Is(err, corerouting.ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "RoutingRule 里没有 id 为 %d 的活跃规则", ruleID)
		}
		if err != nil {
			return nil, err
		}
		pr := parseOverrideRule(rr.Spec.Match)
		if !pr.HasOverride || len(pr.Inbounds) == 0 || pr.OtherCondCount == 0 {
			return nil, v1.Newf(v1.CodeBadRequest, "RoutingRule #%d 不是任意门复用分流规则（若要删除整条单跳隧道请使用 --server-name 与 --tag）", ruleID)
		}
		for _, t := range pr.Inbounds {
			if t == singbox.StealTunnelInboundTag {
				return nil, v1.New(v1.CodeBadRequest, "不能删除偷自己基础设施 tunnel-in 的分流规则（请用 server steal plan）")
			}
			if _, _, isChain := singbox.ParseChainTag(t); isChain {
				return nil, v1.New(v1.CodeBadRequest, "不能单独删除链式隧道规则（请用 tunnel chain delete-plan）")
			}
		}
		return s.submitPlanOps(ctx, []map[string]any{
			{
				"op":              "delete",
				"kind":            corerouting.KindName,
				"id":              rr.Metadata.ID,
				"resourceVersion": rr.Metadata.ResourceVersion,
			},
		})
	}

	if srvRef == "" || tag == "" {
		return nil, v1.New(v1.CodeBadRequest, "删除单跳隧道必须同时提供 --server-name 与 --tag")
	}
	if tag == singbox.StealTunnelInboundTag {
		return nil, v1.New(v1.CodeBadRequest, "tunnel-in 是偷自己模式基础设施，请改用 server steal plan --mode off 清理")
	}
	if _, _, isChain := singbox.ParseChainTag(tag); isChain {
		return nil, v1.Newf(v1.CodeBadRequest, "%q 属于链式隧道，请改用 tunnel chain delete-plan 整链清理", tag)
	}
	srv, err := s.resolveServer(ctx, srvRef)
	if err != nil {
		return nil, err
	}
	ib, err := s.deps.Inbounds.ByServerTag(ctx, srv.Metadata.ID, tag)
	if errors.Is(err, coreinbounds.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "服务器 %s 下没有名为 %q 的活跃入站", srv.Spec.Name, tag)
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(ib.Spec.Protocol), "direct") {
		return nil, v1.Newf(v1.CodeBadRequest, "入站 %q 协议为 %s，不是任意门 direct 隧道入站", tag, ib.Spec.Protocol)
	}

	ops := []map[string]any{
		{
			"op":              "delete",
			"kind":            coreinbounds.KindName,
			"id":              ib.Metadata.ID,
			"resourceVersion": ib.Metadata.ResourceVersion,
		},
	}
	rrs, err := s.deps.RoutingRules.ListByServer(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	for _, rr := range rrs {
		if ruleOnlyBindsTag(parseOverrideRule(rr.Spec.Match), tag) {
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

// 避免未使用导入警告（占位供未来扩展时检查）。
var _ = json.Valid
