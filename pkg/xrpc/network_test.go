package xrpc

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestParsePortSpecs(t *testing.T) {
	def, err := ParsePortSpecs(nil, PortScanMaxPorts)
	if err != nil || !slices.Equal(def, DefaultPortScanPorts) {
		t.Fatalf("默认端口解析异常：got=%v err=%v", def, err)
	}
	ports, err := ParsePortSpecs([]string{"443", "8000-8002", "80,443"}, PortScanMaxPorts)
	if err != nil {
		t.Fatalf("合法端口解析失败：%v", err)
	}
	want := []int{80, 443, 8000, 8001, 8002}
	if !slices.Equal(ports, want) {
		t.Fatalf("期望 %v，得到 %v", want, ports)
	}
	for _, bad := range [][]string{
		{""},
		{"0"},
		{"65536"},
		{"9000-8000"},
		{"1-300"},
		{"abc"},
	} {
		if _, err := ParsePortSpecs(bad, PortScanMaxPorts); err == nil {
			t.Fatalf("非法输入 %v 应失败", bad)
		}
	}
}

func TestNetworkStructsRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	res := NetworkValidateSiteResult{
		Domain: "example.com", Port: 443, SNI: "example.com",
		OK: true, Reachable: true, LatencyMS: 18, TLSVersion: "TLS 1.3", ALPN: "h2", HTTP2: true,
		CertValid: true, CertExpiresAt: &now, CertDaysLeft: 60, RealitySuitable: true, CheckedAt: now,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got NetworkValidateSiteResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.RealitySuitable || got.TLSVersion != "TLS 1.3" || got.CertDaysLeft != 60 {
		t.Fatalf("往返不一致：%+v", got)
	}

	rr := NetworkReturnRouteTestResult{
		Carrier: "CT", TargetIP: "1.2.3.4", RouteType: "CN2 GIA", EntryIP: "59.43.1.1", EntryASN: "AS4809",
		Hops: []ReturnRouteHop{{Hop: 1, IP: "59.43.1.1", ASN: "AS4809", LatencyMS: 12}}, TestedAt: now,
	}
	raw2, err := json.Marshal(rr)
	if err != nil {
		t.Fatal(err)
	}
	var got2 NetworkReturnRouteTestResult
	if err := json.Unmarshal(raw2, &got2); err != nil {
		t.Fatal(err)
	}
	if got2.RouteType != "CN2 GIA" || len(got2.Hops) != 1 || got2.Hops[0].ASN != "AS4809" {
		t.Fatalf("返程结果往返不一致：%+v", got2)
	}
}
