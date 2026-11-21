package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	corebatchin "github.com/satchel/satchel/internal/core/batchinbounds"
	corebatchout "github.com/satchel/satchel/internal/core/batchoutbounds"
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

var dangerChinese = map[v1.Danger]string{
	v1.DangerDelete:     "删除类",
	v1.DangerPermission: "权限类",
	v1.DangerBatch:      "批量类",
}

func (s *Service) applyCmd(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	planID := int64(inv.Int("plan", 0))
	if planID <= 0 {
		return nil, v1.New(v1.CodeBadRequest, "缺少必填参数 --plan <计划编号>").WithNext("先执行 satchel plan 生成计划编号")
	}
	command.RecordPlanID(ctx, planID)
	force := inv.Bool("force")

	p, err := s.deps.Plans.Get(ctx, planID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if p.Status.Status != coreplans.StatusPending {
		return nil, v1.Newf(v1.CodeConflict, "计划 %d 当前状态为 %s，不能重复提交", planID, p.Status.Status).
			WithNext("如需再次修改，请重新运行 satchel plan")
	}
	if !now.Before(p.Spec.ExpiresAt) {
		_ = s.deps.Plans.MarkExpired(ctx, planID)
		return nil, v1.Newf(v1.CodeConflict, "计划 %d 已于 %s 过期，不能提交", planID, p.Spec.ExpiresAt.Format("2006-01-02T15:04:05Z")).
			WithNext("请重新运行 satchel plan 生成新计划")
	}

	var plannedObjs []PlannedObject
	if err := json.Unmarshal(p.Spec.Objects, &plannedObjs); err != nil || len(plannedObjs) == 0 {
		return nil, v1.New(v1.CodeInternal, "计划内容损坏，无法解析变更对象列表")
	}
	var savedDiff PlanDiffData
	_ = json.Unmarshal(p.Spec.Diff, &savedDiff)
	batchID := ""
	if savedDiff.BatchMeta != nil {
		batchID = savedDiff.BatchMeta.BatchID
	}
	ops := make([]Operation, len(plannedObjs))
	for i, po := range plannedObjs {
		ops[i] = Operation{
			Op:              po.Op,
			Kind:            po.Kind,
			ID:              po.ID,
			ResourceVersion: po.ExpectedVersion,
			Spec:            po.Spec,
		}
	}

	// 先按计划对象与 --force 及实时滚动窗口检查动态危险类与 --confirm（先验权再动库，拒绝越权探测）。
	prelimDangers := map[v1.Danger]bool{}
	for _, po := range plannedObjs {
		for _, d := range po.Dangers {
			prelimDangers[d] = true
		}
	}
	if force {
		prelimDangers[v1.DangerPermission] = true
	}
	id := v1.IdentityFrom(ctx)
	if err := s.enforceDynamicDangers(ctx, id, prelimDangers, plannedObjs, inv.Confirm, now); err != nil {
		return nil, err
	}

	// 按实时库重算每个操作与 diff，核对对象集合、现状漂移与版本号。
	ev, err := s.evaluateOps(ctx, ops, false)
	if err != nil {
		// 若对象被删或被恢复或自然键被占，映射为 conflict 提示重跑 plan。
		if code := v1.AsError(err).Code; code == v1.CodeNotFound || code == v1.CodeBadRequest || code == v1.CodeNameTaken {
			return nil, v1.Newf(v1.CodeConflict, "计划 %d 对应的库内现状已变化，请重新 plan：%s", planID, v1.AsError(err).Reason).
				WithNext("请重新运行 satchel plan")
		}
		return nil, err
	}

	// 核对逐对象 diff 是否漂移（无论是否带 --force，diff 漂移一律拒收并要求重跑 plan）。
	for i, po := range plannedObjs {
		live := ev.Items[i]
		if live.Planned.FieldDiffHash != po.FieldDiffHash {
			return nil, v1.Newf(v1.CodeConflict, "计划 %d 中对象 %s（%s）的实时变更内容已发生漂移，不能直接提交", planID, po.Kind, po.Name).
				WithState("object", po.Name).
				WithNext("请重新读取最新对象并重新运行 satchel plan")
		}
	}

	// 核对版本号：不带 --force 时版本不匹配报 version_conflict（退出码 6）。
	if !force {
		for i, po := range plannedObjs {
			if po.Op == "create" {
				continue
			}
			live := ev.Items[i]
			if live.CurrentVersion != po.ExpectedVersion {
				return nil, versionConflictErr(po.Kind, po.ExpectedVersion, live.CurrentVersion)
			}
		}
	}

	// 按 server_id 升序获取全部受影响服务器的配置锁，防止并发 apply / restart-kernel 竞争与多机死锁。
	if s.deps.Nodes != nil {
		unlocks := make([]func(), 0, len(ev.AffectedServers))
		for _, srvID := range ev.AffectedServers {
			unlocks = append(unlocks, s.deps.Nodes.LockServerConfig(srvID))
		}
		defer func() {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
		}()
	}

	// 写前快照 + 业务对象提交 + 批量滚动窗口记账 + 消费 plan 全部在单一数据库事务内原子完成。
	applyID := newApplyID()
	appliedSummaries := make([]AppliedObjectSummary, 0, len(ev.Items))
	err = s.deps.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// 事务内重查滚动窗口，防范并发请求绕过窗口阈值。
		if err := s.enforceBatchWindowInTx(ctx, tx, id, plannedObjs, prelimDangers, inv.Confirm, now); err != nil {
			return err
		}
		// 1. 原子流转计划状态（pending -> applied），并发二次提交会立刻在此报 conflict。
		if err := s.deps.Plans.MarkAppliedTx(ctx, tx, planID, applyID); err != nil {
			return err
		}
		// 2. 为全部已存在目标追加写前快照（共享 apply_id；Server 快照仅含 ServerSpec，绝不含三把令牌），随后落库变更。
		for _, it := range ev.Items {
			if it.Op.Op != "create" && len(it.CurrentBefore) > 0 {
				if _, err := s.deps.Snapshots.AppendTx(ctx, tx, coresnapshots.AppendInput{
					ObjectKind:    it.Op.Kind,
					ObjectID:      it.Op.ID,
					ObjectVersion: it.CurrentVersion,
					Content:       it.CurrentBefore,
					Source:        coresnapshots.SourceApply,
					Status:        coresnapshots.StatusSaved,
					ApplyID:       applyID,
				}); err != nil {
					return err
				}
			}
			sum, err := s.applyOneTx(ctx, tx, it, force)
			if err != nil {
				return err
			}
			appliedSummaries = append(appliedSummaries, sum)
		}
		// 3. 持久化记录本次批量操作计数（重启不清零）及多机批次追踪记录。
		if savedDiff.BatchMeta != nil && savedDiff.BatchMeta.BatchID != "" {
			switch savedDiff.BatchMeta.Kind {
			case coreinbounds.KindName:
				if s.deps.BatchInbounds != nil {
					entries := make([]corebatchin.Entry, 0, len(ev.Items))
					for _, it := range ev.Items {
						if it.Op.Op == "create" && it.Op.Kind == coreinbounds.KindName && it.InboundSpec != nil {
							entries = append(entries, corebatchin.Entry{
								BatchID:   savedDiff.BatchMeta.BatchID,
								Tag:       it.InboundSpec.Tag,
								ServerID:  it.InboundSpec.ServerID,
								Protocol:  it.InboundSpec.Protocol,
								Port:      it.InboundSpec.Port,
								CreatedAt: now,
							})
						}
					}
					if len(entries) > 0 {
						if err := s.deps.BatchInbounds.InsertTx(ctx, tx, entries); err != nil {
							return err
						}
					}
				}
			case coreoutbounds.KindName:
				if s.deps.BatchOutbounds != nil {
					entries := make([]corebatchout.Entry, 0, len(ev.Items))
					for _, it := range ev.Items {
						if it.Op.Op == "create" && it.Op.Kind == coreoutbounds.KindName && it.OutboundSpec != nil {
							entries = append(entries, corebatchout.Entry{
								BatchID:   savedDiff.BatchMeta.BatchID,
								Tag:       it.OutboundSpec.Tag,
								ServerID:  it.OutboundSpec.ServerID,
								Protocol:  it.OutboundSpec.Protocol,
								CreatedAt: now,
							})
						}
					}
					if len(entries) > 0 {
						if err := s.deps.BatchOutbounds.InsertTx(ctx, tx, entries); err != nil {
							return err
						}
					}
				}
			}
		}
		if s.deps.Batch != nil {
			if err := s.deps.Batch.RecordTx(ctx, tx, id, ev.AffectedCount, "apply", now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 4. 数据库已提交：逐台串行推送规范 kernel.json 至受影响节点；任一节点失败不回滚已成功节点，汇总返回 partial_failure（退出码 8）。
	serverResults := make([]ServerApplyResult, 0, len(ev.AffectedServers))
	failedCount := 0
	sdiffByID := make(map[int64]ServerConfigDiff, len(ev.Servers))
	for _, sd := range ev.Servers {
		sdiffByID[sd.ServerID] = sd
	}
	for _, srvID := range ev.AffectedServers {
		srv, err := s.deps.Servers.ByID(ctx, srvID)
		if err != nil {
			failedCount++
			serverResults = append(serverResults, ServerApplyResult{
				ServerID: srvID, Status: "failed", Code: string(v1.AsError(err).Code), Error: v1.AsError(err).Reason,
			})
			continue
		}
		rendered, err := s.RenderServerConfig(ctx, srvID)
		if err != nil {
			failedCount++
			serverResults = append(serverResults, ServerApplyResult{
				ServerID: srvID, ServerName: srv.Spec.Name, Status: "failed", Code: string(v1.AsError(err).Code), Error: v1.AsError(err).Reason,
			})
			continue
		}
		// 若节点已同步且渲染配置前后无变化（例如仅改服务器地区标签等元数据），无需重启内核。
		sd := sdiffByID[srvID]
		if !sd.ConfigChanged && srv.Status.AppliedHash != "" && srv.Status.AppliedHash == rendered.Hash {
			serverResults = append(serverResults, ServerApplyResult{
				ServerID:    srvID,
				ServerName:  srv.Spec.Name,
				Status:      "unchanged",
				AppliedHash: srv.Status.AppliedHash,
				Generation:  uint64(srv.Status.AppliedGeneration),
				Inbounds:    rendered.Inbounds,
			})
			continue
		}
		if s.deps.Nodes == nil {
			failedCount++
			serverResults = append(serverResults, ServerApplyResult{
				ServerID: srvID, ServerName: srv.Spec.Name, Status: "failed", Code: string(v1.CodeUnavailable), Error: "节点通道未装配",
			})
			continue
		}
		params := xrpc.ConfigApplyParams{
			Config:  string(rendered.Pretty),
			ApplyID: applyID,
		}
		if srv.Status.AppliedHash == "" || srv.Status.FirstApplyEligible {
			params.ForceInitial = true
		} else {
			params.ExpectedCurrentHash = srv.Status.AppliedHash
		}
		res, callErr := s.deps.Nodes.CallConfigApply(ctx, srvID, params)
		if callErr != nil {
			ae := v1.AsError(callErr)
			failedCount++
			serverResults = append(serverResults, ServerApplyResult{
				ServerID:   srvID,
				ServerName: srv.Spec.Name,
				Status:     "failed",
				Code:       string(ae.Code),
				Error:      ae.Reason,
			})
			continue
		}
		if !res.Applied {
			failedCount++
			serverResults = append(serverResults, ServerApplyResult{
				ServerID:      srvID,
				ServerName:    srv.Spec.Name,
				Status:        "failed",
				Code:          string(v1.CodeInternal),
				Error:         res.Error,
				RolledBack:    res.RolledBack,
				RollbackError: res.RollbackError,
				CurrentHash:   res.CurrentHash,
			})
			continue
		}
		if err := s.deps.Servers.MarkAppliedConfig(ctx, srvID, res.AppliedHash, int64(res.Generation)); err != nil {
			failedCount++
			serverResults = append(serverResults, ServerApplyResult{
				ServerID: srvID, ServerName: srv.Spec.Name, Status: "failed",
				Code: string(v1.AsError(err).Code), Error: "节点已应用配置但更新主控 applied_hash 失败：" + v1.AsError(err).Reason,
				AppliedHash: res.AppliedHash, Generation: res.Generation,
			})
			continue
		}
		serverResults = append(serverResults, ServerApplyResult{
			ServerID:    srvID,
			ServerName:  srv.Spec.Name,
			Status:      "applied",
			AppliedHash: res.AppliedHash,
			Generation:  res.Generation,
			Inbounds:    res.Inbounds,
		})
	}

	if failedCount > 0 {
		errOut := v1.Newf(v1.CodePartialFailure, "配置已写入主控库（apply_id=%s），但 %d/%d 台节点未成功落地", applyID, failedCount, len(ev.AffectedServers)).
			WithState("plan_id", planID).
			WithState("apply_id", applyID).
			WithState("objects", appliedSummaries).
			WithState("results", serverResults).
			WithNext("查看 state.results 排查离线或回滚节点，修复后重新 satchel plan && satchel apply 收敛")
		if batchID != "" {
			errOut = errOut.WithState("batch_id", batchID)
		}
		return nil, errOut
	}

	return &ApplyResult{
		PlanID:        planID,
		ApplyID:       applyID,
		BatchID:       batchID,
		AffectedCount: ev.AffectedCount,
		Objects:       appliedSummaries,
		Servers:       serverResults,
	}, nil
}

func (s *Service) enforceDynamicDangers(ctx context.Context, id v1.Identity, base map[v1.Danger]bool, plannedObjs []PlannedObject, confirm string, now time.Time) error {
	th, used, err := s.loadThresholdsAndUsed(ctx, id, nil, now)
	if err != nil {
		return err
	}
	return s.enforceDynamicDangersTyped(id, base, plannedObjs, confirm, used, th)
}

func (s *Service) enforceDynamicDangersTyped(id v1.Identity, base map[v1.Danger]bool, plannedObjs []PlannedObject, confirm string, used int64, th coresettings.BatchThresholds) error {
	required := map[v1.Danger]bool{}
	for d, ok := range base {
		if ok {
			required[d] = true
		}
	}
	affected := int64(len(plannedObjs))
	if affected > int64(th.Single) || used+affected > int64(th.Window) {
		required[v1.DangerBatch] = true
	}
	dangers := orderedDangers(required)
	if len(dangers) == 0 {
		return nil
	}
	for _, d := range dangers {
		if !id.HasDanger(d) {
			return v1.Newf(v1.CodeForbidden, "apply 命中危险操作的%s，当前身份没有开这一类", dangerChinese[d]).
				WithState("required_danger", string(d)).
				WithState("required_dangers", dangers).
				WithState("danger", id.Danger)
		}
	}
	expected := expectedConfirmFor(dangers, plannedObjs)
	if confirm == "" || confirm != expected {
		kind := string(command.ConfirmObject)
		if (len(dangers) == 1 && dangers[0] == v1.DangerBatch) || len(plannedObjs) > 1 {
			kind = string(command.ConfirmCount)
		}
		return v1.Newf(v1.CodeConfirmRequired, "apply 命中危险操作，需要用 --confirm %s 确认", expected).
			WithState("kind", kind).
			WithState("expected", expected).
			WithState("required_dangers", dangers).
			WithNext("重新执行并加上 --confirm " + expected)
	}
	return nil
}

func expectedConfirmFor(dangers []v1.Danger, plannedObjs []PlannedObject) string {
	affected := int64(len(plannedObjs))
	if (len(dangers) == 1 && dangers[0] == v1.DangerBatch) || affected > 1 {
		return strconv.FormatInt(affected, 10)
	}
	if len(plannedObjs) == 1 {
		return plannedObjs[0].Name
	}
	return strconv.FormatInt(affected, 10)
}

func (s *Service) loadThresholdsAndUsed(ctx context.Context, id v1.Identity, tx *bun.Tx, now time.Time) (coresettings.BatchThresholds, int64, error) {
	th := coresettings.DefaultBatchThresholds()
	if s.deps.Settings != nil {
		if loaded, err := s.deps.Settings.LoadBatchThresholds(ctx); err == nil {
			th = loaded
		}
	}
	var used int64
	if s.deps.Batch != nil {
		since := now.Add(-time.Duration(th.WindowMinutes) * time.Minute)
		var err error
		if tx != nil {
			used, err = s.deps.Batch.WindowUsedTx(ctx, *tx, id, since)
		} else {
			used, err = s.deps.Batch.WindowUsed(ctx, id, since)
		}
		if err != nil {
			return th, 0, err
		}
	}
	return th, used, nil
}

func (s *Service) enforceBatchWindowInTx(ctx context.Context, tx bun.Tx, id v1.Identity, plannedObjs []PlannedObject, base map[v1.Danger]bool, confirm string, now time.Time) error {
	th, used, err := s.loadThresholdsAndUsed(ctx, id, &tx, now)
	if err != nil {
		return err
	}
	return s.enforceDynamicDangersTyped(id, base, plannedObjs, confirm, used, th)
}

func (s *Service) applyOneTx(ctx context.Context, tx bun.Tx, it evaluatedItem, force bool) (AppliedObjectSummary, error) {
	switch it.Op.Kind {
	case coreservers.KindName:
		var out *coreservers.Server
		var err error
		switch it.Op.Op {
		case "create":
			out, err = s.deps.Servers.CreateFromSpecTx(ctx, tx, *it.ServerSpec)
		case "update":
			out, err = s.deps.Servers.UpdateSpecTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force, *it.ServerSpec)
		case "delete":
			out, err = s.deps.Servers.SoftDeleteTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		case "restore":
			out, err = s.deps.Servers.RestoreTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		}
		if err != nil {
			return AppliedObjectSummary{}, err
		}
		return AppliedObjectSummary{Op: it.Op.Op, Kind: it.Op.Kind, ID: out.Metadata.ID, Name: out.Spec.Name, ResourceVersion: out.Metadata.ResourceVersion}, nil

	case coreinbounds.KindName:
		var out *coreinbounds.Inbound
		var err error
		switch it.Op.Op {
		case "create":
			out, err = s.deps.Inbounds.CreateTx(ctx, tx, *it.InboundSpec)
		case "update":
			out, err = s.deps.Inbounds.UpdateTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force, *it.InboundSpec)
		case "delete":
			out, err = s.deps.Inbounds.SoftDeleteTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		case "restore":
			out, err = s.deps.Inbounds.RestoreTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		}
		if err != nil {
			return AppliedObjectSummary{}, err
		}
		return AppliedObjectSummary{Op: it.Op.Op, Kind: it.Op.Kind, ID: out.Metadata.ID, Name: out.Metadata.Name, ResourceVersion: out.Metadata.ResourceVersion}, nil

	case coreoutbounds.KindName:
		var out *coreoutbounds.Outbound
		var err error
		switch it.Op.Op {
		case "create":
			out, err = s.deps.Outbounds.CreateTx(ctx, tx, *it.OutboundSpec)
		case "update":
			out, err = s.deps.Outbounds.UpdateTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force, *it.OutboundSpec)
		case "delete":
			out, err = s.deps.Outbounds.SoftDeleteTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		case "restore":
			out, err = s.deps.Outbounds.RestoreTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		}
		if err != nil {
			return AppliedObjectSummary{}, err
		}
		return AppliedObjectSummary{Op: it.Op.Op, Kind: it.Op.Kind, ID: out.Metadata.ID, Name: out.Metadata.Name, ResourceVersion: out.Metadata.ResourceVersion}, nil

	case corerouting.KindName:
		var out *corerouting.RoutingRule
		var err error
		switch it.Op.Op {
		case "create":
			out, err = s.deps.RoutingRules.CreateTx(ctx, tx, *it.RoutingRuleSpec)
		case "update":
			out, err = s.deps.RoutingRules.UpdateTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force, *it.RoutingRuleSpec)
		case "delete":
			out, err = s.deps.RoutingRules.SoftDeleteTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		case "restore":
			out, err = s.deps.RoutingRules.RestoreTx(ctx, tx, it.Op.ID, it.Op.ResourceVersion, force)
		}
		if err != nil {
			return AppliedObjectSummary{}, err
		}
		return AppliedObjectSummary{Op: it.Op.Op, Kind: it.Op.Kind, ID: out.Metadata.ID, Name: fmt.Sprintf("RoutingRule#%d", out.Metadata.ID), ResourceVersion: out.Metadata.ResourceVersion}, nil
	}
	return AppliedObjectSummary{}, v1.Newf(v1.CodeBadRequest, "不支持的 kind %q", it.Op.Kind)
}

