package singbox

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

func TestRenderDeterminismAndRedact(t *testing.T) {
	srv := v1.ServerSpec{
		Name:         "hk-1",
		Ipv6Enabled:  true,
		CoreLogLevel: "info",
		CoreDNS:      json.RawMessage(`{"servers":[{"address":"local","tag":"dns-local"}]}`),
	}
	inA := ServerInput{
		Server: srv,
		Inbounds: []v1.InboundSpec{
			{
				ServerID: 1, Tag: "in-b", Protocol: "vless", Port: 8443, Listen: "127.0.0.1", Enabled: true, SortOrder: 20,
				TLS:      v1.SecretJSON(`{"enabled":true,"reality":{"enabled":true,"private_key":"priv-secret","short_id":["a1b2c3"]}}`),
				Settings: v1.SecretJSON(`{"users":[{"name":"u1","uuid":"6a4f1c2e-3b5d-4e7f-8a9b-0c1d2e3f4a5b"}]}`),
			},
			{
				ServerID: 1, Tag: "in-disabled", Protocol: "vless", Port: 9999, Enabled: false, SortOrder: 1,
			},
			{
				ServerID: 1, Tag: "in-a", Protocol: "shadowsocks", Port: 443, Enabled: true, SortOrder: 10,
				Settings: v1.SecretJSON(`{"method":"2022-blake3-aes-128-gcm","password":"ss-password"}`),
			},
		},
		Outbounds: []v1.OutboundSpec{
			{ServerID: 1, Tag: "out-proxy", Protocol: "socks", Settings: v1.SecretJSON(`{"server":"10.0.0.1","server_port":1080,"password":"out-pw"}`)},
		},
		RoutingRules: []v1.RoutingRuleSpec{
			{ServerID: 1, SortOrder: 20, Match: json.RawMessage(`{"domain_suffix":[".ads"]}`), OutboundTag: "block", Enabled: true},
			{ServerID: 1, SortOrder: 10, Match: json.RawMessage(`{"domain_suffix":[".cn"]}`), OutboundTag: "direct", Enabled: true},
			{ServerID: 1, SortOrder: 99, OutboundTag: "out-proxy", Enabled: true, CatchAll: true},
		},
	}

	// 打乱输入切片顺序，渲染结果与哈希必须完全一致。
	inB := ServerInput{
		Server:       srv,
		Inbounds:     []v1.InboundSpec{inA.Inbounds[2], inA.Inbounds[1], inA.Inbounds[0]},
		Outbounds:    inA.Outbounds,
		RoutingRules: []v1.RoutingRuleSpec{inA.RoutingRules[2], inA.RoutingRules[0], inA.RoutingRules[1]},
	}

	rA, err := Render(inA)
	if err != nil {
		t.Fatal(err)
	}
	rB, err := Render(inB)
	if err != nil {
		t.Fatal(err)
	}
	if rA.Hash != rB.Hash || string(rA.Canonical) != string(rB.Canonical) {
		t.Fatalf("渲染非确定性：\nA=%s\nB=%s", rA.Canonical, rB.Canonical)
	}
	if rA.Hash != xrpc.CanonicalConfigHash(rA.Pretty) {
		t.Fatalf("Pretty 与 Canonical 哈希不一致：%s vs %s", rA.Hash, xrpc.CanonicalConfigHash(rA.Pretty))
	}
	if len(rA.Inbounds) != 2 || rA.Inbounds[0] != "in-a" || rA.Inbounds[1] != "in-b" {
		t.Fatalf("仅启用入站应按 sort_order 排序，得到 %v", rA.Inbounds)
	}
	if strings.Contains(string(rA.Canonical), "in-disabled") {
		t.Fatalf("禁用入站不应渲染进 kernel.json")
	}
	if !strings.Contains(string(rA.Canonical), `"final":"out-proxy"`) || !strings.Contains(string(rA.Canonical), `"action":"reject"`) {
		t.Fatalf("路由规则渲染不完整：%s", rA.Canonical)
	}

	// 敏感字段脱敏检查。
	redacted := string(RedactJSON(rA.Pretty, true))
	for _, secret := range []string{"priv-secret", "a1b2c3", "6a4f1c2e-3b5d-4e7f-8a9b-0c1d2e3f4a5b", "ss-password", "out-pw"} {
		if strings.Contains(redacted, secret) {
			t.Errorf("打码后仍泄露敏感值 %q:\n%s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, `"***"`) {
		t.Errorf("打码结果应包含 ***")
	}
}
