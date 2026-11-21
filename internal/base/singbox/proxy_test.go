package singbox

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestKeygenAndDeriveRealityPublicKey(t *testing.T) {
	res, err := Keygen("all", "2022-blake3-aes-256-gcm")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.UUID) != 36 || strings.Count(res.UUID, "-") != 4 {
		t.Fatalf("bad UUID: %q", res.UUID)
	}
	if len(res.ShortID) != 8 {
		t.Fatalf("bad short_id: %q", res.ShortID)
	}
	derived, err := DeriveRealityPublicKey(res.RealityPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if derived != res.RealityPublicKey {
		t.Fatalf("derived public key mismatch: got %q want %q", derived, res.RealityPublicKey)
	}
	rawPSK, err := base64.StdEncoding.DecodeString(res.SS2022PSK)
	if err != nil || len(rawPSK) != 32 {
		t.Fatalf("expected 32-byte SS2022 PSK, got len=%d err=%v", len(rawPSK), err)
	}
	if wgPriv, err := base64.StdEncoding.DecodeString(res.WireGuardPrivateKey); err != nil || len(wgPriv) != 32 {
		t.Fatalf("bad wireguard priv: len=%d err=%v", len(wgPriv), err)
	}
	if _, err := Keygen("bogus", ""); err == nil {
		t.Fatal("expected error for bogus keygen type")
	}
}

func TestProtocolMatrixAndAllExamplesRender(t *testing.T) {
	mat := ProtocolMatrix()
	if len(mat.Native) != 15 {
		t.Fatalf("expected 15 native protocols, got %d", len(mat.Native))
	}
	all := ListExamples("", "", "")
	if len(all) < 12 {
		t.Fatalf("expected >=12 examples, got %d", len(all))
	}
	for _, ex := range all {
		r, err := Render(ServerInput{
			Server:   v1.ServerSpec{CoreLogLevel: "warn", Ipv6Enabled: true},
			Inbounds: []v1.InboundSpec{ex.Spec},
		})
		if err != nil {
			t.Errorf("example %s failed Render: %v", ex.ID, err)
		} else if strings.Contains(string(r.Canonical), `"public_key"`) {
			t.Errorf("example %s leaked public_key into kernel.json", ex.ID)
		}
	}
	if got := ListExamples("vless", "tcp", "reality"); len(got) != 1 || got[0].ID != "vless-reality-vision" {
		t.Fatalf("filtered examples mismatch: %+v", got)
	}
}

