// Package singbox 是基础层的纯 Go 规范 sing-box 配置渲染器、协议矩阵、密钥生成与敏感信息打码工具：
// 接收结构化的 Server / Inbound / Outbound / RoutingRule 规格，确定性生成符合 sing-box@v1.14.4 规范的 kernel.json，
// 并使用 pkg/xrpc.CanonicalJSON / CanonicalConfigHash 计算与节点侧完全一致的配置哈希。
package singbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// ServerInput 是单台服务器渲染 kernel.json 所需的只读输入快照。
type ServerInput struct {
	Server       v1.ServerSpec
	Inbounds     []v1.InboundSpec
	Outbounds    []v1.OutboundSpec
	RoutingRules []v1.RoutingRuleSpec
}

// Rendered 是确定性渲染结果：Canonical 为紧凑字典序 JSON，Pretty 为 2 空格缩进的可读 JSON，
// Hash 为与节点 config.status / config.apply 同源的 SHA-256 十六进制哈希。
type Rendered struct {
	Canonical []byte
	Pretty    []byte
	Hash      string
	Inbounds  []string
}

// Render 将单台服务器的内核全局配置、已启用入站、出站（含 WireGuard endpoints 与 balancer_group）
// 以及路由规则（含自动 sniff 与 rule_set）确定性渲染为符合 sing-box@v1.14.4 严格规范的配置。
func Render(in ServerInput) (*Rendered, error) {
	doc := map[string]any{}

	// 1. log
	level := strings.TrimSpace(in.Server.CoreLogLevel)
	if level == "" {
		level = "warn"
	}
	doc["log"] = map[string]any{
		"level":     level,
		"timestamp": true,
	}

	// 2. dns（仅当 core_dns 为非空 JSON 对象时输出）
	if dnsObj, err := decodeOptionalObject(in.Server.CoreDNS, "core_dns"); err != nil {
		return nil, err
	} else if len(dnsObj) > 0 {
		doc["dns"] = dnsObj
	}

	// 3. inbounds（只渲染 enabled=true，按 sort_order ASC, tag ASC 稳定排序）
	sortedIn := make([]v1.InboundSpec, 0, len(in.Inbounds))
	for _, ib := range in.Inbounds {
		if !ib.Enabled {
			continue
		}
		sortedIn = append(sortedIn, ib)
	}
	sort.SliceStable(sortedIn, func(i, j int) bool {
		if sortedIn[i].SortOrder != sortedIn[j].SortOrder {
			return sortedIn[i].SortOrder < sortedIn[j].SortOrder
		}
		return sortedIn[i].Tag < sortedIn[j].Tag
	})

	renderedInbounds := make([]any, 0, len(sortedIn))
	inboundTags := make([]string, 0, len(sortedIn))
	type portBind struct {
		tag    string
		listen string
		port   int64
		net    string // "tcp" | "udp" | "both"
	}
	var binds []portBind

	for _, ib := range sortedIn {
		if err := ValidateInboundSpec(ib); err != nil {
			return nil, err
		}
		entry := map[string]any{}
		if settingsObj, err := decodeOptionalObject(ib.Settings.Reveal(), "inbound.settings"); err != nil {
			return nil, fmt.Errorf("入站 %s: %w", ib.Tag, err)
		} else {
			for k, v := range settingsObj {
				entry[k] = v
			}
		}
		listen := strings.TrimSpace(ib.Listen)
		if listen == "" {
			if in.Server.Ipv6Enabled {
				listen = "::"
			} else {
				listen = "0.0.0.0"
			}
		}
		curNet := inboundTransportNet(ib.Protocol)
		for _, prev := range binds {
			if prev.port == ib.Port && listensOverlap(prev.listen, listen) && netsOverlap(prev.net, curNet) {
				return nil, v1.Newf(v1.CodeBadRequest, "入站 %s 与入站 %s 在 %s:%d 存在端口冲突", ib.Tag, prev.tag, listen, ib.Port)
			}
		}
		binds = append(binds, portBind{tag: ib.Tag, listen: listen, port: ib.Port, net: curNet})

		entry["type"] = strings.ToLower(strings.TrimSpace(ib.Protocol))
		entry["tag"] = ib.Tag
		entry["listen"] = listen
		entry["listen_port"] = ib.Port

		if tlsObj, err := decodeOptionalObject(ib.TLS.Reveal(), "inbound.tls"); err != nil {
			return nil, fmt.Errorf("入站 %s: %w", ib.Tag, err)
		} else if len(tlsObj) > 0 {
			stripRealityPublicKey(tlsObj)
			entry["tls"] = tlsObj
		}
		if trObj, err := decodeOptionalObject(ib.Transport, "inbound.transport"); err != nil {
			return nil, fmt.Errorf("入站 %s: %w", ib.Tag, err)
		} else if len(trObj) > 0 {
			entry["transport"] = trObj
		}
		renderedInbounds = append(renderedInbounds, entry)
		inboundTags = append(inboundTags, ib.Tag)
	}
	doc["inbounds"] = renderedInbounds

	// 4. outbounds & endpoints（wireguard 渲染至 endpoints；按 balancer_group 合成 urltest / selector）
	sortedOut := append([]v1.OutboundSpec(nil), in.Outbounds...)
	sort.SliceStable(sortedOut, func(i, j int) bool {
		return sortedOut[i].Tag < sortedOut[j].Tag
	})
	renderedOutbounds := make([]any, 0, len(sortedOut)+2)
	renderedEndpoints := make([]any, 0)
	explicitTags := map[string]bool{}
	detourRefs := map[string]string{}
	groupMembers := map[string][]string{}
	groupProbe := map[string]map[string]any{}
	hasDirect := false

	for _, ob := range sortedOut {
		proto := strings.ToLower(strings.TrimSpace(ob.Protocol))
		if explicitTags[ob.Tag] {
			return nil, v1.Newf(v1.CodeBadRequest, "出站 tag %q 重复", ob.Tag)
		}
		explicitTags[ob.Tag] = true

		entry := map[string]any{}
		if settingsObj, err := decodeOptionalObject(ob.Settings.Reveal(), "outbound.settings"); err != nil {
			return nil, fmt.Errorf("出站 %s: %w", ob.Tag, err)
		} else {
			for k, v := range settingsObj {
				entry[k] = v
			}
		}
		if det, ok := entry["detour"].(string); ok && strings.TrimSpace(det) != "" {
			detourRefs[ob.Tag] = strings.TrimSpace(det)
		}
		entry["type"] = proto
		entry["tag"] = ob.Tag
		if ob.Tag == "direct" {
			hasDirect = true
		}
		if proto == "wireguard" {
			renderedEndpoints = append(renderedEndpoints, entry)
		} else {
			renderedOutbounds = append(renderedOutbounds, entry)
		}
		if grp := strings.TrimSpace(ob.BalancerGroup); grp != "" {
			groupMembers[grp] = append(groupMembers[grp], ob.Tag)
			if _, has := groupProbe[grp]; !has {
				if pObj, err := decodeOptionalObject(ob.Probe, "outbound.probe"); err != nil {
					return nil, fmt.Errorf("出站 %s: %w", ob.Tag, err)
				} else if len(pObj) > 0 {
					groupProbe[grp] = pObj
				}
			}
		}
	}

	if !hasDirect {
		renderedOutbounds = append(renderedOutbounds, map[string]any{
			"type": "direct",
			"tag":  "direct",
		})
		explicitTags["direct"] = true
	}

	// 合成 balancer_group 组出站（sing-box@v1.14.4 支持 urltest 与 selector，无原生 loadbalance）。
	groupNames := make([]string, 0, len(groupMembers))
	for name := range groupMembers {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	validOutbounds := make(map[string]bool, len(explicitTags)+len(groupNames))
	for t := range explicitTags {
		validOutbounds[t] = true
	}
	for _, grp := range groupNames {
		if explicitTags[grp] {
			return nil, v1.Newf(v1.CodeBadRequest, "出站负载均衡组名 %q 与已有出站 tag 冲突", grp)
		}
		members := groupMembers[grp]
		sort.Strings(members)
		probeObj := groupProbe[grp]
		strategy := "urltest"
		if s, ok := probeObj["strategy"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "selector") {
			strategy = "selector"
		}
		grpEntry := map[string]any{
			"type":      strategy,
			"tag":       grp,
			"outbounds": members,
		}
		if strategy == "urltest" {
			urlVal := xrpc.DefaultOutboundProbeURL
			if u, ok := probeObj["url"].(string); ok && strings.TrimSpace(u) != "" {
				urlVal = strings.TrimSpace(u)
			}
			intervalVal := "3m"
			if iv, ok := probeObj["interval"].(string); ok && strings.TrimSpace(iv) != "" {
				intervalVal = strings.TrimSpace(iv)
			}
			grpEntry["url"] = urlVal
			grpEntry["interval"] = intervalVal
			if tol, ok := probeObj["tolerance"]; ok {
				grpEntry["tolerance"] = tol
			} else {
				grpEntry["tolerance"] = 50
			}
		}
		renderedOutbounds = append(renderedOutbounds, grpEntry)
		validOutbounds[grp] = true
	}

	// 校验所有出站 detour 目标存在且无环。
	for src, dst := range detourRefs {
		if !validOutbounds[dst] {
			return nil, v1.Newf(v1.CodeBadRequest, "出站 %s 的 detour 目标 %q 不存在", src, dst)
		}
	}
	if err := detectDetourCycle(detourRefs); err != nil {
		return nil, err
	}

	doc["outbounds"] = renderedOutbounds
	if len(renderedEndpoints) > 0 {
		doc["endpoints"] = renderedEndpoints
	}

	// 5. route（按 sort_order ASC 稳定排序；catch_all 决定 final，自动前置 sniff 并合成 rule_set）
	sortedRules := make([]v1.RoutingRuleSpec, 0, len(in.RoutingRules))
	for _, rr := range in.RoutingRules {
		if !rr.Enabled {
			continue
		}
		sortedRules = append(sortedRules, rr)
	}
	sort.SliceStable(sortedRules, func(i, j int) bool {
		return sortedRules[i].SortOrder < sortedRules[j].SortOrder
	})

	finalOutbound := "direct"
	renderedRules := make([]any, 0, len(sortedRules)+1)
	needsSniff := false
	hasExplicitSniff := false
	referencedRuleSets := map[string]bool{}
	customRuleSets := map[string]RuleSetDef{}

	for _, rr := range sortedRules {
		if rr.CatchAll {
			tag := strings.TrimSpace(rr.OutboundTag)
			if tag != "" {
				if tag != "direct" && tag != "block" && tag != "reject" && !validOutbounds[tag] {
					return nil, v1.Newf(v1.CodeBadRequest, "兜底路由引用的出站 %q 不存在", tag)
				}
				finalOutbound = tag
			}
			continue
		}
		if err := ValidateRoutingMatch(rr.Match); err != nil {
			return nil, err
		}
		ruleObj, err := decodeOptionalObject(rr.Match, "routing_rule.match")
		if err != nil {
			return nil, err
		}
		if ruleObj == nil {
			ruleObj = map[string]any{}
		}
		// 提取并剥离自定义 rule_set_defs（仅用于渲染 route.rule_set，不留在单条 rule 对象内）。
		if rawDefs, ok := ruleObj["rule_set_defs"]; ok {
			if b, err := json.Marshal(rawDefs); err == nil {
				var defs []RuleSetDef
				if json.Unmarshal(b, &defs) == nil {
					for _, d := range defs {
						if strings.TrimSpace(d.Tag) != "" {
							customRuleSets[strings.TrimSpace(d.Tag)] = d
						}
					}
				}
			}
			delete(ruleObj, "rule_set_defs")
		}
		// 检测是否显式包含 sniff 或需要自动 sniff。
		if act, ok := ruleObj["action"].(string); ok && strings.EqualFold(strings.TrimSpace(act), "sniff") {
			hasExplicitSniff = true
		}
		for _, k := range []string{"domain", "domain_suffix", "domain_keyword", "domain_regex", "protocol", "rule_set"} {
			if _, exists := ruleObj[k]; exists {
				needsSniff = true
				break
			}
		}
		for _, tag := range stringSliceField(ruleObj["rule_set"]) {
			referencedRuleSets[tag] = true
		}

		if _, hasAction := ruleObj["action"]; !hasAction {
			outTag := strings.TrimSpace(rr.OutboundTag)
			if outTag == "block" || outTag == "reject" {
				ruleObj["action"] = "reject"
			} else {
				if !validOutbounds[outTag] {
					return nil, v1.Newf(v1.CodeBadRequest, "路由规则引用的出站 %q 不存在", outTag)
				}
				ruleObj["action"] = "route"
				ruleObj["outbound"] = outTag
			}
		} else if act, _ := ruleObj["action"].(string); act == "route" {
			if outTag, _ := ruleObj["outbound"].(string); strings.TrimSpace(outTag) != "" && !validOutbounds[strings.TrimSpace(outTag)] {
				return nil, v1.Newf(v1.CodeBadRequest, "路由规则引用的出站 %q 不存在", outTag)
			}
		}
		renderedRules = append(renderedRules, ruleObj)
	}

	if needsSniff && !hasExplicitSniff {
		renderedRules = append([]any{map[string]any{"action": "sniff"}}, renderedRules...)
	}

	routeObj := map[string]any{
		"final": finalOutbound,
	}
	if len(renderedRules) > 0 {
		routeObj["rules"] = renderedRules
	}
	if len(referencedRuleSets) > 0 {
		rsTags := make([]string, 0, len(referencedRuleSets))
		for tag := range referencedRuleSets {
			rsTags = append(rsTags, tag)
		}
		sort.Strings(rsTags)
		rsList := make([]any, 0, len(rsTags))
		for _, tag := range rsTags {
			if def, ok := customRuleSets[tag]; ok {
				rsList = append(rsList, def)
			} else if def, ok := BuiltinRuleSets[tag]; ok {
				rsList = append(rsList, def)
			} else {
				return nil, v1.Newf(v1.CodeBadRequest, "路由规则引用的 rule_set %q 未定义（既不在内置规则集目录中，也未提供 rule_set_defs）", tag)
			}
		}
		routeObj["rule_set"] = rsList
	}
	doc["route"] = routeObj

	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "序列化 sing-box 配置失败", err)
	}
	canonical, err := xrpc.CanonicalJSON(raw)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "规范化 sing-box 配置失败", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, canonical, "", "  "); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "格式化 sing-box 配置失败", err)
	}
	return &Rendered{
		Canonical: canonical,
		Pretty:    pretty.Bytes(),
		Hash:      xrpc.CanonicalConfigHash(canonical),
		Inbounds:  inboundTags,
	}, nil
}

