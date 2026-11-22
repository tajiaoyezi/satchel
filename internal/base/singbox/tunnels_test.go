package singbox

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestRenderProtectMaster443AndTCPUDP443Coexist(t *testing.T) {
	srv := v1.ServerSpec{Name: "node-1", Ipv6Enabled: true, CoreLogLevel: "warn"}

	// 1. ProtectMaster443=true 拦截启用且端口为 443 的入站（返回 conflict），但不拦截禁用的 443 或非 443。
	_, err := Render(ServerInput{
		Server:           srv,
		ProtectMaster443: true,
		Inbounds: []v1.InboundSpec{
			{ServerID: 1, Tag: "vless-443", Protocol: "vless", Port: 443, Enabled: true},
		},
	})
	if err == nil || v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("ProtectMaster443 应拦截 443 入站并返回 conflict，实际：%v", err)
	}
	if _, err := Render(ServerInput{
		Server:           srv,
		ProtectMaster443: true,
		Inbounds: []v1.InboundSpec{
			{ServerID: 1, Tag: "vless-443-off", Protocol: "vless", Port: 443, Enabled: false},
			{ServerID: 1, Tag: "vless-8443", Protocol: "vless", Port: 8443, Enabled: true},
		},
	}); err != nil {
		t.Fatalf("禁用 443 与启用 8443 在 ProtectMaster443 下应放行，实际：%v", err)
	}

	// 2. direct(network:"tcp"):443 与 hysteria2(udp):443 共存成功。
	r, err := Render(ServerInput{
		Server: srv,
		Inbounds: []v1.InboundSpec{
			{
				ServerID: 1, Tag: StealTunnelInboundTag, Protocol: "direct", Port: 443, Enabled: true, SortOrder: 10,
				Settings: v1.SecretJSON(`{"network":"tcp"}`),
			},
			{
				ServerID: 1, Tag: "hy2-443", Protocol: "hysteria2", Port: 443, Enabled: true, SortOrder: 20,
				TLS:      v1.SecretJSON(`{"enabled":true}`),
				Settings: v1.SecretJSON(`{"users":[{"name":"u1","password":"pw"}]}`),
			},
		},
		RoutingRules: []v1.RoutingRuleSpec{
			{
				ServerID: 1, SortOrder: 10, Enabled: true, OutboundTag: "direct",
				Match: json.RawMessage(`{"inbound":["tunnel-in"],"domain":["decoy.example.com"],"override_address":"127.0.0.1","override_port":8001}`),
			},
			{
				ServerID: 1, SortOrder: 20, Enabled: true, OutboundTag: "direct",
				Match: json.RawMessage(`{"inbound":["tunnel-in"],"override_address":"127.0.0.1","override_port":18443}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("direct(tcp):443 + hysteria2(udp):443 应允许共存，实际：%v", err)
	}
	if !strings.Contains(string(r.Canonical), `"action":"sniff"`) {
		t.Fatalf("含 domain 条件应自动前置 sniff：%s", r.Canonical)
	}

	// 3. direct 未限定 network（TCP+UDP）监听 443 与 hysteria2(udp):443 应报端口冲突。
	if _, err := Render(ServerInput{
		Server: srv,
		Inbounds: []v1.InboundSpec{
			{ServerID: 1, Tag: "dir-both", Protocol: "direct", Port: 443, Enabled: true, Settings: v1.SecretJSON(`{}`)},
			{ServerID: 1, Tag: "hy2-443", Protocol: "hysteria2", Port: 443, Enabled: true, TLS: v1.SecretJSON(`{"enabled":true}`)},
		},
	}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
		t.Fatalf("direct(both):443 + hysteria2(udp):443 应报端口冲突，实际：%v", err)
	}

	// 4. 重复启用入站 tag 拦截。
	if _, err := Render(ServerInput{
		Server: srv,
		Inbounds: []v1.InboundSpec{
			{ServerID: 1, Tag: "dup", Protocol: "direct", Port: 10001, Enabled: true},
			{ServerID: 1, Tag: "dup", Protocol: "direct", Port: 10002, Enabled: true},
		},
	}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
		t.Fatalf("重复入站 tag 应报 bad_request，实际：%v", err)
	}
}

func TestValidateDirectNetworkAndOverrideMatch(t *testing.T) {
	// 合法的 direct settings.network
	for _, s := range []string{`{}`, `{"network":"tcp"}`, `{"network":"udp"}`, `{"network":["tcp","udp"]}`} {
		ib := v1.InboundSpec{Tag: "t1", Protocol: "direct", Port: 20000, Enabled: true, Settings: v1.SecretJSON(s)}
		if err := ValidateInboundSpec(ib); err != nil {
			t.Errorf("合法 direct settings %s 校验失败：%v", s, err)
		}
	}
	// 非法的 direct settings.network（含 "tcp,udp" 字符串）
	for _, s := range []string{`{"network":"tcp,udp"}`, `{"network":"icmp"}`, `{"network":[]}`, `{"network":123}`} {
		ib := v1.InboundSpec{Tag: "t1", Protocol: "direct", Port: 20000, Enabled: true, Settings: v1.SecretJSON(s)}
		if err := ValidateInboundSpec(ib); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("非法 direct settings %s 应报 bad_request，实际：%v", s, err)
		}
	}

	// 合法的 override_address / override_port
	if err := ValidateRoutingMatch(json.RawMessage(`{"inbound":["t1"],"override_address":"198.51.100.1","override_port":443}`)); err != nil {
		t.Fatalf("合法 override match 失败：%v", err)
	}
	// 非法的 override_address / override_port
	badMatches := []string{
		`{"override_address":""}`,
		`{"override_address":"bad host"}`,
		`{"override_address":"bad\nhost"}`,
		`{"override_address":123}`,
		`{"override_port":0}`,
		`{"override_port":70000}`,
		`{"override_port":"443"}`,
		`{"override_port":443.5}`,
	}
	for _, m := range badMatches {
		if err := ValidateRoutingMatch(json.RawMessage(m)); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("非法 match %s 应报 bad_request，实际：%v", m, err)
		}
	}
}

func TestTunnelHelpers(t *testing.T) {
	// NormalizeDirectNetwork + DirectNetworkSettingsJSON + DirectNetworkLabel
	sv, lbl, err := NormalizeDirectNetwork("tcp,udp")
	if err != nil || sv != "" || lbl != "tcp,udp" {
		t.Fatalf("tcp,udp 归一化错误：sv=%q lbl=%q err=%v", sv, lbl, err)
	}
	if got := string(DirectNetworkSettingsJSON(sv)); got != "{}" {
		t.Fatalf("双协议 settings 应为 {}，得到 %s", got)
	}
	if got := DirectNetworkLabel(DirectNetworkSettingsJSON(sv)); got != "tcp,udp" {
		t.Fatalf("DirectNetworkLabel 期望 tcp,udp，得到 %q", got)
	}
	if sv, lbl, err := NormalizeDirectNetwork("tcp"); err != nil || sv != "tcp" || lbl != "tcp" || DirectNetworkLabel(DirectNetworkSettingsJSON(sv)) != "tcp" {
		t.Fatalf("tcp 归一化失败：%q %q %v", sv, lbl, err)
	}
	if _, _, err := NormalizeDirectNetwork("sctp"); err == nil {
		t.Fatalf("非法网络 sctp 应报错")
	}

	// ValidateChainLabel / FormatChainTag / ParseChainTag
	if got, err := ValidateChainLabel("hk-jp-01"); err != nil || got != "hk-jp-01" {
		t.Fatalf("合法 label 校验失败：%q %v", got, err)
	}
	for _, bad := range []string{"a", "-ab", "ab-", "HK-1", "a_b", strings.Repeat("a", 33)} {
		if _, err := ValidateChainLabel(bad); err == nil {
			t.Errorf("非法 label %q 应报错", bad)
		}
	}
	tag := FormatChainTag("hk-jp-us", 2)
	if tag != "tunnel-hk-jp-us-h2" {
		t.Fatalf("FormatChainTag = %q", tag)
	}
	if l, h, ok := ParseChainTag(tag); !ok || l != "hk-jp-us" || h != 2 {
		t.Fatalf("ParseChainTag(%q) = %q, %d, %v", tag, l, h, ok)
	}
	for _, nonChain := range []string{"tunnel-in", "tunnel-20001", "tunnel-a-h0", "tunnel-ab-h01", "other-ab-h0"} {
		if _, _, ok := ParseChainTag(nonChain); ok {
			t.Errorf("非链式标签 %q 不应解析通过", nonChain)
		}
	}

	// AllocateFreePort & NextFreePort
	used := map[int]bool{442: true, 444: true}
	p, err := AllocateFreePort(used, 442, 445, true)
	if err != nil || p != 445 {
		t.Fatalf("避开 442/443/444 后应唯一选中 445，实际 p=%d err=%v", p, err)
	}
	if np, ok := NextFreePort(used, 442, 445, true); !ok || np != 445 {
		t.Fatalf("NextFreePort 期望 445，实际 %d %v", np, ok)
	}
	used[445] = true
	if _, err := AllocateFreePort(used, 442, 445, true); err == nil || v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("端口耗尽应返回 conflict，实际：%v", err)
	}
}
