package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corebatch "github.com/satchel/satchel/internal/core/batch"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	coreplans "github.com/satchel/satchel/internal/core/plans"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	coresnapshots "github.com/satchel/satchel/internal/core/snapshots"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type orderedFakeNodes struct {
	*fakeNodes
	mu             sync.Mutex
	order          []string
	lastConfigCall *xrpc.ConfigApplyParams
	lastNginxCall  *xrpc.NginxWebsitesApplyParams
}

func newOrderedFakeNodes() *orderedFakeNodes {
	return &orderedFakeNodes{fakeNodes: newFakeNodes()}
}

func (f *orderedFakeNodes) CallConfigApply(ctx context.Context, serverID int64, params xrpc.ConfigApplyParams) (*xrpc.ConfigApplyResult, error) {
	f.mu.Lock()
	f.order = append(f.order, "config")
	p := params
	f.lastConfigCall = &p
	f.mu.Unlock()
	return f.fakeNodes.CallConfigApply(ctx, serverID, params)
}

func (f *orderedFakeNodes) CallNginxWebsitesApply(_ context.Context, _ int64, params xrpc.NginxWebsitesApplyParams) (*xrpc.NginxWebsitesApplyResult, error) {
	f.mu.Lock()
	f.order = append(f.order, "nginx")
	p := params
	f.lastNginxCall = &p
	f.mu.Unlock()
	res := &xrpc.NginxWebsitesApplyResult{
		Applied:  []xrpc.AppliedWebsiteFile{},
		Removed:  params.RemoveDomains,
		Reloaded: true,
	}
	if params.StealSite != nil {
		res.StealApplied = true
		res.StealConfPath = nginx.ManagedStealConfPath()
	}
	if params.RemoveSteal {
		res.StealRemoved = true
		res.StealConfPath = nginx.ManagedStealConfPath()
	}
	return res, nil
}

func (f *orderedFakeNodes) resetOrder() {
	f.mu.Lock()
	f.order = nil
	f.mu.Unlock()
}

