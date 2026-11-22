package network

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	corereturnroutes "github.com/satchel/satchel/internal/core/returnroutes"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type fakeNodes struct {
	warpInstalled bool
	warpCreds     *xrpc.WarpCredentialsResult
	lastScan      xrpc.NetworkPortScanParams
	lastValidate  xrpc.NetworkValidateSiteParams
	lastLatency   xrpc.NetworkDomainLatencyParams
	rrByCarrier   map[string]*xrpc.NetworkReturnRouteTestResult
}

func (f *fakeNodes) CallNetworkPortScan(_ context.Context, _ int64, p xrpc.NetworkPortScanParams) (*xrpc.NetworkPortScanResult, error) {
	f.lastScan = p
	return &xrpc.NetworkPortScanResult{
		Host: p.Host, ScannedCount: len(p.Ports), OpenPorts: []int{443}, ClosedCount: len(p.Ports) - 1,
		Results: []xrpc.PortScanItem{{Port: 443, Open: true, LatencyMS: 2}},
	}, nil
}

func (f *fakeNodes) CallNetworkValidateSite(_ context.Context, _ int64, p xrpc.NetworkValidateSiteParams) (*xrpc.NetworkValidateSiteResult, error) {
	f.lastValidate = p
	return &xrpc.NetworkValidateSiteResult{
		Domain: p.Domain, Port: p.Port, SNI: p.SNI, OK: true, Reachable: true,
		TLSVersion: "TLS 1.3", ALPN: "h2", HTTP2: true, CertValid: true, RealitySuitable: true,
	}, nil
}

func (f *fakeNodes) CallNetworkDomainLatency(_ context.Context, _ int64, p xrpc.NetworkDomainLatencyParams) (*xrpc.NetworkDomainLatencyResult, error) {
	f.lastLatency = p
	items := make([]xrpc.DomainLatencyItem, len(p.Domains))
	for i, d := range p.Domains {
		items[i] = xrpc.DomainLatencyItem{Domain: d, OK: true, LatencyMS: int64(10 + i)}
	}
	return &xrpc.NetworkDomainLatencyResult{
		Mode: p.Mode, Port: p.Port, Total: len(items), OKCount: len(items), Results: items, Fastest: items,
	}, nil
}

func (f *fakeNodes) CallNetworkReturnRouteTest(_ context.Context, _ int64, p xrpc.NetworkReturnRouteTestParams) (*xrpc.NetworkReturnRouteTestResult, error) {
	if r, ok := f.rrByCarrier[p.Carrier]; ok {
		return r, nil
	}
	return &xrpc.NetworkReturnRouteTestResult{
		Carrier: p.Carrier, TargetIP: "202.97.1.1", RouteType: "Unknown", Reason: "no match", TestedAt: time.Now().UTC(),
	}, nil
}

func (f *fakeNodes) CallWarpStatus(_ context.Context, _ int64) (*xrpc.WarpStatusResult, error) {
	return &xrpc.WarpStatusResult{
		Installed: f.warpInstalled, AccountType: "plus", Endpoint: xrpc.DefaultWarpEndpoint, AddressV4: "172.16.0.2/32", MTU: 1280,
	}, nil
}

func (f *fakeNodes) CallWarpInstall(_ context.Context, _ int64, p xrpc.WarpInstallParams) (*xrpc.WarpInstallResult, error) {
	f.warpInstalled = true
	return &xrpc.WarpInstallResult{
		WarpStatusResult: xrpc.WarpStatusResult{
			Installed: true, AccountType: "free", DeviceID: "dev-1", Endpoint: xrpc.DefaultWarpEndpoint, AddressV4: "172.16.0.2/32", MTU: 1280,
		},
	}, nil
}

func (f *fakeNodes) CallWarpAuthorize(_ context.Context, _ int64, p xrpc.WarpAuthorizeParams) (*xrpc.WarpAuthorizeResult, error) {
	f.warpInstalled = true
	mode := "license"
	if p.PrivateKey != "" {
		mode = "import"
	}
	return &xrpc.WarpAuthorizeResult{
		WarpStatusResult: xrpc.WarpStatusResult{
			Installed: true, AccountType: "plus", HasLicense: p.LicenseKey != "", Endpoint: xrpc.DefaultWarpEndpoint, AddressV4: "172.16.0.2/32", MTU: 1280,
		},
		Mode: mode,
	}, nil
}

