package tunnels

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// ChainHopView 描述链式隧道中的单跳。
type ChainHopView struct {
	Hop           int    `json:"hop"`
	ServerID      int64  `json:"server_id"`
	ServerName    string `json:"server_name"`
	InboundID     int64  `json:"inbound_id"`
	RuleID        int64  `json:"rule_id,omitempty"`
	Tag           string `json:"tag"`
	ListenPort    int64  `json:"listen_port"`
	Network       string `json:"network"`
	TargetAddress string `json:"target_address,omitempty"`
	TargetPort    int    `json:"target_port,omitempty"`
	Enabled       bool   `json:"enabled"`
}

// ChainView 是 tunnel chain list 聚合输出的单条链式隧道摘要。
type ChainView struct {
	Label           string         `json:"label"`
	HopCount        int            `json:"hop_count"`
	EntryServerID   int64          `json:"entry_server_id,omitempty"`
	EntryServerName string         `json:"entry_server_name,omitempty"`
	EntryHost       string         `json:"entry_host,omitempty"`
	EntryPort       int64          `json:"entry_port,omitempty"`
	FinalTarget     string         `json:"final_target,omitempty"`
	Network         string         `json:"network,omitempty"`
	Complete        bool           `json:"complete"`
	Hops            []ChainHopView `json:"hops"`
}

func (s *Service) tunnelChainList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	limit, offset, err := pageLimitOffset(inv)
	if err != nil {
		return nil, err
	}

	srvs, err := s.deps.Servers.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	srvByID := make(map[int64]*coreservers.Server, len(srvs))
	for _, srv := range srvs {
		srvByID[srv.Metadata.ID] = srv
	}

	byLabel := map[string][]ChainHopView{}
	for _, srv := range srvs {
		ibs, err := s.deps.Inbounds.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return nil, err
		}
		var chainInbounds []*coreinbounds.Inbound
		for _, ib := range ibs {
			if !strings.EqualFold(strings.TrimSpace(ib.Spec.Protocol), "direct") {
				continue
			}
			if _, _, ok := singbox.ParseChainTag(ib.Spec.Tag); ok {
				chainInbounds = append(chainInbounds, ib)
			}
		}
		if len(chainInbounds) == 0 {
			continue
		}
		rrs, err := s.deps.RoutingRules.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return nil, err
		}
		ruleByTag := map[string]*corerouting.RoutingRule{}
		parsedByTag := map[string]parsedOverrideRule{}
		for _, rr := range rrs {
			if rr.Spec.CatchAll {
				continue
			}
			pr := parseOverrideRule(rr.Spec.Match)
			if !pr.HasOverride || pr.OtherCondCount > 0 {
				continue
			}
			for _, t := range pr.Inbounds {
				if _, exists := ruleByTag[t]; !exists {
					ruleByTag[t] = rr
					parsedByTag[t] = pr
				}
			}
		}
		for _, ib := range chainInbounds {
			label, hop, ok := singbox.ParseChainTag(ib.Spec.Tag)
			if !ok {
				continue
			}
			hv := ChainHopView{
				Hop:        hop,
				ServerID:   srv.Metadata.ID,
				ServerName: srv.Spec.Name,
				InboundID:  ib.Metadata.ID,
				Tag:        ib.Spec.Tag,
				ListenPort: ib.Spec.Port,
				Network:    singbox.DirectNetworkLabel(ib.Spec.Settings.Reveal()),
				Enabled:    ib.Spec.Enabled,
			}
			if rr, exists := ruleByTag[ib.Spec.Tag]; exists {
				pr := parsedByTag[ib.Spec.Tag]
				hv.RuleID = rr.Metadata.ID
				hv.TargetAddress = pr.OverrideAddress
				hv.TargetPort = pr.OverridePort
			}
			byLabel[label] = append(byLabel[label], hv)
		}
	}

	labels := make([]string, 0, len(byLabel))
	for lbl := range byLabel {
		labels = append(labels, lbl)
	}
	sort.Strings(labels)

	chains := make([]ChainView, 0, len(labels))
	for _, lbl := range labels {
		hops := byLabel[lbl]
		sort.Slice(hops, func(i, j int) bool {
			if hops[i].Hop != hops[j].Hop {
				return hops[i].Hop < hops[j].Hop
			}
			return hops[i].ServerID < hops[j].ServerID
		})
		complete := len(hops) >= 2
		for idx, h := range hops {
			if h.Hop != idx || h.RuleID == 0 || h.TargetAddress == "" || h.TargetPort <= 0 {
				complete = false
				break
			}
		}
		cv := ChainView{
			Label:    lbl,
			HopCount: len(hops),
			Complete: complete,
			Hops:     hops,
		}
		if len(hops) > 0 {
			first := hops[0]
			cv.EntryServerID = first.ServerID
			cv.EntryServerName = first.ServerName
			if esrv := srvByID[first.ServerID]; esrv != nil {
				cv.EntryHost = serverEntryHost(esrv)
			}
			cv.EntryPort = first.ListenPort
			cv.Network = first.Network
			last := hops[len(hops)-1]
			if last.TargetAddress != "" && last.TargetPort > 0 {
				cv.FinalTarget = fmt.Sprintf("%s:%d", last.TargetAddress, last.TargetPort)
			}
		}
		chains = append(chains, cv)
	}

	total := len(chains)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	slice := chains[offset:end]
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