func (f *orderedFakeNodes) getOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func TestSameHost443KernelGuardAndPendingDomainMatch(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := setRepo.Write(ctx, coresettings.WriteRequest{
			Values:          map[string]any{"master_url": "https://panel.example.com"},
			ExpectedVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		srvRepo := coreservers.New(bdb, st)
		nodes := newOrderedFakeNodes()
		svc := New(Deps{
			DB:           bdb,
			Servers:      srvRepo,
			Inbounds:     coreinbounds.New(bdb, st),
			Outbounds:    coreoutbounds.New(bdb, st),
			RoutingRules: corerouting.New(bdb, st),
			Websites:     corewebsites.New(bdb, st),
			Plans:        coreplans.New(bdb, st),
			Snapshots:    coresnapshots.New(bdb, st),
			Batch:        corebatch.New(bdb, st),
			Settings:     setRepo,
			Nodes:        nodes,
			Now:          func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) },
		})
		admin := v1.WithIdentity(ctx, v1.LocalAdmin("root"))

		// 1. same_host_as_master=true 节点禁止启用 use_443 与 443 入站，但非 443 入站放行并透传 protect_master_443=true。
		localSrv, err := srvRepo.Create(ctx, "local-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		if err := srvRepo.SetSameHost(ctx, localSrv.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}
		_, err = svc.planCmd(admin, &command.Invocation{
			Flags: map[string]any{
				"spec": fmt.Sprintf(`{"op":"update","kind":"Server","id":%d,"resourceVersion":%d,"spec":{"use_443":true,"domain":"decoy.example.com"}}`, localSrv.Metadata.ID, localSrv.Metadata.ResourceVersion),
			},
		})
		if err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("同机节点开启 use_443 应报 conflict，得到：%v", err)
		}

		_, err = svc.planCmd(admin, &command.Invocation{
			Flags: map[string]any{
				"spec": fmt.Sprintf(`{"op":"create","kind":"Inbound","spec":{"server_id":%d,"tag":"in-443","protocol":"direct","port":443,"enabled":true}}`, localSrv.Metadata.ID),
			},
		})
		if err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("同机节点新建 443 入站应报 conflict，得到：%v", err)
		}

		// 非 443 入站可 plan + apply，且 config.apply 携带 protect_master_443=true。
		pvAny, err := svc.planCmd(admin, &command.Invocation{
			Flags: map[string]any{
				"spec": fmt.Sprintf(`{"op":"create","kind":"Inbound","spec":{"server_id":%d,"tag":"in-18443","protocol":"direct","port":18443,"enabled":true}}`, localSrv.Metadata.ID),
			},
		})
		if err != nil {
			t.Fatalf("同机非 443 入站 plan 失败：%v", err)
		}
		pv := pvAny.(*PlanView)
		if _, err := svc.applyCmd(admin, &command.Invocation{Flags: map[string]any{"plan": int(pv.PlanID)}}); err != nil {
			t.Fatalf("同机非 443 入站 apply 失败：%v", err)
		}
		if nodes.lastConfigCall == nil || !nodes.lastConfigCall.ProtectMaster443 {
			t.Fatalf("同机节点 config.apply 应携带 protect_master_443=true，实际：%+v", nodes.lastConfigCall)
		}

		// 2. pending 服务器域名撞 https:// master_url（panel.example.com）同样拒绝 443 入站与 use_443=true。
		pendSrv, err := srvRepo.Create(ctx, "pend-same", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		// 先把 pending 节点 domain 设为 panel.example.com（use_443=false 允许）。
		pvDomAny, err := svc.planCmd(admin, &command.Invocation{
			Flags: map[string]any{
				"spec": fmt.Sprintf(`{"op":"update","kind":"Server","id":%d,"resourceVersion":%d,"spec":{"domain":"panel.example.com"}}`, pendSrv.Metadata.ID, pendSrv.Metadata.ResourceVersion),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		pvDom := pvDomAny.(*PlanView)
		if _, err := svc.applyCmd(admin, &command.Invocation{
			Flags:   map[string]any{"plan": int(pvDom.PlanID)},
			Confirm: pvDom.ConfirmHint,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.planCmd(admin, &command.Invocation{
			Flags: map[string]any{
				"spec": fmt.Sprintf(`{"op":"create","kind":"Inbound","spec":{"server_id":%d,"tag":"pend-443","protocol":"direct","port":443,"enabled":true}}`, pendSrv.Metadata.ID),
			},
		}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("pending 撞主控域名服务器新建 443 入站应报 conflict，得到：%v", err)
		}
	})
}

func TestStealNginxPreviewOrderAndSnapshotRollback(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		srvRepo := coreservers.New(bdb, st)
		nodes := newOrderedFakeNodes()
		svc := New(Deps{
			DB:           bdb,
			Servers:      srvRepo,
			Inbounds:     coreinbounds.New(bdb, st),
			Outbounds:    coreoutbounds.New(bdb, st),
			RoutingRules: corerouting.New(bdb, st),
			Websites:     corewebsites.New(bdb, st),
			Plans:        coreplans.New(bdb, st),
			Snapshots:    coresnapshots.New(bdb, st),
			Batch:        corebatch.New(bdb, st),
			Settings:     setRepo,
			Nodes:        nodes,
			Now:          func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) },
		})
		admin := v1.WithIdentity(ctx, v1.LocalAdmin("root"))

		srv, err := srvRepo.Create(ctx, "hk-steal", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}

		// 1. 同时开启 use_443=true、改 core_log_level="info"、建一条 WS 入站与一条 direct 443 入站：
		//    - plan 预览 steal_config 含 WS location 且不含 proxy_protocol；
		//    - apply 必须先调 nginx.websites.apply 再调 config.apply！
		specJSON := fmt.Sprintf(`[
			{"op":"update","kind":"Server","id":%d,"resourceVersion":%d,"spec":{"use_443":true,"steal_mode":"tunnel","domain":"decoy.example.com","site_type":"decoy","site_value":"landing","core_log_level":"info"}},
			{"op":"create","kind":"Inbound","spec":{"server_id":%d,"tag":"tunnel-in","protocol":"direct","port":443,"settings":{"network":"tcp"},"enabled":true}},
			{"op":"create","kind":"Inbound","spec":{"server_id":%d,"tag":"ws-in","protocol":"vless","port":20001,"listen":"127.0.0.1","transport":{"type":"ws","path":"/ray"},"settings":{"users":[{"name":"u1","uuid":"6a4f1c2e-3b5d-4e7f-8a9b-0c1d2e3f4a5b"}]},"enabled":true}}
		]`, srv.Metadata.ID, srv.Metadata.ResourceVersion, srv.Metadata.ID, srv.Metadata.ID)

		pvAny, err := svc.planCmd(admin, &command.Invocation{Flags: map[string]any{"spec": specJSON}})
		if err != nil {
			t.Fatalf("开启 steal plan 失败：%v", err)
		}
		pv := pvAny.(*PlanView)
		if len(pv.Servers) != 1 || !pv.Servers[0].StealChanged || pv.Servers[0].StealConfig == nil {
			t.Fatalf("plan 应包含 steal_config 预览：%+v", pv.Servers)
		}
		sc := pv.Servers[0].StealConfig
		if sc.Action != "apply" || sc.ConfPath != nginx.ManagedStealConfPath() || !strings.Contains(sc.Conf, "location /ray {") || strings.Contains(sc.Conf, "proxy_protocol") {
			t.Fatalf("steal_config 预览内容异常：%+v", sc)
		}

		nodes.resetOrder()
		arAny, err := svc.applyCmd(admin, &command.Invocation{
			Flags:   map[string]any{"plan": int(pv.PlanID)},
			Confirm: pv.ConfirmHint,
		})
		if err != nil {
			t.Fatalf("开启 steal apply 失败：%v", err)
		}
		ar := arAny.(*ApplyResult)
		order := nodes.getOrder()
		if len(order) != 2 || order[0] != "nginx" || order[1] != "config" {
			t.Fatalf("开启 use_443 必须先推 nginx 再推 config，实际顺序：%v", order)
		}
		if len(ar.Servers) != 1 || !ar.Servers[0].StealApplied || ar.Servers[0].StealRemoved {
			t.Fatalf("ApplyResult 应标记 StealApplied=true：%+v", ar.Servers)
		}

		// 2. snapshot rollback 回滚开启前的 Server 快照（use_443=false）：
		//    - apply 必须先推 config 再推 nginx(remove_steal: true)！
		//    同时删掉 tunnel-in 模拟完整关闭顺序。
		curSrv, err := srvRepo.ByID(ctx, srv.Metadata.ID)
		if err != nil {
			t.Fatal(err)
		}
		rbPlanAny, err := svc.snapshotRollback(admin, &command.Invocation{Flags: map[string]any{"apply-id": ar.ApplyID}})
		if err != nil {
			t.Fatalf("snapshot rollback 失败：%v", err)
		}
		rbPV := rbPlanAny.(*PlanView)
		if len(rbPV.Servers) != 1 || !rbPV.Servers[0].StealChanged || rbPV.Servers[0].StealConfig == nil || rbPV.Servers[0].StealConfig.Action != "remove" {
			t.Fatalf("回滚计划应包含 steal remove 预览：%+v", rbPV.Servers)
		}
		// 校验原 Server version 确已递增
		if curSrv.Metadata.ResourceVersion != 2 {
			t.Fatalf("Server version 期望 2，得到 %d", curSrv.Metadata.ResourceVersion)
		}

		nodes.resetOrder()
		rbArAny, err := svc.applyCmd(admin, &command.Invocation{
			Flags:   map[string]any{"plan": int(rbPV.PlanID)},
			Confirm: rbPV.ConfirmHint,
		})
		if err != nil {
			t.Fatalf("回滚 apply 失败：%v", err)
		}
		rbAr := rbArAny.(*ApplyResult)
		order2 := nodes.getOrder()
		if len(order2) != 2 || order2[0] != "config" || order2[1] != "nginx" {
			t.Fatalf("关闭 use_443 必须先推 config 再推 nginx remove_steal，实际顺序：%v", order2)
		}
		if len(rbAr.Servers) != 1 || !rbAr.Servers[0].StealRemoved || rbAr.Servers[0].StealApplied {
			t.Fatalf("回滚结果应标记 StealRemoved=true：%+v", rbAr.Servers)
		}
		if nodes.lastNginxCall == nil || !nodes.lastNginxCall.RemoveSteal {
			t.Fatalf("关闭 use_443 时 nginx.websites.apply 应传 remove_steal=true：%+v", nodes.lastNginxCall)
		}

		// 确认回滚后的 Server 确实回到 use_443=false
		afterSrv, err := srvRepo.ByID(ctx, srv.Metadata.ID)
		if err != nil {
			t.Fatal(err)
		}
		if afterSrv.Spec.Use443 {
			b, _ := json.Marshal(afterSrv.Spec)
			t.Fatalf("回滚后 use_443 应为 false：%s", b)
		}
	})
}