func (f *fakeNodes) CallWarpRemove(_ context.Context, _ int64) (*xrpc.WarpRemoveResult, error) {
	f.warpInstalled = false
	return &xrpc.WarpRemoveResult{Removed: true}, nil
}

func (f *fakeNodes) CallWarpCredentials(_ context.Context, _ int64) (*xrpc.WarpCredentialsResult, error) {
	if !f.warpInstalled || f.warpCreds == nil {
		return nil, v1.New(v1.CodeConflict, "节点未安装 WARP")
	}
	return f.warpCreds, nil
}

type fakePlanner struct {
	lastJSON string
	lastOps  []struct {
		Op   string          `json:"op"`
		Kind string          `json:"kind"`
		ID   int64           `json:"id"`
		Spec json.RawMessage `json:"spec"`
	}
}

func (p *fakePlanner) CreatePlanFromJSON(_ context.Context, specJSON, _, _ string) (any, error) {
	p.lastJSON = specJSON
	p.lastOps = nil
	if err := json.Unmarshal([]byte(specJSON), &p.lastOps); err != nil {
		return nil, err
	}
	return map[string]any{"plan_id": 42, "ops": len(p.lastOps)}, nil
}

func TestNetworkProbesWarpAndReturnRoutes(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		srvRepo := coreservers.New(bdb, st)
		inRepo := coreinbounds.New(bdb, st)
		outRepo := coreoutbounds.New(bdb, st)
		rrRepo := corerouting.New(bdb, st)
		retRepo := corereturnroutes.New(bdb, st)

		nodes := &fakeNodes{
			rrByCarrier: map[string]*xrpc.NetworkReturnRouteTestResult{
				"CT": {Carrier: "CT", TargetIP: "202.97.94.229", RouteType: "CN2 GIA", Region: "Shanghai", EntryIP: "59.43.130.1", EntryASN: "AS4809", TestedAt: time.Now().UTC()},
				"CU": {Carrier: "CU", TargetIP: "210.22.97.1", RouteType: "AS9929", Region: "Shanghai", EntryIP: "218.105.1.1", EntryASN: "AS9929", TestedAt: time.Now().UTC()},
			},
		}
		planner := &fakePlanner{}
		svc := New(Deps{
			Servers:      srvRepo,
			Inbounds:     inRepo,
			Outbounds:    outRepo,
			RoutingRules: rrRepo,
			ReturnRoutes: retRepo,
			Nodes:        nodes,
			Planner:      planner,
			StartJob: func(jctx context.Context, _ int64, _ *command.Invocation, run func(context.Context) (any, error), done func()) (any, error) {
				if done != nil {
					defer done()
				}
				return run(jctx)
			},
		})
		b := svc.Bindings()
		admin := v1.WithIdentity(ctx, v1.LocalAdmin("root"))
		user := v1.WithIdentity(ctx, v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead}})

		srv, err := srvRepo.Create(ctx, "hk-net", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}

		// 1. 普通用户全部报 forbidden。
		for name, fn := range b {
			if _, err := fn(user, &command.Invocation{Args: []string{"hk-net"}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeForbidden {
				t.Fatalf("命令 %s 应对普通用户报 forbidden，得到：%v", name, err)
			}
		}

		// 2. 三项网络探测 + 硬上限拦截。
		if _, err := b["server scan"](admin, &command.Invocation{
			Args:  []string{"hk-net"},
			Flags: map[string]any{"ports": []string{"443", "8000-8002"}, "concurrency": 8},
		}); err != nil {
			t.Fatalf("server scan 失败：%v", err)
		}
		if len(nodes.lastScan.Ports) != 4 {
			t.Fatalf("期望 4 个端口，得到 %v", nodes.lastScan.Ports)
		}
		if _, err := b["server scan"](admin, &command.Invocation{
			Args:  []string{"hk-net"},
			Flags: map[string]any{"ports": []string{"1-300"}},
		}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("超过 256 端口应报 bad_request，得到：%v", err)
		}

		if _, err := b["website validate"](admin, &command.Invocation{
			Args:  []string{"hk-net"},
			Flags: map[string]any{"domain": "www.example.com", "port": 443},
		}); err != nil {
			t.Fatalf("website validate 失败：%v", err)
		}

		over200 := make([]string, 201)
		for i := range over200 {
			over200[i] = fmt.Sprintf("h%d.example.com", i)
		}
		if _, err := b["server domain-latency"](admin, &command.Invocation{
			Args:  []string{"hk-net"},
			Flags: map[string]any{"domains": over200},
		}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("超过 200 域名应报 bad_request，得到：%v", err)
		}
		if _, err := b["server domain-latency"](admin, &command.Invocation{
			Args:  []string{"hk-net"},
			Flags: map[string]any{"domains": []string{"a.example.com"}, "concurrency": 17},
		}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("超过 16 并发应报 bad_request，得到：%v", err)
		}
		if _, err := b["server domain-latency"](admin, &command.Invocation{
			Args:  []string{"hk-net"},
			Flags: map[string]any{"domains": []string{"a.example.com", "b.example.com", "a.example.com"}, "concurrency": 16},
		}); err != nil {
			t.Fatalf("server domain-latency 失败：%v", err)
		}
		if len(nodes.lastLatency.Domains) != 2 {
			t.Fatalf("去重后应有 2 个域名，得到 %v", nodes.lastLatency.Domains)
		}

		// 3. WARP 全链路：未安装拒绝 outbound-plan -> install 写 warp_installed(不抬版本) -> outbound-plan --anti-china -> singbox.Render 验证 -> remove 提示残留。
		if _, err := b["server warp outbound-plan"](admin, &command.Invocation{Args: []string{"hk-net"}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("未安装 WARP 时 outbound-plan 应报 conflict，得到：%v", err)
		}
		if _, err := b["server warp install"](admin, &command.Invocation{Args: []string{"hk-net"}}); err != nil {
			t.Fatalf("server warp install 失败：%v", err)
		}
		srvAfter, _ := srvRepo.ByID(ctx, srv.Metadata.ID)
		if !srvAfter.Status.WarpInstalled || srvAfter.Metadata.ResourceVersion != 1 {
			t.Fatalf("install 后应置 warp_installed=true 且不抬版本：ver=%d warp=%v", srvAfter.Metadata.ResourceVersion, srvAfter.Status.WarpInstalled)
		}
		wgPriv, _, err := singbox.GenerateWireGuardKeypair()
		if err != nil {
			t.Fatal(err)
		}
		nodes.warpCreds = &xrpc.WarpCredentialsResult{
			Installed:     true,
			PrivateKey:    wgPriv,
			PeerPublicKey: xrpc.DefaultWarpPeerPublicKey,
			Endpoint:      xrpc.DefaultWarpEndpoint,
			AddressV4:     "172.16.0.2/32",
			AddressV6:     "2606:4700:110:8a36::1/128",
			Reserved:      []int{12, 34, 56},
			MTU:           1280,
		}
		if _, err := b["server warp outbound-plan"](admin, &command.Invocation{
			Args:  []string{"hk-net"},
			Flags: map[string]any{"anti-china": true},
		}); err != nil {
			t.Fatalf("server warp outbound-plan 失败：%v", err)
		}
		if len(planner.lastOps) != 2 || planner.lastOps[0].Kind != coreoutbounds.KindName || planner.lastOps[1].Kind != corerouting.KindName {
			t.Fatalf("outbound-plan --anti-china 应生成 2 条 ops（Outbound+RoutingRule）：%s", planner.lastJSON)
		}
		// 把规划出来的 Outbound + RoutingRule 直接交给 singbox.Render 验证顶层 endpoints[] 与 rule_set 合成。
		var obSpec v1.OutboundSpec
		var rrSpec v1.RoutingRuleSpec
		if err := json.Unmarshal(planner.lastOps[0].Spec, &obSpec); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(planner.lastOps[1].Spec, &rrSpec); err != nil {
			t.Fatal(err)
		}
		rendered, err := singbox.Render(singbox.ServerInput{
			Server:       srvAfter.Spec,
			Outbounds:    []v1.OutboundSpec{obSpec},
			RoutingRules: []v1.RoutingRuleSpec{rrSpec},
		})
		if err != nil {
			t.Fatalf("WARP Outbound + 防送中规则渲染失败：%v", err)
		}
		if !strings.Contains(string(rendered.Canonical), `"endpoints"`) || !strings.Contains(string(rendered.Canonical), `"geosite-google"`) {
			t.Fatalf("渲染结果缺少 endpoints 或 geosite-google：%s", string(rendered.Canonical))
		}
		// 落库一条 is_warp 出站后测 warp status 与 warp remove 的残留提示。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := outRepo.CreateTx(ctx, tx, obSpec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		stAny, err := b["server warp status"](admin, &command.Invocation{Args: []string{"hk-net"}})
		if err != nil {
			t.Fatalf("server warp status 失败：%v", err)
		}
		if len(stAny.(*WarpStatusView).WarpOutbounds) != 1 {
			t.Fatalf("warp status 应列出 1 条 warp 出站：%+v", stAny)
		}
		remAny, err := b["server warp remove"](admin, &command.Invocation{Args: []string{"hk-net"}})
		if err != nil {
			t.Fatalf("server warp remove 失败：%v", err)
		}
		if len(remAny.(*WarpRemoveView).RemainingWarpOutbounds) != 1 || remAny.(*WarpRemoveView).RemainingWarpOutbounds[0] != "warp" {
			t.Fatalf("warp remove 应提示残留 warp 出站：%+v", remAny)
		}
		srvAfterRem, _ := srvRepo.ByID(ctx, srv.Metadata.ID)
		if srvAfterRem.Status.WarpInstalled || srvAfterRem.Metadata.ResourceVersion != 1 {
			t.Fatalf("remove 后应置 warp_installed=false 且不抬版本：ver=%d warp=%v", srvAfterRem.Metadata.ResourceVersion, srvAfterRem.Status.WarpInstalled)
		}

		// 4. ReturnRoute list / get / test（回写 status 不抬升 resourceVersion）。
		var rrCT, rrCU *corereturnroutes.ReturnRoute
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			rrCT, err = retRepo.CreateTx(ctx, tx, v1.ReturnRouteSpec{ServerID: srv.Metadata.ID, Carrier: "CT", Enabled: true})
			if err != nil {
				return err
			}
			rrCU, err = retRepo.CreateTx(ctx, tx, v1.ReturnRouteSpec{ServerID: srv.Metadata.ID, Carrier: "CU", Enabled: true})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		testAny, err := b["return-route test"](admin, &command.Invocation{Args: []string{"hk-net"}})
		if err != nil {
			t.Fatalf("return-route test 失败：%v", err)
		}
		tv := testAny.(*ReturnRouteTestView)
		if tv.Total != 2 {
			t.Fatalf("应测试 2 条已启用返程线路，实际 %d", tv.Total)
		}
		gotCT, err := b["return-route get"](admin, &command.Invocation{Args: []string{fmt.Sprintf("%d", rrCT.Metadata.ID)}})
		if err != nil {
			t.Fatalf("return-route get 失败：%v", err)
		}
		ctObj := gotCT.(*corereturnroutes.ReturnRoute)
		if ctObj.Metadata.ResourceVersion != 1 || ctObj.Status.RouteType != "CN2 GIA" || ctObj.Status.EntryAsn != "AS4809" || ctObj.Status.TestedAt == nil {
			t.Fatalf("CT 回写状态异常或抬升了版本：%+v", ctObj)
		}
		listAny, err := b["return-route list"](admin, &command.Invocation{Flags: map[string]any{"server-name": "hk-net"}})
		if err != nil || listAny.(*command.PageResult).Total != 2 || rrCU.Metadata.ID == 0 {
			t.Fatalf("return-route list 异常：err=%v page=%+v", err, listAny)
		}
	})
}