func (s *Service) tunnelChainPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}

	label, err := singbox.ValidateChainLabel(inv.String("label", ""))
	if err != nil {
		return nil, err
	}

	var rawServers []string
	if inv != nil {
		rawServers = inv.Strings("servers")
	}
	if len(rawServers) < 2 {
		return nil, v1.New(v1.CodeBadRequest, "链式隧道至少需要 2 台服务器（--servers 为字符串数组，按入口到出口顺序逐项传入）")
	}

	srvs := make([]*coreservers.Server, 0, len(rawServers))
	seenSrv := make(map[int64]bool, len(rawServers))
	for _, entry := range rawServers {
		ref := strings.TrimSpace(entry)
		if ref == "" {
			return nil, v1.New(v1.CodeBadRequest, "链式隧道 --servers 中不能包含空白服务器名")
		}
		srv, err := s.resolveServer(ctx, ref)
		if err != nil {
			return nil, err
		}
		if seenSrv[srv.Metadata.ID] {
			return nil, v1.Newf(v1.CodeBadRequest, "链式隧道 --servers 不能包含重复服务器 %q", srv.Spec.Name)
		}
		seenSrv[srv.Metadata.ID] = true
		srvs = append(srvs, srv)
	}

	// 除入口机外，每台后续服务器必须具备可达地址。
	hosts := make([]string, len(srvs))
	for i, srv := range srvs {
		hosts[i] = serverEntryHost(srv)
		if i > 0 && hosts[i] == "" {
			return nil, v1.Newf(v1.CodeBadRequest, "第 %d 跳服务器 %s 缺少可达地址（ip_address / domain / pull_address 均为空）", i+1, srv.Spec.Name)
		}
	}

	tgt, err := s.resolveTunnelTarget(ctx, inv)
	if err != nil {
		return nil, err
	}

	// 校验同名链 label 不被任何目标服务器占用（含软删除占坑），也不与其它服务器活跃链冲突。
	for i, srv := range srvs {
		tag := singbox.FormatChainTag(label, i)
		if _, err := s.deps.Inbounds.ByServerTagIncludeDeleted(ctx, srv.Metadata.ID, tag); err == nil {
			return nil, v1.Newf(v1.CodeNameTaken, "服务器 %s 上已存在链式隧道入站 %q（含软删除占坑）", srv.Spec.Name, tag)
		} else if !errors.Is(err, coreinbounds.ErrNotFound) {
			return nil, err
		}
	}
	existingAll, _, err := s.deps.Inbounds.List(ctx, coreinbounds.ListFilter{Protocol: "direct", IncludeDeleted: true}, 10000, 0)
	if err != nil {
		return nil, err
	}
	for _, ib := range existingAll {
		if existingLbl, _, ok := singbox.ParseChainTag(ib.Spec.Tag); ok && existingLbl == label {
			// 目标服务器上任意跳号已占坑，或其他服务器上已有活跃同名链均报 name_taken。
			if seenSrv[ib.Spec.ServerID] || ib.Metadata.DeletedAt == nil {
				return nil, v1.Newf(v1.CodeNameTaken, "链式隧道 label %q 已被占用（冲突入站 %s）", label, ib.Spec.Tag)
			}
		}
	}

	netRaw := strings.TrimSpace(inv.String("network", ""))
	if netRaw == "" {
		netRaw = tgt.DefaultNetwork
	}
	netSetting, _, err := singbox.NormalizeDirectNetwork(netRaw)
	if err != nil {
		return nil, err
	}
	settingsJSON := singbox.DirectNetworkSettingsJSON(netSetting)

	// 加载每台服务器的活跃入站端口与同机 443 守卫标志。
	n := len(srvs)
	usedByHop := make([]map[int]bool, n)
	protectByHop := make([]bool, n)
	for i, srv := range srvs {
		ibs, err := s.deps.Inbounds.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return nil, err
		}
		usedByHop[i] = activeUsedPorts(ibs, srv.Spec.Use443)
		protectByHop[i] = s.isSameHost443Protected(ctx, srv, "")
	}

	// 按跳分配端口：
	// - 入口跳（0）：使用 --entry-port 或随机空闲；
	// - 中间跳（1..N-2）：优先沿用上一跳端口，冲突则随机空闲；
	// - 出口跳（N-1）：优先沿用入口端口，冲突则随机空闲（且避开与最终目标自环）。
	ports := make([]int, n)
	entryPort := inv.Int("entry-port", 0)
	if entryPort < 0 || entryPort > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--entry-port %d 必须在 1..65535 之间（或省略/0 表示自动分配）", entryPort)
	}
	if entryPort == 443 && protectByHop[0] {
		return nil, v1.New(v1.CodeConflict, "入口服务器为主控同机节点，禁止使用 443 端口")
	}
	if entryPort > 0 {
		if usedByHop[0][entryPort] {
			return nil, v1.Newf(v1.CodeConflict, "入口服务器 %s 的端口 %d 已被占用", srvs[0].Spec.Name, entryPort)
		}
		ports[0] = entryPort
	} else {
		p, err := singbox.AllocateFreePort(usedByHop[0], int(srvs[0].Spec.PortRangeMin), int(srvs[0].Spec.PortRangeMax), protectByHop[0])
		if err != nil {
			return nil, err
		}
		ports[0] = p
	}

	for i := 1; i < n-1; i++ {
		pref := ports[i-1]
		if !usedByHop[i][pref] && !(protectByHop[i] && pref == 443) {
			ports[i] = pref
		} else {
			p, err := singbox.AllocateFreePort(usedByHop[i], int(srvs[i].Spec.PortRangeMin), int(srvs[i].Spec.PortRangeMax), protectByHop[i])
			if err != nil {
				return nil, err
			}
			ports[i] = p
		}
	}

	// 出口跳（N-1）
	exitIdx := n - 1
	exitUsed := usedByHop[exitIdx]
	if checkNoSelfLoop(srvs[exitIdx], tgt.Port, tgt) != nil {
		exitUsed[tgt.Port] = true
	}
	exitPref := ports[0]
	if !exitUsed[exitPref] && !(protectByHop[exitIdx] && exitPref == 443) && checkNoSelfLoop(srvs[exitIdx], exitPref, tgt) == nil {
		ports[exitIdx] = exitPref
	} else {
		p, err := singbox.AllocateFreePort(exitUsed, int(srvs[exitIdx].Spec.PortRangeMin), int(srvs[exitIdx].Spec.PortRangeMax), protectByHop[exitIdx])
		if err != nil {
			return nil, err
		}
		ports[exitIdx] = p
	}
	if err := checkNoSelfLoop(srvs[exitIdx], ports[exitIdx], tgt); err != nil {
		return nil, err
	}

	// 构造全部 2N 个变更项并提交为单一原子 Plan。
	ops := make([]map[string]any, 0, 2*n)
	for i := 0; i < n; i++ {
		tag := singbox.FormatChainTag(label, i)
		var nextAddr string
		var nextPort int
		if i < n-1 {
			nextAddr = hosts[i+1]
			nextPort = ports[i+1]
		} else {
			nextAddr = tgt.Address
			nextPort = tgt.Port
		}
		ops = append(ops,
			map[string]any{
				"op":   "create",
				"kind": coreinbounds.KindName,
				"spec": map[string]any{
					"server_id":  srvs[i].Metadata.ID,
					"tag":        tag,
					"protocol":   "direct",
					"port":       ports[i],
					"listen":     "",
					"settings":   settingsJSON,
					"enabled":    true,
					"sort_order": 50,
				},
			},
			map[string]any{
				"op":   "create",
				"kind": corerouting.KindName,
				"spec": map[string]any{
					"server_id":  srvs[i].Metadata.ID,
					"sort_order": 50,
					"match": map[string]any{
						"inbound":          []string{tag},
						"override_address": nextAddr,
						"override_port":    nextPort,
					},
					"outbound_tag": "direct",
					"enabled":      true,
					"catch_all":    false,
				},
			},
		)
	}
	return s.submitPlanOps(ctx, ops)
}

