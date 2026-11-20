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
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type fakeNodes struct {
	mu      sync.Mutex
	locks   map[int64]*sync.Mutex
	calls   []fakeApplyCall
	handler func(serverID int64, p xrpc.ConfigApplyParams) (*xrpc.ConfigApplyResult, error)
}

type fakeApplyCall struct {
	ServerID int64
	Params   xrpc.ConfigApplyParams
}

func newFakeNodes() *fakeNodes {
	return &fakeNodes{locks: map[int64]*sync.Mutex{}}
}

func (f *fakeNodes) LockServerConfig(serverID int64) func() {
	f.mu.Lock()
	mu := f.locks[serverID]
	if mu == nil {
		mu = &sync.Mutex{}
		f.locks[serverID] = mu
	}
	f.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (f *fakeNodes) CallConfigApply(_ context.Context, serverID int64, params xrpc.ConfigApplyParams) (*xrpc.ConfigApplyResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeApplyCall{ServerID: serverID, Params: params})
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		return h(serverID, params)
	}
	return &xrpc.ConfigApplyResult{
		Applied:       true,
		AppliedHash:   xrpc.CanonicalConfigHash([]byte(params.Config)),
		Generation:    1,
		KernelRunning: true,
		Inbounds:      []string{},
	}, nil
}

type fixture struct {
	svc       *Service
	servers   *coreservers.Repo
	inbounds  *coreinbounds.Repo
	outbounds *coreoutbounds.Repo
	routing   *corerouting.Repo
	plans     *coreplans.Repo
	snapshots *coresnapshots.Repo
	batch     *corebatch.Repo
	settings  *coresettings.Repo
	nodes     *fakeNodes
	now       time.Time
	admin     context.Context
	user      context.Context
}

