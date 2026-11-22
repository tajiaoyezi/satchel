package apply

import (
	"context"
	"encoding/json"
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

type fakeWebsiteApplyCall struct {
	ServerID int64
	Params   xrpc.NginxWebsitesApplyParams
}

type fakeNodesWithWebsites struct {
	*fakeNodes
	wmu      sync.Mutex
	wcalls   []fakeWebsiteApplyCall
	whandler func(serverID int64, p xrpc.NginxWebsitesApplyParams) (*xrpc.NginxWebsitesApplyResult, error)
}

func newFakeNodesWithWebsites() *fakeNodesWithWebsites {
	return &fakeNodesWithWebsites{fakeNodes: newFakeNodes()}
}

func (f *fakeNodesWithWebsites) CallNginxWebsitesApply(_ context.Context, serverID int64, params xrpc.NginxWebsitesApplyParams) (*xrpc.NginxWebsitesApplyResult, error) {
	f.wmu.Lock()
	f.wcalls = append(f.wcalls, fakeWebsiteApplyCall{ServerID: serverID, Params: params})
	h := f.whandler
	f.wmu.Unlock()
	if h != nil {
		return h(serverID, params)
	}
	applied := make([]xrpc.AppliedWebsiteFile, 0, len(params.Sites))
	for _, s := range params.Sites {
		applied = append(applied, xrpc.AppliedWebsiteFile{
			Domain:   s.Domain,
			ConfPath: nginx.ManagedConfPath(s.Domain),
		})
	}
	return &xrpc.NginxWebsitesApplyResult{
		Applied:  applied,
		Removed:  params.RemoveDomains,
		Reloaded: true,
	}, nil
}

type stubVerifier struct {
	mu       sync.Mutex
	calls    int
	expected string
}

func (v *stubVerifier) Verify(_ context.Context, inv *command.Invocation) error {
	v.mu.Lock()
	v.calls++
	v.mu.Unlock()
	if inv == nil || inv.Verify == nil || inv.Verify.Password != v.expected {
		return v1.New(v1.CodeHumanRequired, "需要当场提供正确的管理员密码")
	}
	return nil
}

type websiteFixture struct {
	svc       *Service
	servers   *coreservers.Repo
	websites  *corewebsites.Repo
	snapshots *coresnapshots.Repo
	nodes     *fakeNodesWithWebsites
	verifier  *stubVerifier
	now       time.Time
	admin     context.Context
	token     context.Context
}

func setupWebsiteFixture(t *testing.T, bdb *bun.DB) *websiteFixture {
	t.Helper()
	st := store.New(bdb, schema.Default())
	setRepo := coresettings.New(bdb, st, schema.Default())
	if err := setRepo.EnsureSingleton(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &websiteFixture{
		servers:   coreservers.New(bdb, st),
		websites:  corewebsites.New(bdb, st),
		snapshots: coresnapshots.New(bdb, st),
		nodes:     newFakeNodesWithWebsites(),
		verifier:  &stubVerifier{expected: "secret-pass"},
		now:       time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
		admin:     v1.WithIdentity(context.Background(), v1.LocalAdmin("root")),
		token: v1.WithIdentity(context.Background(), v1.Identity{
			Actor:     "ci-bot",
			ActorKind: v1.ActorToken,
			Role:      v1.RoleAdmin,
			Scopes:    []v1.Scope{v1.ScopeRead, v1.ScopeOperate},
			Danger:    []v1.Danger{v1.DangerDelete, v1.DangerPermission, v1.DangerBatch},
		}),
	}
	f.svc = New(Deps{
		DB:           bdb,
		Servers:      f.servers,
		Inbounds:     coreinbounds.New(bdb, st),
		Outbounds:    coreoutbounds.New(bdb, st),
		RoutingRules: corerouting.New(bdb, st),
		Websites:     f.websites,
		Plans:        coreplans.New(bdb, st),
		Snapshots:    f.snapshots,
		Batch:        corebatch.New(bdb, st),
		Settings:     setRepo,
		Nodes:        f.nodes,
		Verifier:     f.verifier,
		Now:          func() time.Time { return f.now },
	})
	return f
}

func (f *websiteFixture) invoke(ctx context.Context, name string, inv *command.Invocation) (any, error) {
	if inv == nil {
		inv = &command.Invocation{}
	}
	inv.Path = strings.Split(name, " ")
	if inv.Flags == nil {
		inv.Flags = map[string]any{}
	}
	return f.svc.Bindings()[name](ctx, inv)
}

func TestWebsitePlanApplyRollbackAndLifecycle(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setupWebsiteFixture(t, bdb)
		srv, err := f.servers.Create(context.Background(), "hk-web-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}

		// 1. 远程节点 API 令牌直接 plan + apply 创建 WSS 反代站：无需人验、不触发 sing-box config.apply、回写 managed/conf_path 不抬升版本。
		createSpec, _ := json.Marshal(map[string]any{
			"op":   "create",
			"kind": "Website",
			"spec": map[string]any{
				"server_id": srv.Metadata.ID,
				"domain":    "App.Example.COM",
				"type":      "proxy",
				"target":    "wss://127.0.0.1:10000/ws",
			},
		})
		rawPlan, err := f.invoke(f.token, "plan", &command.Invocation{Flags: map[string]any{"spec": string(createSpec)}})
		if err != nil {
			t.Fatalf("plan create website failed: %v", err)
		}
		pv := rawPlan.(*PlanView)
		if pv.RequiresHuman {
			t.Fatalf("remote website plan must not set requires_human")
		}
		if len(pv.Servers) != 1 || !pv.Servers[0].WebsiteChanged || pv.Servers[0].ConfigChanged {
			t.Fatalf("unexpected server diff for website-only plan: %+v", pv.Servers)
		}
		if len(pv.Servers[0].Websites) != 1 || !strings.Contains(pv.Servers[0].Websites[0].Conf, xrpc.SatchelManagedHeader) {
			t.Fatalf("expected managed nginx conf preview, got %+v", pv.Servers[0].Websites)
		}

		rawApply, err := f.invoke(f.token, "apply", &command.Invocation{Flags: map[string]any{"plan": int(pv.PlanID)}})
		if err != nil {
			t.Fatalf("apply create website failed: %v", err)
		}
		ar := rawApply.(*ApplyResult)
		if len(ar.Objects) != 1 || ar.Objects[0].Kind != "Website" || ar.Objects[0].ResourceVersion != 1 {
			t.Fatalf("unexpected applied objects: %+v", ar.Objects)
		}
		siteID := ar.Objects[0].ID
		if len(f.nodes.calls) != 0 {
			t.Fatalf("website-only apply must not invoke kernel config.apply, got %d calls", len(f.nodes.calls))
		}
		if len(f.nodes.wcalls) != 1 || f.nodes.wcalls[0].Params.ProtectMaster443 {
			t.Fatalf("unexpected nginx.websites.apply calls: %+v", f.nodes.wcalls)
		}
		if len(ar.Servers) != 1 || !ar.Servers[0].NginxReloaded || len(ar.Servers[0].WebsitesApplied) != 1 {
			t.Fatalf("unexpected server apply result: %+v", ar.Servers)
		}

		got, err := f.websites.ByID(context.Background(), siteID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata.ResourceVersion != 1 || !got.Status.Managed || got.Status.ConfPath != "/etc/nginx/servers/satchel-site-app.example.com.conf" {
			t.Fatalf("status update must not bump version: ver=%d managed=%v path=%q", got.Metadata.ResourceVersion, got.Status.Managed, got.Status.ConfPath)
		}

		// 2. 更新 target -> 产生写前快照并推送新配置，再经 snapshot rollback 恢复原 target（版本递增不倒退）。
		updSpec, _ := json.Marshal(map[string]any{
			"op":              "update",
			"kind":            "Website",
			"id":              siteID,
			"resourceVersion": got.Metadata.ResourceVersion,
			"spec":            map[string]any{"target": "http://127.0.0.1:8080"},
		})
		updPlanRaw, err := f.invoke(f.token, "plan", &command.Invocation{Flags: map[string]any{"spec": string(updSpec)}})
		if err != nil {
			t.Fatalf("plan update failed: %v", err)
		}
		updApplyRaw, err := f.invoke(f.token, "apply", &command.Invocation{Flags: map[string]any{"plan": int(updPlanRaw.(*PlanView).PlanID)}})
		if err != nil {
			t.Fatalf("apply update failed: %v", err)
		}
		updApply := updApplyRaw.(*ApplyResult)
		snaps, err := f.snapshots.ListByApplyID(context.Background(), updApply.ApplyID)
		if err != nil || len(snaps) != 1 || snaps[0].ObjectKind != "Website" {
			t.Fatalf("expected 1 Website snapshot, got len=%d err=%v", len(snaps), err)
		}

		rbPlanRaw, err := f.invoke(f.token, "snapshot rollback", &command.Invocation{Flags: map[string]any{"id": int(snaps[0].ID)}})
		if err != nil {
			t.Fatalf("snapshot rollback failed: %v", err)
		}
		rbPlan := rbPlanRaw.(*PlanView)
		if _, err := f.invoke(f.token, "apply", &command.Invocation{Flags: map[string]any{"plan": int(rbPlan.PlanID)}}); err != nil {
			t.Fatalf("apply rollback failed: %v", err)
		}
		afterRB, err := f.websites.ByID(context.Background(), siteID)
		if err != nil {
			t.Fatal(err)
		}
		if afterRB.Spec.Target != "wss://127.0.0.1:10000/ws" || afterRB.Metadata.ResourceVersion != 3 {
			t.Fatalf("rollback mismatch: target=%q ver=%d", afterRB.Spec.Target, afterRB.Metadata.ResourceVersion)
		}

		// 3. 软删除 -> remove_domains 下发、同名重建报 name_taken、restore 恢复原 ID。
		delSpec, _ := json.Marshal(map[string]any{
			"op": "delete", "kind": "Website", "id": siteID, "resourceVersion": afterRB.Metadata.ResourceVersion,
		})
		delPlan := mustPlan(t, f, f.token, string(delSpec))
		if _, err := f.invoke(f.token, "apply", &command.Invocation{
			Flags: map[string]any{"plan": int(delPlan.PlanID)}, Confirm: delPlan.ConfirmHint,
		}); err != nil {
			t.Fatalf("apply delete failed: %v", err)
		}
		lastWCall := f.nodes.wcalls[len(f.nodes.wcalls)-1]
		if len(lastWCall.Params.RemoveDomains) != 1 || lastWCall.Params.RemoveDomains[0] != "app.example.com" {
			t.Fatalf("expected remove_domains=[app.example.com], got %+v", lastWCall.Params.RemoveDomains)
		}
		if _, err := f.invoke(f.token, "plan", &command.Invocation{Flags: map[string]any{"spec": string(createSpec)}}); v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("duplicate create during soft-delete should return name_taken, got %v", err)
		}
		deletedObj, err := f.websites.ByIDIncludeDeleted(context.Background(), siteID)
		if err != nil {
			t.Fatal(err)
		}
		restSpec, _ := json.Marshal(map[string]any{
			"op": "restore", "kind": "Website", "id": siteID, "resourceVersion": deletedObj.Metadata.ResourceVersion,
		})
		restPlan := mustPlan(t, f, f.token, string(restSpec))
		if _, err := f.invoke(f.token, "apply", &command.Invocation{
			Flags: map[string]any{"plan": int(restPlan.PlanID)}, Confirm: restPlan.ConfirmHint,
		}); err != nil {
			t.Fatalf("apply restore failed: %v", err)
		}
		restoredObj, err := f.websites.ByID(context.Background(), siteID)
		if err != nil || restoredObj.Metadata.ResourceVersion != 5 || !restoredObj.Status.Managed {
			t.Fatalf("restored object mismatch: obj=%+v err=%v", restoredObj, err)
		}
	})
}

func TestWebsiteSameHost443AndDynamicHumanGate(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setupWebsiteFixture(t, bdb)
		srv, err := f.servers.Create(context.Background(), "local-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.servers.SetSameHost(context.Background(), srv.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}

		// 1. 同机节点 plan 创建 use_443=true 的 Website -> 立即 conflict 拒绝，不建计划。
		use443Spec, _ := json.Marshal(map[string]any{
			"op":   "create",
			"kind": "Website",
			"spec": map[string]any{
				"server_id": srv.Metadata.ID,
				"domain":    "tls.example.com",
				"type":      "decoy",
				"target":    "landing",
				"use_443":   true,
			},
		})
		if _, err := f.invoke(f.admin, "plan", &command.Invocation{Flags: map[string]any{"spec": string(use443Spec)}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("same-host use_443 website must be rejected with conflict, got %v", err)
		}

		// 2. 同机节点 plan 创建 use_443=false 的 Website -> 成功且标 requires_human=true。
		okSpec, _ := json.Marshal(map[string]any{
			"op":   "create",
			"kind": "Website",
			"spec": map[string]any{
				"server_id": srv.Metadata.ID,
				"domain":    "decoy.example.com",
				"type":      "decoy",
				"target":    "blog",
				"use_443":   false,
			},
		})
		pv := mustPlan(t, f, f.admin, string(okSpec))
		if !pv.RequiresHuman {
			t.Fatalf("same-host website plan must set requires_human=true")
		}

		// 3. API 令牌 apply -> human_required，零落库零推节点。
		if _, err := f.invoke(f.token, "apply", &command.Invocation{Flags: map[string]any{"plan": int(pv.PlanID)}}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("token apply on same-host website must return human_required, got %v", err)
		}
		// 4. 管理员无密码 / 错密码 apply -> human_required。
		if _, err := f.invoke(f.admin, "apply", &command.Invocation{
			Flags:  map[string]any{"plan": int(pv.PlanID)},
			Verify: &command.Verification{Password: "wrong"},
		}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("wrong password must fail with human_required, got %v", err)
		}
		if len(f.nodes.wcalls) != 0 {
			t.Fatalf("failed verification must not call node, got %d", len(f.nodes.wcalls))
		}

		// 5. 管理员带正确当场密码 apply -> 成功，且下发 protect_master_443=true。
		if _, err := f.invoke(f.admin, "apply", &command.Invocation{
			Flags:  map[string]any{"plan": int(pv.PlanID)},
			Verify: &command.Verification{Password: "secret-pass"},
		}); err != nil {
			t.Fatalf("verified admin apply failed: %v", err)
		}
		if len(f.nodes.wcalls) != 1 || !f.nodes.wcalls[0].Params.ProtectMaster443 {
			t.Fatalf("expected protect_master_443=true on same-host apply, got %+v", f.nodes.wcalls)
		}

		// 6. plan 时远程（requires_human=false），apply 前切换为 same_host_as_master=true -> apply 按实时状态强制拒绝令牌。
		srv2, err := f.servers.Create(context.Background(), "flip-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		flipSpec, _ := json.Marshal(map[string]any{
			"op": "create", "kind": "Website",
			"spec": map[string]any{"server_id": srv2.Metadata.ID, "domain": "flip.example.com", "type": "static"},
		})
		flipPlan := mustPlan(t, f, f.token, string(flipSpec))
		if flipPlan.RequiresHuman {
			t.Fatalf("expected requires_human=false before flip")
		}
		if err := f.servers.SetSameHost(context.Background(), srv2.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.invoke(f.token, "apply", &command.Invocation{Flags: map[string]any{"plan": int(flipPlan.PlanID)}}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("live same_host flip must enforce human_required on apply, got %v", err)
		}

		// 7. 同机节点改 Server.site_type 触发 requires_human，但改 Server.region 不触发。
		srvCur, _ := f.servers.ByID(context.Background(), srv.Metadata.ID)
		regionSpec, _ := json.Marshal(map[string]any{
			"op": "update", "kind": "Server", "id": srv.Metadata.ID, "resourceVersion": srvCur.Metadata.ResourceVersion,
			"spec": map[string]any{"region": "Tokyo"},
		})
		regionPlan := mustPlan(t, f, f.token, string(regionSpec))
		if regionPlan.RequiresHuman {
			t.Fatalf("changing Server.region on same-host must not set requires_human")
		}
		nginxFieldSpec, _ := json.Marshal(map[string]any{
			"op": "update", "kind": "Server", "id": srv.Metadata.ID, "resourceVersion": srvCur.Metadata.ResourceVersion,
			"spec": map[string]any{"site_type": "proxy"},
		})
		nginxPlan := mustPlan(t, f, f.admin, string(nginxFieldSpec))
		if !nginxPlan.RequiresHuman {
			t.Fatalf("changing Server.site_type on same-host must set requires_human")
		}
		if _, err := f.invoke(f.token, "apply", &command.Invocation{Flags: map[string]any{"plan": int(nginxPlan.PlanID)}}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("token apply modifying same-host Server.site_type must return human_required, got %v", err)
		}
	})
}

func TestWebsiteNodeRollbackPartialFailureAndValidation(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setupWebsiteFixture(t, bdb)
		srv, err := f.servers.Create(context.Background(), "hk-rb", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		// 校验：批内重名、状态只读、注入字符
		dupSpec, _ := json.Marshal([]map[string]any{
			{"op": "create", "kind": "Website", "spec": map[string]any{"server_id": srv.Metadata.ID, "domain": "dup.example.com", "type": "static"}},
			{"op": "create", "kind": "Website", "spec": map[string]any{"server_id": srv.Metadata.ID, "domain": "DUP.example.com", "type": "static"}},
		})
		if _, err := f.invoke(f.admin, "plan", &command.Invocation{Flags: map[string]any{"spec": string(dupSpec)}}); v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("duplicate website in single plan must return name_taken, got %v", err)
		}
		statusSpec, _ := json.Marshal(map[string]any{
			"op": "create", "kind": "Website",
			"spec": map[string]any{"server_id": srv.Metadata.ID, "domain": "bad.example.com", "type": "static", "managed": true},
		})
		if _, err := f.invoke(f.admin, "plan", &command.Invocation{Flags: map[string]any{"spec": string(statusSpec)}}); v1.AsError(err).Code != v1.CodeFieldNotApplyable {
			t.Fatalf("status field in website spec must return field_not_applyable, got %v", err)
		}

		// 节点 nginx -t 失败自动回滚 -> apply 返回 partial_failure（退出码 8）。
		f.nodes.whandler = func(serverID int64, p xrpc.NginxWebsitesApplyParams) (*xrpc.NginxWebsitesApplyResult, error) {
			return &xrpc.NginxWebsitesApplyResult{
				Reloaded:   false,
				RolledBack: true,
				Error:      "nginx -t failed: syntax error",
			}, nil
		}
		okSpec, _ := json.Marshal(map[string]any{
			"op": "create", "kind": "Website",
			"spec": map[string]any{"server_id": srv.Metadata.ID, "domain": "broken.example.com", "type": "static"},
		})
		pv := mustPlan(t, f, f.token, string(okSpec))
		_, err = f.invoke(f.token, "apply", &command.Invocation{Flags: map[string]any{"plan": int(pv.PlanID)}})
		ae := v1.AsError(err)
		if ae == nil || ae.Code != v1.CodePartialFailure {
			t.Fatalf("expected partial_failure when node rolls back, got %v", err)
		}
	})
}

func mustPlan(t *testing.T, f *websiteFixture, ctx context.Context, spec string) *PlanView {
	t.Helper()
	raw, err := f.invoke(ctx, "plan", &command.Invocation{Flags: map[string]any{"spec": spec}})
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	return raw.(*PlanView)
}