func (s *Service) tunnelChainDeletePlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}
	label, err := singbox.ValidateChainLabel(inv.String("label", ""))
	if err != nil {
		return nil, err
	}

	srvs, err := s.deps.Servers.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	type matchedHop struct {
		hop int
		ib  *coreinbounds.Inbound
	}
	var matched []matchedHop
	for _, srv := range srvs {
		ibs, err := s.deps.Inbounds.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return nil, err
		}
		for _, ib := range ibs {
			if !strings.EqualFold(strings.TrimSpace(ib.Spec.Protocol), "direct") {
				continue
			}
			if lbl, hop, ok := singbox.ParseChainTag(ib.Spec.Tag); ok && lbl == label {
				matched = append(matched, matchedHop{hop: hop, ib: ib})
			}
		}
	}
	if len(matched) == 0 {
		return nil, v1.Newf(v1.CodeNotFound, "未找到标签为 %q 的活跃链式隧道", label)
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].hop != matched[j].hop {
			return matched[i].hop < matched[j].hop
		}
		return matched[i].ib.Spec.ServerID < matched[j].ib.Spec.ServerID
	})

	ops := make([]map[string]any, 0, len(matched)*2)
	seenRule := map[int64]bool{}
	rulesCache := map[int64][]*corerouting.RoutingRule{}
	for _, m := range matched {
		ops = append(ops, map[string]any{
			"op":              "delete",
			"kind":            coreinbounds.KindName,
			"id":              m.ib.Metadata.ID,
			"resourceVersion": m.ib.Metadata.ResourceVersion,
		})
		srvID := m.ib.Spec.ServerID
		rrs, cached := rulesCache[srvID]
		if !cached {
			rrs, err = s.deps.RoutingRules.ListByServer(ctx, srvID)
			if err != nil {
				return nil, err
			}
			rulesCache[srvID] = rrs
		}
		for _, rr := range rrs {
			if seenRule[rr.Metadata.ID] {
				continue
			}
			if ruleOnlyBindsTag(parseOverrideRule(rr.Spec.Match), m.ib.Spec.Tag) {
				seenRule[rr.Metadata.ID] = true
				ops = append(ops, map[string]any{
					"op":              "delete",
					"kind":            corerouting.KindName,
					"id":              rr.Metadata.ID,
					"resourceVersion": rr.Metadata.ResourceVersion,
				})
			}
		}
	}
	return s.submitPlanOps(ctx, ops)
}