func (s *Service) snapshotList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	beforeID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	f := coresnapshots.Filter{
		Kind:     strings.TrimSpace(inv.String("kind", "")),
		ObjectID: int64(inv.Int("object-id", 0)),
		ApplyID:  strings.TrimSpace(inv.String("apply-id", "")),
	}
	total, err := s.deps.Snapshots.Count(ctx, f)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.Snapshots.List(ctx, f, beforeID, page.Limit+1)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].ID)
	}
	for _, r := range rows {
		res.Items = append(res.Items, r)
	}
	return res, nil
}

func (s *Service) snapshotGet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(inv.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "快照 id 必须是正整数，得到 %q", inv.Arg(0))
	}
	snap, err := s.deps.Snapshots.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets) {
		snap.Content = singbox.RedactJSON(snap.Content, false)
	}
	return snap, nil
}

func (s *Service) snapshotRollback(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	snapID := int64(inv.Int("id", 0))
	applyID := strings.TrimSpace(inv.String("apply-id", ""))
	if (snapID > 0 && applyID != "") || (snapID <= 0 && applyID == "") {
		return nil, v1.New(v1.CodeBadRequest, "snapshot rollback 必须且只能提供 --id 或 --apply-id 之一")
	}

	var targets []*coresnapshots.Snapshot
	if snapID > 0 {
		snap, err := s.deps.Snapshots.Get(ctx, snapID)
		if err != nil {
			return nil, err
		}
		targets = []*coresnapshots.Snapshot{snap}
	} else {
		list, err := s.deps.Snapshots.ListByApplyID(ctx, applyID)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, v1.Newf(v1.CodeNotFound, "没有找到 apply_id 为 %q 的配置快照", applyID)
		}
		targets = list
	}

	seen := map[string]bool{}
	var ops []Operation
	var skipped []SkippedObject
	for _, snap := range targets {
		if snap.ObjectKind == "SystemSettings" {
			return nil, v1.New(v1.CodeBadRequest, "SystemSettings 快照不能经 snapshot rollback 回滚").
				WithNext("请改用 satchel settings rollback <id> --resource-version <N>")
		}
		if !supportedKind(snap.ObjectKind) {
			skipped = append(skipped, SkippedObject{SnapshotID: snap.ID, Kind: snap.ObjectKind, ID: snap.ObjectID, Reason: "当前版本不支持经 plan/apply 回滚该类型"})
			continue
		}
		key := fmt.Sprintf("%s#%d", snap.ObjectKind, snap.ObjectID)
		if seen[key] {
			continue
		}
		seen[key] = true

		curVer, deleted, exists, err := s.currentObjectState(ctx, snap.ObjectKind, snap.ObjectID)
		if err != nil {
			return nil, err
		}
		if !exists {
			skipped = append(skipped, SkippedObject{SnapshotID: snap.ID, Kind: snap.ObjectKind, ID: snap.ObjectID, Reason: "目标对象不存在"})
			continue
		}
		if deleted {
			skipped = append(skipped, SkippedObject{SnapshotID: snap.ID, Kind: snap.ObjectKind, ID: snap.ObjectID, Reason: "目标对象已软删除，回滚不会复活已删除对象"})
			continue
		}
		ops = append(ops, Operation{
			Op:              "update",
			Kind:            snap.ObjectKind,
			ID:              snap.ObjectID,
			ResourceVersion: curVer,
			Spec:            snap.Content,
		})
	}
	if len(ops) == 0 {
		return nil, v1.New(v1.CodeConflict, "没有可回滚的活动对象（全部目标均已软删除或不存在）").
			WithState("skipped", skipped).
			WithNext("查看 state.skipped 了解跳过原因；如需恢复已删除对象请显式执行 restore 计划")
	}
	return s.createPlanFromOps(ctx, ops, skipped, nil)
}

