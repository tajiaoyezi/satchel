package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/pkg/xrpc"
)

func TestNetworkProbesWarpAndReturnRoutesEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		pub := masterPub(t, h)

		tokHK := h.addServer("hk-net")
		_ = h.addServer("hk-off") // 保持离线，用于验证纯 ReturnRoute 变更不推节点。

		wgPriv, wgPub, err := singbox.GenerateWireGuardKeypair()
		if err != nil {
			t.Fatal(err)
		}

		var mu sync.Mutex
		warpInstalled := false
		warpAcctType := "free"
		warpHasLicense := false
		warpPriv := wgPriv
		warpAddrV4 := "172.16.0.2/32"
		warpReserved := []int{11, 22, 33}
		lastAppliedKernel := ""
		configHash := ""
		var generation uint64

		methods := xrpc.Methods{
			xrpc.MethodConfigApply: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.ConfigApplyParams
				_ = json.Unmarshal(raw, &p)
				mu.Lock()
				defer mu.Unlock()
				prev := configHash
				lastAppliedKernel = p.Config
				configHash = xrpc.CanonicalConfigHash([]byte(p.Config))
				generation++
				return xrpc.ConfigApplyResult{
					Applied: true, AppliedHash: configHash, PreviousHash: prev, CurrentHash: configHash,
					Generation: generation, KernelRunning: true, Inbounds: []string{},
				}, nil
			},
			xrpc.MethodNetworkPortScan: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.NetworkPortScanParams
				_ = json.Unmarshal(raw, &p)
				return xrpc.NetworkPortScanResult{
					Host: p.Host, ScannedCount: len(p.Ports), OpenPorts: []int{443}, ClosedCount: len(p.Ports) - 1,
					Results: []xrpc.PortScanItem{{Port: 443, Open: true, LatencyMS: 3}}, DurationMS: 5,
				}, nil
			},
			xrpc.MethodNetworkValidateSite: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.NetworkValidateSiteParams
				_ = json.Unmarshal(raw, &p)
				if p.Port == 65432 {
					return xrpc.NetworkValidateSiteResult{
						Domain: p.Domain, Port: p.Port, SNI: p.SNI, OK: false, Reachable: false,
						Error: "connection refused", CheckedAt: time.Now().UTC(),
					}, nil
				}
				return xrpc.NetworkValidateSiteResult{
					Domain: p.Domain, Port: p.Port, SNI: p.SNI, OK: true, Reachable: true, LatencyMS: 12,
					TLSVersion: "TLS 1.3", ALPN: "h2", HTTP2: true, CertValid: true, RealitySuitable: true,
					CertSubject: p.Domain, CertDaysLeft: 60, CheckedAt: time.Now().UTC(),
				}, nil
			},
			xrpc.MethodNetworkDomainLatency: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.NetworkDomainLatencyParams
				_ = json.Unmarshal(raw, &p)
				items := make([]xrpc.DomainLatencyItem, len(p.Domains))
				for i, d := range p.Domains {
					items[i] = xrpc.DomainLatencyItem{Domain: d, OK: true, LatencyMS: int64(20 - i), TLSVersion: "TLS 1.3", ALPN: "h2"}
				}
				fastest := append([]xrpc.DomainLatencyItem(nil), items...)
				if len(fastest) == 2 {
					fastest[0], fastest[1] = fastest[1], fastest[0]
				}
				return xrpc.NetworkDomainLatencyResult{
					Mode: p.Mode, Port: p.Port, Total: len(items), OKCount: len(items), Results: items, Fastest: fastest,
				}, nil
			},
			xrpc.MethodNetworkReturnRouteTest: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.NetworkReturnRouteTestParams
				_ = json.Unmarshal(raw, &p)
				if p.Carrier == "CT" {
					return xrpc.NetworkReturnRouteTestResult{
						Carrier: "CT", TargetIP: "202.97.94.229", RouteType: "CN2 GIA", Region: "Shanghai",
						EntryIP: "59.43.130.1", EntryASN: "AS4809", Reason: "hit 59.43",
						Hops: []xrpc.ReturnRouteHop{{Hop: 1, IP: "59.43.130.1", ASN: "AS4809", LatencyMS: 11}}, TestedAt: time.Now().UTC(),
					}, nil
				}
				return xrpc.NetworkReturnRouteTestResult{
					Carrier: p.Carrier, TargetIP: "210.22.97.1", RouteType: "AS9929", Region: "Shanghai",
					EntryIP: "218.105.1.1", EntryASN: "AS9929", Reason: "hit 9929",
					Hops: []xrpc.ReturnRouteHop{{Hop: 1, IP: "218.105.1.1", ASN: "AS9929", LatencyMS: 15}}, TestedAt: time.Now().UTC(),
				}, nil
			},
			xrpc.MethodWarpStatus: func(_ context.Context, _ json.RawMessage) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				return xrpc.WarpStatusResult{
					Installed: warpInstalled, AccountType: warpAcctType, PublicKey: wgPub,
					PeerPublicKey: xrpc.DefaultWarpPeerPublicKey, Endpoint: xrpc.DefaultWarpEndpoint,
					AddressV4: warpAddrV4, Reserved: warpReserved, MTU: 1280, HasLicense: warpHasLicense,
				}, nil
			},
			xrpc.MethodWarpInstall: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.WarpInstallParams
				_ = json.Unmarshal(raw, &p)
				mu.Lock()
				defer mu.Unlock()
				warpInstalled = true
				if p.LicenseKey != "" {
					warpAcctType = "plus"
					warpHasLicense = true
				}
				return xrpc.WarpInstallResult{
					WarpStatusResult: xrpc.WarpStatusResult{
						Installed: true, AccountType: warpAcctType, DeviceID: "dev-e2e", PublicKey: wgPub,
						PeerPublicKey: xrpc.DefaultWarpPeerPublicKey, Endpoint: xrpc.DefaultWarpEndpoint,
						AddressV4: warpAddrV4, Reserved: warpReserved, MTU: 1280, HasLicense: warpHasLicense,
					},
				}, nil
			},
			xrpc.MethodWarpAuthorize: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.WarpAuthorizeParams
				_ = json.Unmarshal(raw, &p)
				mu.Lock()
				defer mu.Unlock()
				warpInstalled = true
				mode := "license"
				if p.PrivateKey != "" {
					mode = "import"
					warpAcctType = "imported"
					warpPriv = p.PrivateKey
					warpAddrV4 = p.AddressV4
					warpReserved = p.Reserved
				} else {
					warpAcctType = "plus"
					warpHasLicense = true
				}
				return xrpc.WarpAuthorizeResult{
					WarpStatusResult: xrpc.WarpStatusResult{
						Installed: true, AccountType: warpAcctType, PublicKey: wgPub,
						PeerPublicKey: xrpc.DefaultWarpPeerPublicKey, Endpoint: xrpc.DefaultWarpEndpoint,
						AddressV4: warpAddrV4, Reserved: warpReserved, MTU: 1280, HasLicense: warpHasLicense,
					},
					Mode: mode,
				}, nil
			},
			xrpc.MethodWarpRemove: func(_ context.Context, _ json.RawMessage) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				warpInstalled = false
				return xrpc.WarpRemoveResult{Removed: true}, nil
			},
			xrpc.MethodWarpCredentials: func(_ context.Context, _ json.RawMessage) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				if !warpInstalled {
					return nil, &xrpc.Error{Code: xrpc.CodeConflict, Message: "warp not installed"}
				}
				return xrpc.WarpCredentialsResult{
					Installed: true, AccountType: warpAcctType, PrivateKey: warpPriv, PublicKey: wgPub,
					PeerPublicKey: xrpc.DefaultWarpPeerPublicKey, Endpoint: xrpc.DefaultWarpEndpoint,
					AddressV4: warpAddrV4, Reserved: warpReserved, MTU: 1280,
				}, nil
			},
		}

		ag := mustDial(t, h)
		if res := ag.authenticate(pub, tokHK, xrpc.ControlProtocol); !res.OK {
			t.Fatalf("agent auth failed: %+v", res)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			for {
				m, err := ag.recv(30 * time.Second)
				if err != nil || ctx.Err() != nil {
					return
				}
				if m.Type == xrpc.TypeCall {
					var call xrpc.Call
					if xrpc.Decode(m, &call) == nil {
						ag.send(xrpc.TypeReply, methods.Serve(ctx, call))
					}
				}
			}
		}()

		// 1. 端口扫描 + 站点校验（成功与不可达均返回 HTTP 200 结构化载荷）+ 域名延迟批量探测（超限拦截与正常返回）。
		scanOut, stderr, code := h.cli("server", "scan", "hk-net", "--host", "127.0.0.1", "--ports", "80", "--ports", "443", "--json")
		if code != 0 || !strings.Contains(scanOut, `"open_ports":[443]`) {
			t.Fatalf("server scan 异常 (%d): %s %s", code, scanOut, stderr)
		}
		valOut, stderr, code := h.cli("website", "validate", "hk-net", "--domain", "www.example.com", "--port", "443", "--json")
		if code != 0 || !strings.Contains(valOut, `"reality_suitable":true`) {
			t.Fatalf("website validate 异常 (%d): %s %s", code, valOut, stderr)
		}
		valFail, stderr, code := h.cli("website", "validate", "hk-net", "--domain", "www.example.com", "--port", "65432", "--json")
		if code != 0 || !strings.Contains(valFail, `"ok":false`) {
			t.Fatalf("不可达站点应返回退出码 0 与 ok:false (%d): %s %s", code, valFail, stderr)
		}
		if _, _, code := h.cli("server", "domain-latency", "hk-net", "--domains", "a.example.com", "--concurrency", "17", "--json"); code == 0 {
			t.Fatal("超 16 并发应返回非 0 退出码")
		}
		latOut, stderr, code := h.cli("server", "domain-latency", "hk-net", "--domains", "a.example.com", "--domains", "b.example.com", "--concurrency", "16", "--json")
		if code != 0 || !strings.Contains(latOut, `"ok_count":2`) {
			t.Fatalf("domain-latency 异常 (%d): %s %s", code, latOut, stderr)
		}

		// 2. WARP 完整闭环：未安装拒绝 outbound-plan -> install 长任务 -> authorize 绑定 license（审计打码）-> outbound-plan --anti-china -> apply -> status -> remove -> 离线导入。
		if _, _, code := h.cli("server", "warp", "outbound-plan", "hk-net", "--json"); code == 0 {
			t.Fatal("未安装 WARP 时 outbound-plan 应失败")
		}
		instOut, stderr, code := h.cli("server", "warp", "install", "hk-net", "--json")
		if code != 0 || strings.Contains(instOut, wgPriv) {
			t.Fatalf("server warp install 异常 (%d): %s %s", code, instOut, stderr)
		}
		var instJob struct {
			Status string `json:"status"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal([]byte(instOut), &instJob); err != nil || instJob.Status != "done" || !strings.Contains(instJob.Output, `"installed":true`) || !strings.Contains(instJob.Output, `"reserved":[11,22,33]`) {
			t.Fatalf("server warp install 长任务输出异常：%s (err=%v)", instOut, err)
		}
		const secretLicense = "WARP-PLUS-TOP-SECRET-999"
		authOut, stderr, code := h.cli("server", "warp", "authorize", "hk-net", "--license-key", secretLicense, "--json")
		if code != 0 || !strings.Contains(authOut, `"account_type":"plus"`) || strings.Contains(authOut, secretLicense) {
			t.Fatalf("server warp authorize 异常 (%d): %s %s", code, authOut, stderr)
		}
		// 审计日志中绝不能出现 license 明文。
		auditOut, stderr, code := h.cli("audit", "list", "--limit", "20", "--json")
		if code != 0 || strings.Contains(auditOut, secretLicense) {
			t.Fatalf("审计日志泄露了 WARP license 明文 (%d): %s %s", code, auditOut, stderr)
		}

		// 规划 WARP 出站 + 防送中路由并 apply，核对推送给节点的 kernel.json 含顶层 endpoints[] 与 geosite-google。
		wpOut, stderr, code := h.cli("server", "warp", "outbound-plan", "hk-net", "--anti-china", "--json")
		if code != 0 {
			t.Fatalf("server warp outbound-plan 失败 (%d): %s %s", code, wpOut, stderr)
		}
		var wpRes struct {
			PlanID int64 `json:"plan_id"`
		}
		if err := json.Unmarshal([]byte(wpOut), &wpRes); err != nil || wpRes.PlanID == 0 {
			t.Fatalf("解析 outbound-plan 失败：%v %s", err, wpOut)
		}
		if appOut, stderr, code := h.cli("apply", "--plan", strconv.FormatInt(wpRes.PlanID, 10), "--json"); code != 0 {
			t.Fatalf("apply WARP 计划失败 (%d): %s %s", code, appOut, stderr)
		}
		mu.Lock()
		kernelCfg := lastAppliedKernel
		mu.Unlock()
		if !strings.Contains(kernelCfg, `"endpoints"`) || !strings.Contains(kernelCfg, `"type": "wireguard"`) || !strings.Contains(kernelCfg, `"geosite-google"`) || !strings.Contains(kernelCfg, `"action": "sniff"`) {
			t.Fatalf("下发给节点的 kernel.json 缺少 WireGuard endpoint 或防送中规则：%s", kernelCfg)
		}

		stOut, stderr, code := h.cli("server", "warp", "status", "hk-net", "--json")
		if code != 0 || !strings.Contains(stOut, `"anti_china_rule_present":true`) || strings.Contains(stOut, wgPriv) {
			t.Fatalf("server warp status 异常 (%d): %s %s", code, stOut, stderr)
		}

		remOut, stderr, code := h.cli("server", "warp", "remove", "hk-net", "--confirm", "hk-net", "--json")
		if code != 0 || !strings.Contains(remOut, `"removed":true`) || !strings.Contains(remOut, `"warp"`) {
			t.Fatalf("server warp remove 异常 (%d): %s %s", code, remOut, stderr)
		}

		// 离线导入模式（同样验证私钥进审计打码）。
		impOut, stderr, code := h.cli("server", "warp", "authorize", "hk-net", "--private-key", wgPriv, "--address-v4", "172.16.0.9/32", "--reserved", "1,2,3", "--json")
		if code != 0 || !strings.Contains(impOut, `"mode":"import"`) || strings.Contains(impOut, wgPriv) {
			t.Fatalf("离线导入异常 (%d): %s %s", code, impOut, stderr)
		}
		auditOut2, _, _ := h.cli("audit", "list", "--limit", "10", "--json")
		if strings.Contains(auditOut2, wgPriv) {
			t.Fatalf("审计日志泄露了离线导入的 WireGuard 私钥：%s", auditOut2)
		}

		// 3. ReturnRoute：plan/apply 创建（含离线服务器）-> return-route test 长任务回写 status 不抬版本 -> snapshot rollback。
		srvGetOut, _, _ := h.cli("server", "get", "hk-net", "--json")
		var hkSrv struct {
			Metadata struct {
				ID int64 `json:"id"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal([]byte(srvGetOut), &hkSrv)
		offGetOut, _, _ := h.cli("server", "get", "hk-off", "--json")
		var offSrv struct {
			Metadata struct {
				ID int64 `json:"id"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal([]byte(offGetOut), &offSrv)

		rrSpec := fmt.Sprintf(`[
			{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"ct","enabled":true}},
			{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"cu","enabled":true}},
			{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"ct","enabled":true}}
		]`, hkSrv.Metadata.ID, hkSrv.Metadata.ID, offSrv.Metadata.ID)
		pRR, stderr, code := h.cli("plan", "--spec", rrSpec, "--json")
		if code != 0 {
			t.Fatalf("plan ReturnRoute 失败 (%d): %s %s", code, pRR, stderr)
		}
		var rrPlan struct {
			PlanID int64 `json:"plan_id"`
		}
		_ = json.Unmarshal([]byte(pRR), &rrPlan)
		if appOut, stderr, code := h.cli("apply", "--plan", strconv.FormatInt(rrPlan.PlanID, 10), "--json"); code != 0 {
			t.Fatalf("apply ReturnRoute（含离线机）失败 (%d): %s %s", code, appOut, stderr)
		}

		// 启动 return-route test 长任务并检查状态回写与 resourceVersion 未抬升。
		tOut, stderr, code := h.cli("return-route", "test", "hk-net", "--json")
		if code != 0 {
			t.Fatalf("return-route test 失败 (%d): %s %s", code, tOut, stderr)
		}
		var rrJob struct {
			Status string `json:"status"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal([]byte(tOut), &rrJob); err != nil || rrJob.Status != "done" || !strings.Contains(rrJob.Output, `"CN2 GIA"`) || !strings.Contains(rrJob.Output, `"AS9929"`) {
			t.Fatalf("return-route test 长任务输出异常：%s (err=%v)", tOut, err)
		}
		listOut, stderr, code := h.cli("return-route", "list", "--server-name", "hk-net", "--json")
		if code != 0 {
			t.Fatalf("return-route list 失败 (%d): %s %s", code, listOut, stderr)
		}
		var listPage struct {
			Total int `json:"total"`
			Items []struct {
				Metadata struct {
					ID              int64 `json:"id"`
					ResourceVersion int64 `json:"resourceVersion"`
				} `json:"metadata"`
				Spec struct {
					Carrier string `json:"carrier"`
				} `json:"spec"`
				Status struct {
					RouteType string     `json:"route_type"`
					EntryASN  string     `json:"entry_asn"`
					TestedAt  *time.Time `json:"tested_at"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(listOut), &listPage); err != nil || listPage.Total != 2 {
			t.Fatalf("解析 return-route list 失败：%v %s", err, listOut)
		}
		var ctID int64
		for _, it := range listPage.Items {
			if it.Metadata.ResourceVersion != 1 || it.Status.TestedAt == nil {
				t.Fatalf("测路后不应抬升版本且应写 tested_at：%+v", it)
			}
			if it.Spec.Carrier == "CT" {
				ctID = it.Metadata.ID
				if it.Status.RouteType != "CN2 GIA" || it.Status.EntryASN != "AS4809" {
					t.Fatalf("CT 状态不对：%+v", it.Status)
				}
			}
		}
		getOut, stderr, code := h.cli("return-route", "get", strconv.FormatInt(ctID, 10), "--json")
		if code != 0 || !strings.Contains(getOut, `"CN2 GIA"`) {
			t.Fatalf("return-route get 失败 (%d): %s %s", code, getOut, stderr)
		}
	})
}
