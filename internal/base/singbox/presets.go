package singbox

import (
	"encoding/json"
	"sort"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// RuleSetDef 描述一条可输出到 sing-box route.rule_set 的规则集定义。
type RuleSetDef struct {
	Tag            string           `json:"tag"`
	Type           string           `json:"type"` // "remote" | "local" | "inline"
	Format         string           `json:"format,omitempty"`
	URL            string           `json:"url,omitempty"`
	Path           string           `json:"path,omitempty"`
	DownloadDetour string           `json:"download_detour,omitempty"`
	Rules          []map[string]any `json:"rules,omitempty"`
}

// BuiltinRuleSets 是常用内置 rule_set 标签到 SagerNet 官方 .srs 规则集的映射。
var BuiltinRuleSets = map[string]RuleSetDef{
	"geosite-cn": {
		Tag: "geosite-cn", Type: "remote", Format: "binary",
		URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs", DownloadDetour: "direct",
	},
	"geoip-cn": {
		Tag: "geoip-cn", Type: "remote", Format: "binary",
		URL: "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs", DownloadDetour: "direct",
	},
	"geosite-openai": {
		Tag: "geosite-openai", Type: "remote", Format: "binary",
		URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-openai.srs", DownloadDetour: "direct",
	},
	"geosite-tiktok": {
		Tag: "geosite-tiktok", Type: "remote", Format: "binary",
		URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-tiktok.srs", DownloadDetour: "direct",
	},
	"geosite-google": {
		Tag: "geosite-google", Type: "remote", Format: "binary",
		URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-google.srs", DownloadDetour: "direct",
	},
	"geosite-meta": {
		Tag: "geosite-meta", Type: "remote", Format: "binary",
		URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-meta.srs", DownloadDetour: "direct",
	},
	"geosite-category-ads-all": {
		Tag: "geosite-category-ads-all", Type: "remote", Format: "binary",
		URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-category-ads-all.srs", DownloadDetour: "direct",
	},
}

// RoutingPreset 是一条内置 sing-box@v1.14.4 快捷路由预设（零 geoip/geosite 废弃键）。
type RoutingPreset struct {
	Key                string          `json:"key"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Match              json.RawMessage `json:"match"`
	DefaultOutboundTag string          `json:"default_outbound_tag"`
	RuleSets           []RuleSetDef    `json:"rule_sets,omitempty"`
}

// BuiltinRoutingPresets 返回 10 条内置 sing-box@v1.14.4 快捷路由预设。
func BuiltinRoutingPresets() []RoutingPreset {
	return []RoutingPreset{
		{
			Key: "block-bt", Name: "禁止 BitTorrent",
			Description: "屏蔽 BT 流量（渲染器自动前置 action: sniff 以识别协议）",
			Match:       json.RawMessage(`{"protocol":["bittorrent"]}`), DefaultOutboundTag: "reject",
		},
		{
			Key: "block-private", Name: "禁止访问内网地址",
			Description: "屏蔽局域网与保留网段（使用 sing-box 原生 ip_is_private）",
			Match:       json.RawMessage(`{"ip_is_private":true}`), DefaultOutboundTag: "reject",
		},
		{
			Key: "block-quic", Name: "屏蔽 UDP 443 (QUIC)",
			Description: "拦截客户端 HTTP/3 QUIC 流量以迫使回落 TCP",
			Match:       json.RawMessage(`{"network":["udp"],"port":[443]}`), DefaultOutboundTag: "reject",
		},
		{
			Key: "block-ads", Name: "屏蔽常见广告域名",
			Description: "基于 rule_set geosite-category-ads-all 拦截广告域名",
			Match:       json.RawMessage(`{"rule_set":["geosite-category-ads-all"]}`), DefaultOutboundTag: "reject",
			RuleSets: []RuleSetDef{BuiltinRuleSets["geosite-category-ads-all"]},
		},
		{
			Key: "cn-direct", Name: "中国大陆直连",
			Description: "基于 rule_set geosite-cn 与 geoip-cn 走直连出站",
			Match:       json.RawMessage(`{"rule_set":["geosite-cn","geoip-cn"]}`), DefaultOutboundTag: "direct",
			RuleSets: []RuleSetDef{BuiltinRuleSets["geosite-cn"], BuiltinRuleSets["geoip-cn"]},
		},
		{
			Key: "block-cn-ip", Name: "禁止回流中国大陆 IP",
			Description: "基于 rule_set geoip-cn 拒绝回流中国大陆 IP",
			Match:       json.RawMessage(`{"rule_set":["geoip-cn"]}`), DefaultOutboundTag: "reject",
			RuleSets: []RuleSetDef{BuiltinRuleSets["geoip-cn"]},
		},
		{
			Key: "openai-route", Name: "OpenAI 专用分流",
			Description: "将 OpenAI 相关域名分流至指定落地或 WARP 出站",
			Match:       json.RawMessage(`{"rule_set":["geosite-openai"]}`), DefaultOutboundTag: "direct",
			RuleSets: []RuleSetDef{BuiltinRuleSets["geosite-openai"]},
		},
		{
			Key: "tiktok-route", Name: "TikTok 解锁分流",
			Description: "将 TikTok 相关域名分流至指定解锁出站",
			Match:       json.RawMessage(`{"rule_set":["geosite-tiktok"]}`), DefaultOutboundTag: "direct",
			RuleSets: []RuleSetDef{BuiltinRuleSets["geosite-tiktok"]},
		},
		{
			Key: "warp-google-meta", Name: "防止送中（Google / Meta）",
			Description: "将 Google 与 Meta 域名分流至 WARP 或纯净落地出站",
			Match:       json.RawMessage(`{"rule_set":["geosite-google","geosite-meta"]}`), DefaultOutboundTag: "warp",
			RuleSets: []RuleSetDef{BuiltinRuleSets["geosite-google"], BuiltinRuleSets["geosite-meta"]},
		},
		{
			Key: "rfc-emby", Name: "RFC Emby 专属分流",
			Description: "匹配 rfc.uhdnow.com 走指定出站",
			Match:       json.RawMessage(`{"domain":["rfc.uhdnow.com"]}`), DefaultOutboundTag: "direct",
		},
	}
}

var deprecatedRoutingKeys = map[string]string{
	"geoip":   `sing-box 1.12 已移除路由字段 "geoip"，请改用 "rule_set": ["geoip-cn"]（参见 routing preset builtin）`,
	"geosite": `sing-box 1.12 已移除路由字段 "geosite"，请改用 "rule_set": ["geosite-cn"]（参见 routing preset builtin）`,
	"user":    `sing-box 中 "user" 表示本地 OS 进程用户名，按代理入站凭据名匹配请使用 "auth_user"`,
}

// ValidateRoutingMatch 校验路由规则 match JSON 对象是否合法且不含已废弃的 sing-box 键。
func ValidateRoutingMatch(raw json.RawMessage) error {
	obj, err := decodeOptionalObject(raw, "RoutingRule.spec.match")
	if err != nil {
		return err
	}
	for k, hint := range deprecatedRoutingKeys {
		if _, exists := obj[k]; exists {
			return v1.New(v1.CodeBadRequest, hint)
		}
	}
	return nil
}

// RoutingRuleInput 是路由遮蔽分析的输入行（按 sort_order ASC, id ASC 排序）。
type RoutingRuleInput struct {
	ID       int64
	Match    json.RawMessage
	Enabled  bool
	CatchAll bool
}

// RoutingRuleHint 是路由遮蔽与全兜底分析结果。
type RoutingRuleHint struct {
	IsInboundCatchAll bool    `json:"is_inbound_catch_all"`
	ShadowedBy        []int64 `json:"shadowed_by,omitempty"`
}

// AnalyzeRoutingRules 按 first-match 语义分析单台服务器的路由规则，标出入站全兜底与被前序兜底遮蔽的规则。
func AnalyzeRoutingRules(rules []RoutingRuleInput) map[int64]RoutingRuleHint {
	hints := make(map[int64]RoutingRuleHint, len(rules))
	var globalCatchAlls []int64
	inboundCatchAlls := map[string][]int64{}

	for _, r := range rules {
		if !r.Enabled || r.CatchAll {
			hints[r.ID] = RoutingRuleHint{}
			continue
		}
		obj, err := decodeOptionalObject(r.Match, "match")
		if err != nil {
			hints[r.ID] = RoutingRuleHint{}
			continue
		}
		// 判断该规则的匹配维度（忽略 action / rule_set_defs 等非匹配键）。
		inbounds := stringSliceField(obj["inbound"])
		otherConds := 0
		for k := range obj {
			switch k {
			case "inbound", "action", "rule_set_defs", "override_address", "override_port":
				continue
			default:
				otherConds++
			}
		}
		isGlobalCatchAll := len(inbounds) == 0 && otherConds == 0
		isInboundCatchAll := len(inbounds) > 0 && otherConds == 0

		shadowSet := map[int64]bool{}
		for _, gid := range globalCatchAlls {
			shadowSet[gid] = true
		}
		if len(inbounds) > 0 {
			// 若该规则针对特定入站，且其中任意入站已被前面的入站全兜底吞掉，标出遮蔽者。
			for _, ib := range inbounds {
				for _, sid := range inboundCatchAlls[ib] {
					shadowSet[sid] = true
				}
			}
		} else if len(inboundCatchAlls) > 0 {
			// 全局规则若排在某些入站全兜底之后，对那些入站不再生效，提示被哪些入站兜底遮蔽。
			for _, sids := range inboundCatchAlls {
				for _, sid := range sids {
					shadowSet[sid] = true
				}
			}
		}
		var shadowed []int64
		for sid := range shadowSet {
			shadowed = append(shadowed, sid)
		}
		sort.Slice(shadowed, func(i, j int) bool { return shadowed[i] < shadowed[j] })

		hints[r.ID] = RoutingRuleHint{
			IsInboundCatchAll: isInboundCatchAll,
			ShadowedBy:        shadowed,
		}
		if isGlobalCatchAll {
			globalCatchAlls = append(globalCatchAlls, r.ID)
		}
		if isInboundCatchAll {
			for _, ib := range inbounds {
				inboundCatchAlls[ib] = append(inboundCatchAlls[ib], r.ID)
			}
		}
	}
	return hints
}

func stringSliceField(v any) []string {
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