func (s *Service) currentObjectState(ctx context.Context, kind string, id int64) (version int64, deleted bool, exists bool, err error) {
	switch kind {
	case coreservers.KindName:
		obj, err := s.deps.Servers.ByIDIncludeDeleted(ctx, id)
		if errors.Is(err, coreservers.ErrNotFound) {
			return 0, false, false, nil
		}
		if err != nil {
			return 0, false, false, err
		}
		return obj.Metadata.ResourceVersion, obj.Metadata.DeletedAt != nil, true, nil
	case coreinbounds.KindName:
		obj, err := s.deps.Inbounds.ByIDIncludeDeleted(ctx, id)
		if errors.Is(err, coreinbounds.ErrNotFound) {
			return 0, false, false, nil
		}
		if err != nil {
			return 0, false, false, err
		}
		return obj.Metadata.ResourceVersion, obj.Metadata.DeletedAt != nil, true, nil
	case coreoutbounds.KindName:
		obj, err := s.deps.Outbounds.ByIDIncludeDeleted(ctx, id)
		if errors.Is(err, coreoutbounds.ErrNotFound) {
			return 0, false, false, nil
		}
		if err != nil {
			return 0, false, false, err
		}
		return obj.Metadata.ResourceVersion, obj.Metadata.DeletedAt != nil, true, nil
	case corerouting.KindName:
		obj, err := s.deps.RoutingRules.ByIDIncludeDeleted(ctx, id)
		if errors.Is(err, corerouting.ErrNotFound) {
			return 0, false, false, nil
		}
		if err != nil {
			return 0, false, false, err
		}
		return obj.Metadata.ResourceVersion, obj.Metadata.DeletedAt != nil, true, nil
	}
	return 0, false, false, nil
}