func TestBuildInboundMatrixAndRejections(t *testing.T) {
	built, err := BuildInbound(BuildInboundRequest{
		ServerID: 1, Protocol: "vless", Port: 18443, Security: "reality", SNI: "www.microsoft.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if built.Credentials.RealityPublicKey == "" || built.Credentials.UUID == "" || built.Credentials.ShortID == "" {
		t.Fatalf("expected generated credentials: %+v", built.Credentials)
	}
	derived, err := DeriveRealityPublicKey(built.Credentials.RealityPrivateKey)
	if err != nil || derived != built.Credentials.RealityPublicKey {
		t.Fatalf("reality keypair mismatch: derived=%q creds=%q err=%v", derived, built.Credentials.RealityPublicKey, err)
	}
	if strings.Contains(string(built.Spec.TLS.Reveal()), `"public_key"`) {
		t.Fatalf("spec.tls must not contain public_key: %s", built.Spec.TLS.Reveal())
	}

	// Reject unsupported combos
	if _, err := BuildInbound(BuildInboundRequest{ServerID: 1, Protocol: "anytls", Port: 443, Security: "reality", SNI: "a.com"}); err == nil {
		t.Fatal("expected anytls + reality rejection")
	}
	if _, err := BuildInbound(BuildInboundRequest{ServerID: 1, Protocol: "hysteria2", Port: 443, Transport: "ws", Security: "tls"}); err == nil {
		t.Fatal("expected hysteria2 + ws rejection")
	}
	if _, err := BuildInbound(BuildInboundRequest{ServerID: 1, Protocol: "shadowsocks", Port: 8388, Cipher: "2022-blake3-aes-128-gcm", Password: "too-short"}); err == nil {
		t.Fatal("expected invalid SS2022 PSK rejection")
	}
}

func TestRenderAdvancedSingBoxFeatures(t *testing.T) {
	// 1. VLESS TCP:443 + Hysteria2 UDP:443 allowed; balancer_group -> urltest & selector; wireguard -> endpoints; sniff + rule_set synthesized
	priv, _, err := GenerateRealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	in := ServerInput{
		Server: v1.ServerSpec{CoreLogLevel: "info"},
		Inbounds: []v1.InboundSpec{
			{
				ServerID: 1, Tag: "vless-443", Protocol: "vless", Port: 443, Listen: "::", Enabled: true,
				TLS:      v1.SecretJSON(`{"enabled":true,"reality":{"enabled":true,"handshake":{"server":"a.com","server_port":443},"private_key":"` + priv + `","public_key":"MUST-BE-STRIPPED","short_id":["a1b2c3d4"]}}`),
				Settings: v1.SecretJSON(`{"users":[{"name":"u1","uuid":"6a4f1c2e-3b5d-4e7f-8a9b-0c1d2e3f4a5b"}]}`),
			},
			{
				ServerID: 1, Tag: "hy2-443", Protocol: "hysteria2", Port: 443, Listen: "::", Enabled: true,
				TLS:      v1.SecretJSON(`{"enabled":true,"certificate_path":"/a.crt","key_path":"/a.key"}`),
				Settings: v1.SecretJSON(`{"users":[{"name":"u1","password":"pw"}]}`),
			},
		},
		Outbounds: []v1.OutboundSpec{
			{ServerID: 1, Tag: "hk-a", Protocol: "socks", BalancerGroup: "hk-pool", Settings: v1.SecretJSON(`{"server":"10.0.0.1","server_port":1080}`), Probe: json.RawMessage(`{"url":"https://example.com/204","interval":"2m","tolerance":30}`)},
			{ServerID: 1, Tag: "hk-b", Protocol: "socks", BalancerGroup: "hk-pool", Settings: v1.SecretJSON(`{"server":"10.0.0.2","server_port":1080,"detour":"hk-a"}`)},
			{ServerID: 1, Tag: "sel-a", Protocol: "socks", BalancerGroup: "manual-grp", Settings: v1.SecretJSON(`{"server":"10.0.0.3","server_port":1080}`), Probe: json.RawMessage(`{"strategy":"selector"}`)},
			{ServerID: 1, Tag: "wg-warp", Protocol: "wireguard", IsWarp: true, Settings: v1.SecretJSON(`{"address":["172.16.0.2/32"],"private_key":"wg-priv"}`)},
		},
		RoutingRules: []v1.RoutingRuleSpec{
			{ServerID: 1, SortOrder: 10, Match: json.RawMessage(`{"rule_set":["geosite-openai"]}`), OutboundTag: "hk-pool", Enabled: true},
			{ServerID: 1, SortOrder: 20, Match: json.RawMessage(`{"rule_set":["custom-inline"],"rule_set_defs":[{"tag":"custom-inline","type":"inline","rules":[{"domain_suffix":["example.org"]}]}]}`), OutboundTag: "wg-warp", Enabled: true},
			{ServerID: 1, SortOrder: 99, OutboundTag: "manual-grp", Enabled: true, CatchAll: true},
		},
	}

	rendered, err := Render(in)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(rendered.Canonical, &doc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered.Canonical), "MUST-BE-STRIPPED") {
		t.Fatal("tls.reality.public_key should be stripped")
	}
	eps, _ := doc["endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("expected 1 wireguard endpoint, got %v", doc["endpoints"])
	}
	rawStr := string(rendered.Canonical)
	for _, want := range []string{`"type":"urltest"`, `"tag":"hk-pool"`, `"type":"selector"`, `"tag":"manual-grp"`, `"action":"sniff"`, `"tag":"geosite-openai"`, `"tag":"custom-inline"`, `"final":"manual-grp"`} {
		if !strings.Contains(rawStr, want) {
			t.Errorf("rendered config missing %s:\n%s", want, rendered.Pretty)
		}
	}
	if strings.Contains(rawStr, "rule_set_defs") {
		t.Fatal("rule_set_defs helper key should be stripped from route.rules")
	}

	// 2. Reject TCP+TCP port collision
	_, err = Render(ServerInput{
		Server: v1.ServerSpec{},
		Inbounds: []v1.InboundSpec{
			{ServerID: 1, Tag: "a", Protocol: "vless", Port: 443, Listen: "::", Enabled: true},
			{ServerID: 1, Tag: "b", Protocol: "trojan", Port: 443, Listen: "127.0.0.1", Enabled: true},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "端口冲突") {
		t.Fatalf("expected port conflict error, got %v", err)
	}

	// 3. Reject detour cycle
	_, err = Render(ServerInput{
		Server: v1.ServerSpec{},
		Outbounds: []v1.OutboundSpec{
			{ServerID: 1, Tag: "a", Protocol: "socks", Settings: v1.SecretJSON(`{"detour":"b"}`)},
			{ServerID: 1, Tag: "b", Protocol: "socks", Settings: v1.SecretJSON(`{"detour":"a"}`)},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "循环引用") {
		t.Fatalf("expected detour cycle error, got %v", err)
	}

	// 4. Reject deprecated keys and dangling outbound_tag
	if _, err := Render(ServerInput{
		Server:   v1.ServerSpec{},
		Inbounds: []v1.InboundSpec{{ServerID: 1, Tag: "a", Protocol: "vless", Port: 443, Enabled: true, Settings: v1.SecretJSON(`{"sniff":true}`)}},
	}); err == nil {
		t.Fatal("expected deprecated inbound sniff rejection")
	}
	if _, err := Render(ServerInput{
		Server:       v1.ServerSpec{},
		RoutingRules: []v1.RoutingRuleSpec{{ServerID: 1, SortOrder: 1, Match: json.RawMessage(`{"geosite":["cn"]}`), OutboundTag: "direct", Enabled: true}},
	}); err == nil {
		t.Fatal("expected deprecated geosite rejection")
	}
	if _, err := Render(ServerInput{
		Server:       v1.ServerSpec{},
		RoutingRules: []v1.RoutingRuleSpec{{ServerID: 1, SortOrder: 1, Match: json.RawMessage(`{"domain":["a.com"]}`), OutboundTag: "missing-out", Enabled: true}},
	}); err == nil {
		t.Fatal("expected missing outbound_tag rejection")
	}
}

func TestAnalyzeRoutingRulesAndRedactNewKeys(t *testing.T) {
	hints := AnalyzeRoutingRules([]RoutingRuleInput{
		{ID: 1, Match: json.RawMessage(`{"inbound":["in-1"]}`), Enabled: true},
		{ID: 2, Match: json.RawMessage(`{"inbound":["in-1"],"domain":["example.com"]}`), Enabled: true},
		{ID: 3, Match: json.RawMessage(`{"domain":["global.com"]}`), Enabled: true},
	})
	if !hints[1].IsInboundCatchAll {
		t.Fatalf("rule 1 should be inbound catch-all: %+v", hints[1])
	}
	if len(hints[2].ShadowedBy) != 1 || hints[2].ShadowedBy[0] != 1 {
		t.Fatalf("rule 2 should be shadowed by 1: %+v", hints[2])
	}
	if len(hints[3].ShadowedBy) != 1 || hints[3].ShadowedBy[0] != 1 {
		t.Fatalf("global rule 3 should note shadowed_by inbound catch-all 1: %+v", hints[3])
	}

	masked := string(RedactJSON([]byte(`{"userkey":"uk1","auth_str":"as1","decryption":"dec1"}`), false))
	for _, s := range []string{"uk1", "as1", "dec1"} {
		if strings.Contains(masked, s) {
			t.Errorf("secret %q leaked after redact: %s", s, masked)
		}
	}
}