func setupFixture(t *testing.T, bdb *bun.DB) *fixture {
	t.Helper()
	st := store.New(bdb, schema.Default())
	setRepo := coresettings.New(bdb, st, schema.Default())
	if err := setRepo.EnsureSingleton(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		servers:   coreservers.New(bdb, st),
		inbounds:  coreinbounds.New(bdb, st),
		outbounds: coreoutbounds.New(bdb, st),
		routing:   corerouting.New(bdb, st),
		plans:     coreplans.New(bdb, st),
		snapshots: coresnapshots.New(bdb, st),
		batch:     corebatch.New(bdb, st),
		settings:  setRepo,
		nodes:     newFakeNodes(),
		now:       time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		admin:     v1.WithIdentity(context.Background(), v1.LocalAdmin("root")),
		user: v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate},
		}),
	}
	f.svc = New(Deps{
		DB:           bdb,
		Servers:      f.servers,
		Inbounds:     f.inbounds,
		Outbounds:    f.outbounds,
		RoutingRules: f.routing,
		Plans:        f.plans,
		Snapshots:    f.snapshots,
		Batch:        f.batch,
		Settings:     f.settings,
		Nodes:        f.nodes,
		Now:          func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) run(ctx context.Context, name string, args []string, flags map[string]any, confirm string) (any, int64, error) {
	ctx, slot := command.WithPlanRecorder(ctx)
	if flags == nil {
		flags = map[string]any{}
	}
	h := f.svc.Bindings()[name]
	res, err := h(ctx, &command.Invocation{Path: strings.Split(name, " "), Args: args, Flags: flags, Confirm: confirm})
	return res, slot.Load(), err
}

func TestPlanApplyHappyPathAndStrictValidation(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setupFixture(t, bdb)
		srv, err := f.servers.Create(context.Background(), "hk-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		// 1. 夹带 status 字段必须报 field_not_applyable，不建计划。
		if _, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":1,"spec":{"status":"connected"}}`,
		}, ""); v1.AsError(err).Code != v1.CodeFieldNotApplyable {
			t.Fatalf("spec 中带 status 应报 field_not_applyable，得到 %v", err)
		}
		if _, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":1,"spec":{"core_log_level":"info"},"status":{"core_running":true}}`,
		}, ""); v1.AsError(err).Code != v1.CodeFieldNotApplyable {
			t.Fatalf("顶层带 status 应报 field_not_applyable，得到 %v", err)
		}
		if _, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":1,"spec":{"no_such":1}}`,
		}, ""); v1.AsError(err).Code != v1.CodeUnknownField {
			t.Fatalf("未知字段应报 unknown_field，得到 %v", err)
		}
		// 非管理员全部被拒。
		for _, cmd := range []string{"plan", "apply", "plans list", "plans get", "snapshot list", "snapshot get", "snapshot rollback"} {
			if _, _, err := f.run(f.user, cmd, []string{"1"}, map[string]any{"spec": "{}", "plan": 1, "id": 1}, ""); v1.AsError(err).Code != v1.CodeForbidden {
				t.Errorf("普通用户执行 %s 应报 forbidden，得到 %v", cmd, err)
			}
		}

		// 2. 正常预演：修改 Server.core_log_level + 新增 Inbound + Outbound + RoutingRule。
		specJSON, _ := json.Marshal([]map[string]any{
			{"op": "update", "kind": "Server", "id": srv.Metadata.ID, "resourceVersion": 1, "spec": map[string]any{"core_log_level": "debug"}},
			{"op": "create", "kind": "Inbound", "spec": map[string]any{
				"server_id": srv.Metadata.ID, "tag": "vless-1", "protocol": "vless", "port": 8443, "listen": "127.0.0.1", "enabled": true,
				"settings": map[string]any{"users": []map[string]any{{"name": "u1", "uuid": "secret-uuid-123"}}},
			}},
			{"op": "create", "kind": "Outbound", "spec": map[string]any{
				"server_id": srv.Metadata.ID, "tag": "out-1", "protocol": "direct",
			}},
			{"op": "create", "kind": "RoutingRule", "spec": map[string]any{
				"server_id": srv.Metadata.ID, "sort_order": 1, "outbound_tag": "out-1", "enabled": true, "catch_all": true,
			}},
		})
		res, planSlot, err := f.run(f.admin, "plan", nil, map[string]any{"spec": string(specJSON)}, "")
		if err != nil {
			t.Fatalf("plan failed: %v", err)
		}
		pv := res.(*PlanView)
		if pv.PlanID <= 0 || planSlot != pv.PlanID || pv.AffectedCount != 4 || len(pv.Servers) != 1 {
			t.Fatalf("plan view unexpected: %+v slot=%d", pv, planSlot)
		}

		// 非 secrets 令牌调用 plans get 必须看不到 uuid 明文。
		noSecretsCtx := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate},
		})
		gotPlan, _, err := f.run(noSecretsCtx, "plans get", []string{"1"}, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		rawPlan, _ := json.Marshal(gotPlan)
		if strings.Contains(string(rawPlan), "secret-uuid-123") {
			t.Fatalf("无 secrets 权限查看 plans get 泄露了 uuid：%s", rawPlan)
		}

		// 3. 提交 apply（首次下发 ForceInitial=true）。
		if err := f.servers.SetFirstApplyEligible(context.Background(), srv.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}
		appRes, appSlot, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(pv.PlanID)}, "")
		if err != nil {
			t.Fatalf("apply failed: %v", err)
		}
		ar := appRes.(*ApplyResult)
		if appSlot != pv.PlanID || ar.ApplyID == "" || len(ar.Servers) != 1 || ar.Servers[0].Status != "applied" {
			t.Fatalf("apply result unexpected: %+v", ar)
		}
		if len(f.nodes.calls) != 1 || !f.nodes.calls[0].Params.ForceInitial || f.nodes.calls[0].Params.ExpectedCurrentHash != "" {
			t.Fatalf("首次下发应携带 ForceInitial=true：%+v", f.nodes.calls)
		}
		// 核对库内 applied_hash 与 first_apply_eligible 已消费。
		afterSrv, err := f.servers.ByID(context.Background(), srv.Metadata.ID)
		if err != nil {
			t.Fatal(err)
		}
		if afterSrv.Metadata.ResourceVersion != 2 || afterSrv.Status.AppliedHash != ar.Servers[0].AppliedHash || afterSrv.Status.FirstApplyEligible {
			t.Fatalf("服务器状态更新不对：ver=%d status=%+v", afterSrv.Metadata.ResourceVersion, afterSrv.Status)
		}
		// 写前快照应含 Server（且不含三把令牌）。
		snaps, err := f.snapshots.ListByApplyID(context.Background(), ar.ApplyID)
		if err != nil || len(snaps) != 1 || snaps[0].ObjectKind != "Server" {
			t.Fatalf("应有 1 条 Server 写前快照：%+v %v", snaps, err)
		}
		if strings.Contains(string(snaps[0].Content), afterSrv.Status.Token.Reveal()) {
			t.Fatalf("Server 快照绝不能含通信令牌")
		}
		// 重复 apply 同一个计划报 conflict。
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(pv.PlanID)}, ""); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("重复 apply 应报 conflict，得到 %v", err)
		}

		// 4. 二次变更携带 ExpectedCurrentHash（不再 ForceInitial）。
		pv2Res, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":2,"spec":{"core_log_level":"error"}}`,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		pv2 := pv2Res.(*PlanView)
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(pv2.PlanID)}, ""); err != nil {
			t.Fatal(err)
		}
		if len(f.nodes.calls) != 2 || f.nodes.calls[1].Params.ForceInitial || f.nodes.calls[1].Params.ExpectedCurrentHash != afterSrv.Status.AppliedHash {
			t.Fatalf("二次 apply 应携带 ExpectedCurrentHash=%q：%+v", afterSrv.Status.AppliedHash, f.nodes.calls[1].Params)
		}
	})
}

