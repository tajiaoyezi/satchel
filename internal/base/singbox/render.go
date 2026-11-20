// Package singbox 是基础层的纯 Go 规范 sing-box 配置渲染器与敏感信息打码工具：
// 接收结构化的 Server / Inbound / Outbound / RoutingRule 规格，确定性生成符合 sing-box 1.14 规范的 kernel.json，
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

// Render 将单台服务器的内核全局配置、已启用入站、出站与路由规则确定性渲染为 sing-box 配置。
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
	for _, ib := range sortedIn {
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
		entry["type"] = ib.Protocol
		entry["tag"] = ib.Tag
		entry["listen"] = listen
		entry["listen_port"] = ib.Port

		if tlsObj, err := decodeOptionalObject(ib.TLS.Reveal(), "inbound.tls"); err != nil {
			return nil, fmt.Errorf("入站 %s: %w", ib.Tag, err)
		} else if len(tlsObj) > 0 {
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

	// 4. outbounds（按 tag ASC 稳定排序；若未显式声明 direct，补充默认 direct 出站保证 sing-box 1.14 有效）
	sortedOut := append([]v1.OutboundSpec(nil), in.Outbounds...)
	sort.SliceStable(sortedOut, func(i, j int) bool {
		return sortedOut[i].Tag < sortedOut[j].Tag
	})
	renderedOutbounds := make([]any, 0, len(sortedOut)+1)
	hasDirect := false
	for _, ob := range sortedOut {
		entry := map[string]any{}
		if settingsObj, err := decodeOptionalObject(ob.Settings.Reveal(), "outbound.settings"); err != nil {
			return nil, fmt.Errorf("出站 %s: %w", ob.Tag, err)
		} else {
			for k, v := range settingsObj {
				entry[k] = v
			}
		}
		entry["type"] = ob.Protocol
		entry["tag"] = ob.Tag
		if ob.Tag == "direct" {
			hasDirect = true
		}
		renderedOutbounds = append(renderedOutbounds, entry)
	}
	if !hasDirect {
		renderedOutbounds = append(renderedOutbounds, map[string]any{
			"type": "direct",
			"tag":  "direct",
		})
	}
	doc["outbounds"] = renderedOutbounds

	// 5. route（按 sort_order ASC 稳定排序；catch_all 决定 final，否则默认 direct）
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
	renderedRules := make([]any, 0, len(sortedRules))
	for _, rr := range sortedRules {
		if rr.CatchAll {
			if tag := strings.TrimSpace(rr.OutboundTag); tag != "" {
				finalOutbound = tag
			}
			continue
		}
		ruleObj, err := decodeOptionalObject(rr.Match, "routing_rule.match")
		if err != nil {
			return nil, err
		}
		if ruleObj == nil {
			ruleObj = map[string]any{}
		}
		if _, hasAction := ruleObj["action"]; !hasAction {
			if rr.OutboundTag == "block" || rr.OutboundTag == "reject" {
				ruleObj["action"] = "reject"
			} else {
				ruleObj["action"] = "route"
				ruleObj["outbound"] = rr.OutboundTag
			}
		}
		renderedRules = append(renderedRules, ruleObj)
	}
	routeObj := map[string]any{
		"final": finalOutbound,
	}
	if len(renderedRules) > 0 {
		routeObj["rules"] = renderedRules
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
