package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corebatch "github.com/satchel/satchel/internal/core/batch"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	coreplans "github.com/satchel/satchel/internal/core/plans"
	corereturnroutes "github.com/satchel/satchel/internal/core/returnroutes"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	coresnapshots "github.com/satchel/satchel/internal/core/snapshots"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestReturnRoutePlanApplyAndSnapshotRollback(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		srvRepo := coreservers.New(bdb, st)
		inRepo := coreinbounds.New(bdb, st)
		outRepo := coreoutbounds.New(bdb, st)
		rrRepo := corerouting.New(bdb, st)
		webRepo := corewebsites.New(bdb, st)
		retRepo := corereturnroutes.New(bdb, st)
		planRepo := coreplans.New(bdb, st)
		snapRepo := coresnapshots.New(bdb, st)
		batchRepo := corebatch.New(bdb, st)
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}

		nodes := newOrderedFakeNodes()
		svc := New(Deps{
			DB:           bdb,
			Servers:      srvRepo,
			Inbounds:     inRepo,
			Outbounds:    outRepo,
			RoutingRules: rrRepo,
			Websites:     webRepo,
			ReturnRoutes: retRepo,
			Plans:        planRepo,
			Snapshots:    snapRepo,
			Batch:        batchRepo,
			Settings:     setRepo,
			Nodes:        nodes,
			WebsiteNodes: nodes,
		})
		b := svc.Bindings()
		admin := v1.WithIdentity(ctx, v1.LocalAdmin("root"))

		// 离线（pending）服务器：纯 ReturnRoute 变更不应触发节点 RPC，也不因离线返回 partial_failure。
		srv, err := srvRepo.Create(ctx, "hk-off", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}

		// 1. 拒收 status 字段与非法 probe；拒收单次 plan 内重复创建同 carrier。
		if _, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"ct","route_type":"CN2 GIA"}}`, srv.Metadata.ID),
		}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeFieldNotApplyable {
			t.Fatalf("夹带 status 列应报 field_not_applyable，得到：%v", err)
		}
		if _, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"ct","probe":{"port":99999}}}`, srv.Metadata.ID),
		}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("非法 probe 应报 bad_request，得到：%v", err)
		}
		if _, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`[{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"ct"}},{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"CT"}}]`, srv.Metadata.ID, srv.Metadata.ID),
		}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("批内重复创建同 carrier 应报 name_taken，得到：%v", err)
		}

		// 2. 创建 ReturnRoute（小写 ct 自动归一为 CT）并 apply，不触发任何节点推送。
		pAny, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"ct","enabled":true,"probe":{"target_ip":"202.97.1.1","max_hops":25}}}`, srv.Metadata.ID),
		}})
		if err != nil {
			t.Fatalf("plan create ReturnRoute 失败：%v", err)
		}
		pv := pAny.(*PlanView)
		if len(pv.Servers) != 0 {
			t.Fatalf("纯 ReturnRoute plan 不应渲染节点 diff，得到 %d 台", len(pv.Servers))
		}
		aAny, err := b["apply"](admin, &command.Invocation{Flags: map[string]any{"plan": int(pv.PlanID)}})
		if err != nil {
			t.Fatalf("apply create ReturnRoute 失败：%v", err)
		}
		av := aAny.(*ApplyResult)
		if len(av.Objects) != 1 || len(av.Servers) != 0 || len(nodes.order) != 0 {
			t.Fatalf("离线节点纯 ReturnRoute apply 不应推送节点：objects=%d servers=%d rpc=%v", len(av.Objects), len(av.Servers), nodes.order)
		}
		rrID := av.Objects[0].ID

		// 同机新建重复 CT 报 name_taken。
		if _, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"CT"}}`, srv.Metadata.ID),
		}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("同机重复 carrier 应报 name_taken，得到：%v", err)
		}

		// 3. 更新 enabled 与 probe（留下写前快照）并 apply。
		pUpd, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"update","kind":"ReturnRoute","id":%d,"resourceVersion":1,"spec":{"enabled":false,"probe":{"target_ip":"202.97.2.2","max_hops":18}}}`, rrID),
		}})
		if err != nil {
			t.Fatalf("plan update ReturnRoute 失败：%v", err)
		}
		aUpdAny, err := b["apply"](admin, &command.Invocation{Flags: map[string]any{"plan": int(pUpd.(*PlanView).PlanID)}})
		if err != nil {
			t.Fatalf("apply update ReturnRoute 失败：%v", err)
		}
		updApplyID := aUpdAny.(*ApplyResult).ApplyID
		got, err := retRepo.ByID(ctx, rrID)
		if err != nil || got.Metadata.ResourceVersion != 2 || got.Spec.Enabled {
			t.Fatalf("update 后状态异常：%+v err=%v", got, err)
		}

		// 4. snapshot rollback 恢复原 enabled=true 与 probe（202.97.1.1），版本递增为 3 且零节点 RPC。
		rbAny, err := b["snapshot rollback"](admin, &command.Invocation{Flags: map[string]any{"apply-id": updApplyID}})
		if err != nil {
			t.Fatalf("snapshot rollback 失败：%v", err)
		}
		rbPlanID := rbAny.(*PlanView).PlanID
		if _, err := b["apply"](admin, &command.Invocation{Flags: map[string]any{"plan": int(rbPlanID)}}); err != nil {
			t.Fatalf("apply rollback 失败：%v", err)
		}
		got, err = retRepo.ByID(ctx, rrID)
		if err != nil {
			t.Fatal(err)
		}
		var probeObj map[string]any
		_ = json.Unmarshal(got.Spec.Probe, &probeObj)
		if got.Metadata.ResourceVersion != 3 || !got.Spec.Enabled || probeObj["target_ip"] != "202.97.1.1" {
			t.Fatalf("回滚后状态不对：ver=%d enabled=%v probe=%s", got.Metadata.ResourceVersion, got.Spec.Enabled, string(got.Spec.Probe))
		}

		// 5. 软删除（带 delete 确认）保留占坑，再恢复复用原 ID。
		pDel, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"delete","kind":"ReturnRoute","id":%d,"resourceVersion":3}`, rrID),
		}})
		if err != nil {
			t.Fatalf("plan delete ReturnRoute 失败：%v", err)
		}
		confirmName := fmt.Sprintf("%d/CT", srv.Metadata.ID)
		if _, err := b["apply"](admin, &command.Invocation{Confirm: confirmName, Flags: map[string]any{"plan": int(pDel.(*PlanView).PlanID)}}); err != nil {
			t.Fatalf("apply delete ReturnRoute 失败：%v", err)
		}
		if _, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"create","kind":"ReturnRoute","spec":{"server_id":%d,"carrier":"CT"}}`, srv.Metadata.ID),
		}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("软删除占坑期新建同 carrier 应报 name_taken，得到：%v", err)
		}
		pRes, err := b["plan"](admin, &command.Invocation{Flags: map[string]any{
			"spec": fmt.Sprintf(`{"op":"restore","kind":"ReturnRoute","id":%d,"resourceVersion":4}`, rrID),
		}})
		if err != nil {
			t.Fatalf("plan restore ReturnRoute 失败：%v", err)
		}
		if _, err := b["apply"](admin, &command.Invocation{Confirm: confirmName, Flags: map[string]any{"plan": int(pRes.(*PlanView).PlanID)}}); err != nil {
			t.Fatalf("apply restore ReturnRoute 失败：%v", err)
		}
		got, err = retRepo.ByID(ctx, rrID)
		if err != nil || got.Metadata.ResourceVersion != 5 || got.Metadata.DeletedAt != nil || len(nodes.order) != 0 {
			t.Fatalf("restore 后状态或 RPC 异常：got=%+v rpc=%v err=%v", got, nodes.order, err)
		}
	})
}