func TestVersionConflictForceBoundaryAndDrift(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setupFixture(t, bdb)
		srv, err := f.servers.Create(context.Background(), "hk-force", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		// 生成计划：把 core_log_level 改为 debug（基于 v1）。
		p1Res, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":1,"spec":{"core_log_level":"debug"}}`,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		p1 := p1Res.(*PlanView)

		// 另一写者修改了无关字段（traffic_stats_mode），使版本升至 v2，但 core_log_level 仍为 warn。
		modeUpload := coreservers.TrafficStatsUpload
		if _, err := f.servers.UpdateTrafficStats(context.Background(), srv.Metadata.ID, 1, coreservers.TrafficStatsPatch{Mode: &modeUpload}); err != nil {
			t.Fatal(err)
		}
		// 1) 不带 --force -> version_conflict。
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(p1.PlanID)}, ""); v1.AsError(err).Code != v1.CodeVersionConflict {
			t.Fatalf("不带 --force 应报 version_conflict，得到 %v", err)
		}
		// 2) 带 --force 但令牌缺 permission 危险类 -> forbidden。
		noPerm := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate},
		})
		if _, _, err := f.run(noPerm, "apply", nil, map[string]any{"plan": int(p1.PlanID), "force": true}, "hk-force"); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("缺 permission 带 --force 应报 forbidden，得到 %v", err)
		}
		// 3) 管理员带 --force 缺 confirm -> confirm_required。
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(p1.PlanID), "force": true}, ""); v1.AsError(err).Code != v1.CodeConfirmRequired {
			t.Fatalf("缺 confirm 应报 confirm_required，得到 %v", err)
		}
		// 4) 管理员带 --force + --confirm hk-force 成功落地，且保留了 traffic_stats_mode=upload。
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(p1.PlanID), "force": true}, "hk-force"); err != nil {
			t.Fatalf("合法 --force 应当成功：%v", err)
		}
		got, _ := f.servers.ByID(context.Background(), srv.Metadata.ID)
		if got.Spec.CoreLogLevel != "debug" || got.Spec.TrafficStatsMode != coreservers.TrafficStatsUpload || got.Metadata.ResourceVersion != 3 {
			t.Fatalf("--force 落地结果不对：%+v", got)
		}

		// 5) diff 发生漂移时（另一写者改了同一字段 core_log_level），即便带 --force 也必须报 conflict。
		p2Res, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":3,"spec":{"core_log_level":"info"}}`,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		p2 := p2Res.(*PlanView)
		// 另一计划把 core_log_level 改成 error。
		pOtherRes, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":3,"spec":{"core_log_level":"error"}}`,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(pOtherRes.(*PlanView).PlanID)}, ""); err != nil {
			t.Fatal(err)
		}
		// 此时再带 --force 提交 p2，因为 before 从 debug 漂移成 error，必须报 conflict。
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(p2.PlanID), "force": true}, "hk-force"); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("diff 漂移即便带 --force 也必须报 conflict，得到 %v", err)
		}
	})
}