func stripRealityPublicKey(tlsObj map[string]any) {
	realMap, ok := tlsObj["reality"].(map[string]any)
	if !ok {
		return
	}
	delete(realMap, "public_key")
}

func inboundTransportNet(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "hysteria", "hysteria2", "tuic":
		return "udp"
	case "shadowsocks", "direct", "socks", "mixed":
		return "both"
	default:
		return "tcp"
	}
}

func netsOverlap(a, b string) bool {
	return a == "both" || b == "both" || a == b
}

func listensOverlap(a, b string) bool {
	isWild := func(s string) bool {
		return s == "" || s == "0.0.0.0" || s == "::"
	}
	return a == b || isWild(a) || isWild(b)
}

func detectDetourCycle(refs map[string]string) error {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(refs))
	var visit func(node string) error
	visit = func(node string) error {
		state[node] = visiting
		if next, ok := refs[node]; ok {
			if state[next] == visiting {
				return v1.Newf(v1.CodeBadRequest, "出站 detour 存在循环引用（涉及 %q -> %q）", node, next)
			}
			if state[next] == unvisited {
				if err := visit(next); err != nil {
					return err
				}
			}
		}
		state[node] = visited
		return nil
	}
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if state[k] == unvisited {
			if err := visit(k); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeOptionalObject(raw json.RawMessage, field string) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] != '{' {
		return nil, v1.Newf(v1.CodeBadRequest, "%s 必须是 JSON 对象", field)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, v1.Wrap(v1.CodeBadRequest, field+" 不是合法的 JSON 对象", err)
	}
	return m, nil
}