func TestPartialFailureBatchWindowDeleteRestoreAndRollback(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setupFixture(t, bdb)
		s1, err := f.servers.Create(context.Background(), "hk-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := f.servers.Create(context.Background(), "hk-2", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		// 模拟 s2 离线，s1 在线。
		f.nodes.handler = func(serverID int64, p xrpc.ConfigApplyParams) (*xrpc.ConfigApplyResult, error) {
			if serverID == s2.Metadata.ID {
				return nil, v1.New(v1.CodeUnavailable, "服务器 hk-2 离线")
			}
			return &xrpc.ConfigApplyResult{Applied: true, AppliedHash: xrpc.CanonicalConfigHash([]byte(p.Config)), Generation: 2, KernelRunning: true}, nil
		}
		// 1. 多节点部分失败：hk-1 落地成功、hk-2 失败，返回 partial_failure（退出码 8）。
		bothSpec, _ := json.Marshal([]map[string]any{
			{"op": "update", "kind": "Server", "id": s1.Metadata.ID, "resourceVersion": 1, "spec": map[string]any{"core_log_level": "info"}},
			{"op": "update", "kind": "Server", "id": s2.Metadata.ID, "resourceVersion": 1, "spec": map[string]any{"core_log_level": "info"}},
		})
		pvRes, _, err := f.run(f.admin, "plan", nil, map[string]any{"spec": string(bothSpec)}, "")
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = f.run(f.admin, "apply", nil, map[string]any{"plan": int(pvRes.(*PlanView).PlanID)}, "")
		ae := v1.AsError(err)
		if ae.Code != v1.CodePartialFailure || v1.ExitCodeOf(ae) != 8 {
			t.Fatalf("应报 partial_failure (exit 8)，得到 %+v", ae)
		}
		applyID, _ := ae.State["apply_id"].(string)
		afterS1, _ := f.servers.ByID(context.Background(), s1.Metadata.ID)
		afterS2, _ := f.servers.ByID(context.Background(), s2.Metadata.ID)
		if afterS1.Status.AppliedHash == "" || afterS2.Status.AppliedHash != "" {
			t.Fatalf("s1 应已同步、s2 应未更新 applied_hash：s1=%q s2=%q", afterS1.Status.AppliedHash, afterS2.Status.AppliedHash)
		}

		// 2. 软删除 s2（需要 DangerDelete + --confirm hk-2），随后 snapshot rollback --apply-id 跳过已软删除的 s2。
		delPlanRes, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"delete","kind":"Server","id":2,"resourceVersion":2}`,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		dp := delPlanRes.(*PlanView)
		if len(dp.RequiredDangers) != 1 || dp.RequiredDangers[0] != v1.DangerDelete || dp.ConfirmHint != "hk-2" {
			t.Fatalf("软删除危险推导不对：%+v", dp)
		}
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(dp.PlanID)}, "hk-2"); err != nil {
			t.Fatalf("软删除应成功：%v", err)
		}
		// 软删除期间新建同名 hk-2 返回 name_taken。
		if _, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"create","kind":"Server","spec":{"name":"hk-2"}}`,
		}, ""); v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("软删除期内重名创建应报 name_taken，得到 %v", err)
		}

		// 按 apply_id 回滚：应跳过 s2，仅生成 s1 的恢复计划（且写入 plan_id）。
		rbRes, rbSlot, err := f.run(f.admin, "snapshot rollback", nil, map[string]any{"apply-id": applyID}, "")
		if err != nil {
			t.Fatalf("snapshot rollback 失败：%v", err)
		}
		rbPlan := rbRes.(*PlanView)
		if rbSlot != rbPlan.PlanID || len(rbPlan.Objects) != 1 || rbPlan.Objects[0].ID != s1.Metadata.ID || len(rbPlan.Skipped) != 1 || rbPlan.Skipped[0].ID != s2.Metadata.ID {
			t.Fatalf("snapshot rollback 结果不对：%+v", rbPlan)
		}
		// 执行回滚计划：s1 恢复为 warn 且 resourceVersion 递增至 3（绝不倒退）。
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(rbPlan.PlanID)}, ""); err != nil {
			t.Fatalf("应用回滚计划失败：%v", err)
		}
		s1Rolled, _ := f.servers.ByID(context.Background(), s1.Metadata.ID)
		if s1Rolled.Spec.CoreLogLevel != "warn" || s1Rolled.Metadata.ResourceVersion != 3 {
			t.Fatalf("回滚后 s1 状态不对：level=%s ver=%d", s1Rolled.Spec.CoreLogLevel, s1Rolled.Metadata.ResourceVersion)
		}

		// 3. 恢复已软删除的 s2（restore）。
		f.nodes.handler = nil // s2 恢复在线
		restPlanRes, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"restore","kind":"Server","id":2,"resourceVersion":3}`,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(restPlanRes.(*PlanView).PlanID)}, "hk-2"); err != nil {
			t.Fatalf("restore 应成功：%v", err)
		}
		s2Restored, err := f.servers.ByID(context.Background(), s2.Metadata.ID)
		if err != nil || s2Restored.Metadata.ResourceVersion != 4 || s2Restored.Metadata.DeletedAt != nil {
			t.Fatalf("restore 后 s2 状态不对：%+v %v", s2Restored, err)
		}

		// 4. 批量类滚动窗口：把 batch_window_threshold 设为 3，用无 batch 权限的令牌连续提交 4 次单对象 apply。
		if _, err := f.settings.Write(context.Background(), coresettings.WriteRequest{
			Values: map[string]any{"batch_window_threshold": int64(3), "batch_single_threshold": int64(5), "batch_window_minutes": int64(15)}, ExpectedVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		tokID := int64(505)
		tokNoBatch := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokID, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate},
		})
		tokWithBatch := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokID, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{v1.DangerBatch},
		})
		levels := []string{"debug", "info", "warn", "error"}
		curVer := int64(3)
		for i := 0; i < 3; i++ {
			sp, _ := json.Marshal(map[string]any{"op": "update", "kind": "Server", "id": s1.Metadata.ID, "resourceVersion": curVer, "spec": map[string]any{"core_log_level": levels[i]}})
			pRes, _, err := f.run(tokNoBatch, "plan", nil, map[string]any{"spec": string(sp)}, "")
			if err != nil {
				t.Fatalf("batch step %d plan: %v", i, err)
			}
			if _, _, err := f.run(tokNoBatch, "apply", nil, map[string]any{"plan": int(pRes.(*PlanView).PlanID)}, ""); err != nil {
				t.Fatalf("batch step %d apply: %v", i, err)
			}
			curVer++
		}
		// 第 4 次累计达到 4 > 3，无 batch 权限应报 forbidden，开 batch 后无 confirm 报 confirm_required，带 --confirm 1 放行。
		sp4, _ := json.Marshal(map[string]any{"op": "update", "kind": "Server", "id": s1.Metadata.ID, "resourceVersion": curVer, "spec": map[string]any{"core_log_level": levels[3]}})
		p4Res, _, err := f.run(tokNoBatch, "plan", nil, map[string]any{"spec": string(sp4)}, "")
		if err != nil {
			t.Fatal(err)
		}
		p4 := p4Res.(*PlanView)
		if len(p4.RequiredDangers) != 1 || p4.RequiredDangers[0] != v1.DangerBatch || p4.ConfirmHint != "1" {
			t.Fatalf("第 4 次 plan 应提示 batch 危险与 confirm=1：%+v", p4)
		}
		if _, _, err := f.run(tokNoBatch, "apply", nil, map[string]any{"plan": int(p4.PlanID)}, "1"); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("第 4 次无 batch 应报 forbidden，得到 %v", err)
		}
		if _, _, err := f.run(tokWithBatch, "apply", nil, map[string]any{"plan": int(p4.PlanID)}, ""); v1.AsError(err).Code != v1.CodeConfirmRequired {
			t.Fatalf("第 4 次缺 confirm 应报 confirm_required，得到 %v", err)
		}
		if _, _, err := f.run(tokWithBatch, "apply", nil, map[string]any{"plan": int(p4.PlanID)}, "1"); err != nil {
			t.Fatalf("开启 batch 并带 --confirm 1 应当放行：%v", err)
		}

		// 5. 计划过期检查。
		pExpRes, _, err := f.run(f.admin, "plan", nil, map[string]any{
			"spec": `{"op":"update","kind":"Server","id":1,"resourceVersion":7,"spec":{"core_log_level":"warn"}}`,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(11 * time.Minute)
		if _, _, err := f.run(f.admin, "apply", nil, map[string]any{"plan": int(pExpRes.(*PlanView).PlanID)}, ""); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("过期计划 apply 应报 conflict，得到 %v", err)
		}
	})
}
