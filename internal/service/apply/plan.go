package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	coreplans "github.com/satchel/satchel/internal/core/plans"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// serverConnCriticalFields 变更这些连接字段时自动归入 DangerPermission。
var serverConnCriticalFields = map[string]bool{
	"connection_mode": true,
	"listen_port":     true,
	"pull_port":       true,
	"domain":          true,
	"domain_v6":       true,
	"pull_address":    true,
	"pull_address_v6": true,
}

// serverNginxCriticalFields 在同机节点上变更这些 nginx/443 字段时触发七组动态人类当场验证。
var serverNginxCriticalFields = map[string]bool{
	"use_443":    true,
	"steal_mode": true,
	"site_type":  true,
	"site_value": true,
}

type evaluatedItem struct {
	Op                   Operation
	Planned              PlannedObject
	CurrentVersion       int64
	CurrentBefore        json.RawMessage // 写前快照原文（不含 Server 令牌）
	ServerSpec           *v1.ServerSpec
	InboundSpec          *v1.InboundSpec
	OutboundSpec         *v1.OutboundSpec
	RoutingRuleSpec      *v1.RoutingRuleSpec
	WebsiteSpec          *v1.WebsiteSpec
	WebsiteRemovedDomain string
	TouchesSameHostNginx bool
}

type evalResult struct {
	Items                  []evaluatedItem
	Servers                []ServerConfigDiff
	BaseDangers            []v1.Danger
	AffectedCount          int64
	AffectedServers        []int64 // 排序后需要推送配置的已有活跃服务器 ID
	KernelAffectedServers  map[int64]bool
	WebsiteAffectedServers map[int64]bool
	DeletedServerIDs       map[int64]bool
	RequiresHuman          bool
}

func (s *Service) planCmd(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ops, err := parseOperations(inv.String("spec", ""))
	if err != nil {
		return nil, err
	}
	view, err := s.createPlanFromOps(ctx, ops, nil, nil)
	if err != nil {
		return nil, err
	}
	return view, nil
}

// CreatePlanFromJSON 供同层编排服务经接口复用 plan 校验与持久化（不跨 service/* import），并记录 audit_logs.plan_id。
func (s *Service) CreatePlanFromJSON(ctx context.Context, specJSON, batchKind, batchID string) (any, error) {
	ops, err := parseOperations(specJSON)
	if err != nil {
		return nil, err
	}
	var meta *BatchMeta
	if strings.TrimSpace(batchKind) != "" && strings.TrimSpace(batchID) != "" {
		meta = &BatchMeta{Kind: strings.TrimSpace(batchKind), BatchID: strings.TrimSpace(batchID)}
	}
	return s.createPlanFromOps(ctx, ops, nil, meta)
}

func (s *Service) createPlanFromOps(ctx context.Context, ops []Operation, skipped []SkippedObject, meta *BatchMeta) (*PlanView, error) {
	ev, err := s.evaluateOps(ctx, ops, true)
	if err != nil {
		return nil, err
	}
	id := v1.IdentityFrom(ctx)
	now := s.now()
	dangers, confirmHint, bw, err := s.computeDangers(ctx, id, ev, false, now)
	if err != nil {
		return nil, err
	}
	plannedObjs := make([]PlannedObject, len(ev.Items))
	for i, it := range ev.Items {
		plannedObjs[i] = it.Planned
	}
	diffData := PlanDiffData{
		Objects:         plannedObjs,
		Servers:         ev.Servers,
		RequiredDangers: dangers,
		ConfirmHint:     confirmHint,
		RequiresHuman:   ev.RequiresHuman,
		BatchMeta:       meta,
	}
	objectsJSON, err := json.Marshal(plannedObjs)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码计划对象清单失败", err)
	}
	diffJSON, err := json.Marshal(diffData)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码计划 diff 失败", err)
	}
	p, err := s.deps.Plans.Create(ctx, coreplans.CreateParams{
		Objects:       objectsJSON,
		Diff:          diffJSON,
		AffectedCount: ev.AffectedCount,
		ExpiresAt:     now.Add(s.deps.PlanTTL),
	})
	if err != nil {
		return nil, err
	}
	command.RecordPlanID(ctx, p.Metadata.ID)
	view := buildPlanView(p, diffData, bw, skipped, now, id.HasScope(v1.ScopeSecrets))
	return view, nil
}

// evaluateOps 按当前库实时状态模拟一组操作，计算逐对象前后快照、字段级 diff 与每台受影响服务器的规范 sing-box diff。
// checkVersion 为 true（plan 阶段）时若版本号不匹配立即返回 version_conflict；为 false（apply 重算阶段）时交由 apply 结合 --force 统一判定。
func (s *Service) evaluateOps(ctx context.Context, ops []Operation, checkVersion bool) (*evalResult, error) {
	ev := &evalResult{
		Items:                  make([]evaluatedItem, 0, len(ops)),
		AffectedCount:          int64(len(ops)),
		KernelAffectedServers:  map[int64]bool{},
		WebsiteAffectedServers: map[int64]bool{},
		DeletedServerIDs:       map[int64]bool{},
	}
	dangerSet := map[v1.Danger]bool{}
	affectedServerSet := map[int64]bool{}
	// 检查批内新建同名冲突。
	createdServerNames := map[string]bool{}
	createdInboundTags := map[string]bool{}
	createdOutboundTags := map[string]bool{}
	createdWebsiteDomains := map[string]bool{}

	for _, op := range ops {
		item, srvID, itemDangers, err := s.evaluateOne(ctx, op, checkVersion)
		if err != nil {
			return nil, err
		}
		// 校验批内同名创建冲突。
		if op.Op == "create" {
			switch op.Kind {
			case coreservers.KindName:
				if createdServerNames[item.ServerSpec.Name] {
					return nil, v1.Newf(v1.CodeNameTaken, "单次 plan 中重复创建同名服务器 %q", item.ServerSpec.Name)
				}
				createdServerNames[item.ServerSpec.Name] = true
			case coreinbounds.KindName:
				k := fmt.Sprintf("%d/%s", item.InboundSpec.ServerID, item.InboundSpec.Tag)
				if createdInboundTags[k] {
					return nil, v1.Newf(v1.CodeNameTaken, "单次 plan 中重复创建同名入站 %q", k)
				}
				createdInboundTags[k] = true
			case coreoutbounds.KindName:
				k := fmt.Sprintf("%d/%s", item.OutboundSpec.ServerID, item.OutboundSpec.Tag)
				if createdOutboundTags[k] {
					return nil, v1.Newf(v1.CodeNameTaken, "单次 plan 中重复创建同名出站 %q", k)
				}
				createdOutboundTags[k] = true
			case corewebsites.KindName:
				k := fmt.Sprintf("%d/%s", item.WebsiteSpec.ServerID, item.WebsiteSpec.Domain)
				if createdWebsiteDomains[k] {
					return nil, v1.Newf(v1.CodeNameTaken, "单次 plan 中重复创建同名站点 %q", k)
				}
				createdWebsiteDomains[k] = true
			}
		}
		for _, d := range itemDangers {
			dangerSet[d] = true
		}
		if item.TouchesSameHostNginx {
			ev.RequiresHuman = true
		}
		if op.Kind == coreservers.KindName && op.Op == "delete" {
			ev.DeletedServerIDs[op.ID] = true
		}
		if srvID > 0 {
			affectedServerSet[srvID] = true
			if op.Kind == corewebsites.KindName {
				ev.WebsiteAffectedServers[srvID] = true
			} else {
				ev.KernelAffectedServers[srvID] = true
			}
		}
		ev.Items = append(ev.Items, item)
	}

	// 计算受影响服务器的渲染前后配置 diff。
	serverIDs := make([]int64, 0, len(affectedServerSet))
	for id := range affectedServerSet {
		if ev.DeletedServerIDs[id] {
			continue
		}
		serverIDs = append(serverIDs, id)
	}
	sort.Slice(serverIDs, func(i, j int) bool { return serverIDs[i] < serverIDs[j] })
	ev.AffectedServers = serverIDs

	ev.Servers = make([]ServerConfigDiff, 0, len(serverIDs))
	for _, srvID := range serverIDs {
		sdiff, err := s.simulateServerConfig(ctx, srvID, ev.Items, ev.KernelAffectedServers[srvID], ev.WebsiteAffectedServers[srvID])
		if err != nil {
			return nil, err
		}
		ev.Servers = append(ev.Servers, sdiff)
	}

	ev.BaseDangers = orderedDangers(dangerSet)
	return ev, nil
}

func (s *Service) evaluateOne(ctx context.Context, op Operation, checkVersion bool) (evaluatedItem, int64, []v1.Danger, error) {
	switch op.Kind {
	case coreservers.KindName:
		return s.evalServer(ctx, op, checkVersion)
	case coreinbounds.KindName:
		return s.evalInbound(ctx, op, checkVersion)
	case coreoutbounds.KindName:
		return s.evalOutbound(ctx, op, checkVersion)
	case corerouting.KindName:
		return s.evalRoutingRule(ctx, op, checkVersion)
	case corewebsites.KindName:
		return s.evalWebsite(ctx, op, checkVersion)
	default:
		return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "不支持的 kind %q", op.Kind)
	}
}

func (s *Service) evalServer(ctx context.Context, op Operation, checkVersion bool) (evaluatedItem, int64, []v1.Danger, error) {
	switch op.Op {
	case "create":
		spec := coreservers.DefaultSpec()
		if err := v1.DecodeSpec(v1.Kind(coreservers.KindName), op.Spec, &spec); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		norm, err := coreservers.NormalizeSpec(spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if _, err := s.deps.Servers.ByNameIncludeDeleted(ctx, norm.Name); err == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "Server 名字 %q 已被占用（含保留期内软删除对象）", norm.Name)
		} else if !errors.Is(err, coreservers.ErrNotFound) {
			return evaluatedItem{}, 0, nil, err
		}
		afterRaw, afterMap, err := marshalRawServerSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{}
		for k, v := range afterMap {
			diff[k] = FieldChange{Before: nil, After: v}
		}
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, Name: norm.Name, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff), AfterSpec: afterRaw,
		}
		return evaluatedItem{Op: op, Planned: p, ServerSpec: &norm}, 0, nil, nil

	case "update":
		cur, err := s.deps.Servers.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreservers.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Server 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeConflict, "Server %d 处于软删除状态，不能修改 spec（请先 restore）", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Server", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		var tmp v1.ServerSpec
		if err := v1.DecodeSpec(v1.Kind(coreservers.KindName), op.Spec, &tmp); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		beforeRaw, beforeMap, err := marshalRawServerSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedMap, explicitKeys, err := mergeSpecMap(beforeMap, op.Spec, nil)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedBytes, err := json.Marshal(mergedMap)
		if err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeInternal, "合并 Server spec 失败", err)
		}
		var merged v1.ServerSpec
		if err := json.Unmarshal(mergedBytes, &merged); err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeBadRequest, "Server.spec 字段类型不合法", err)
		}
		norm, err := coreservers.NormalizeSpec(merged)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if norm.Name != cur.Spec.Name {
			if _, err := s.deps.Servers.ByNameIncludeDeleted(ctx, norm.Name); err == nil {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "Server 名字 %q 已被占用", norm.Name)
			} else if !errors.Is(err, coreservers.ErrNotFound) {
				return evaluatedItem{}, 0, nil, err
			}
		}
		afterRaw, afterMap, err := marshalRawServerSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := computeFieldDiff(beforeMap, afterMap, explicitKeys)
		var dangers []v1.Danger
		touchesSameHostNginx := false
		for k := range diff {
			if serverConnCriticalFields[k] {
				dangers = []v1.Danger{v1.DangerPermission}
			}
			if cur.Status.SameHostAsMaster && serverNginxCriticalFields[k] {
				touchesSameHostNginx = true
			}
		}
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: cur.Spec.Name, ServerID: op.ID,
			ExpectedVersion: op.ResourceVersion, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: afterRaw, Dangers: dangers,
		}
		return evaluatedItem{
			Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw,
			ServerSpec: &norm, TouchesSameHostNginx: touchesSameHostNginx,
		}, op.ID, dangers, nil

	case "delete":
		cur, err := s.deps.Servers.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreservers.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Server 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Server %d 已经是软删除状态，不能重复删除", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Server", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawServerSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: false, After: true}}
		dangers := []v1.Danger{v1.DangerDelete}
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: cur.Spec.Name, ServerID: op.ID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw}, op.ID, dangers, nil

	case "restore":
		cur, err := s.deps.Servers.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreservers.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Server 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Server %d 没有被删除，不需要恢复", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Server", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawServerSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: true, After: false}}
		dangers := []v1.Danger{v1.DangerDelete}
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: cur.Spec.Name, ServerID: op.ID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw, ServerSpec: &cur.Spec}, op.ID, dangers, nil
	}
	return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "不支持的操作 %q", op.Op)
}

func (s *Service) evalInbound(ctx context.Context, op Operation, checkVersion bool) (evaluatedItem, int64, []v1.Danger, error) {
	maskedFields := map[string]bool{"tls": true, "settings": true}
	switch op.Op {
	case "create":
		var spec v1.InboundSpec
		if err := v1.DecodeSpec(v1.Kind(coreinbounds.KindName), op.Spec, &spec); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		norm, err := coreinbounds.NormalizeSpec(spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if err := s.ensureActiveServer(ctx, norm.ServerID); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if _, err := s.deps.Inbounds.ByServerTagIncludeDeleted(ctx, norm.ServerID, norm.Tag); err == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "服务器 %d 下的入站 tag %q 已被占用", norm.ServerID, norm.Tag)
		} else if !errors.Is(err, coreinbounds.ErrNotFound) {
			return evaluatedItem{}, 0, nil, err
		}
		afterRaw, afterMap, err := marshalRawInboundSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{}
		for k, v := range afterMap {
			diff[k] = FieldChange{Before: nil, After: v}
		}
		name := fmt.Sprintf("%d/%s", norm.ServerID, norm.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, Name: name, ServerID: norm.ServerID, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff), AfterSpec: afterRaw,
		}
		return evaluatedItem{Op: op, Planned: p, InboundSpec: &norm}, norm.ServerID, nil, nil

	case "update":
		cur, err := s.deps.Inbounds.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreinbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Inbound 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeConflict, "Inbound %d 处于软删除状态，不能修改 spec（请先 restore）", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Inbound", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		var tmp v1.InboundSpec
		if err := v1.DecodeSpec(v1.Kind(coreinbounds.KindName), op.Spec, &tmp); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		beforeRaw, beforeMap, err := marshalRawInboundSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedMap, explicitKeys, err := mergeSpecMap(beforeMap, op.Spec, maskedFields)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedBytes, err := json.Marshal(mergedMap)
		if err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeInternal, "合并 Inbound spec 失败", err)
		}
		var merged v1.InboundSpec
		if err := json.Unmarshal(mergedBytes, &merged); err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeBadRequest, "Inbound.spec 字段类型不合法", err)
		}
		if merged.ServerID != cur.Spec.ServerID {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Inbound %d 不允许跨服务器修改 server_id", op.ID)
		}
		norm, err := coreinbounds.NormalizeSpec(merged)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if norm.Tag != cur.Spec.Tag {
			if _, err := s.deps.Inbounds.ByServerTagIncludeDeleted(ctx, norm.ServerID, norm.Tag); err == nil {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "服务器 %d 下的入站 tag %q 已被占用", norm.ServerID, norm.Tag)
			} else if !errors.Is(err, coreinbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, err
			}
		}
		afterRaw, afterMap, err := marshalRawInboundSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := computeFieldDiff(beforeMap, afterMap, explicitKeys)
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: afterRaw,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw, InboundSpec: &norm}, cur.Spec.ServerID, nil, nil

	case "delete":
		cur, err := s.deps.Inbounds.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreinbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Inbound 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Inbound %d 已经是软删除状态", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Inbound", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawInboundSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: false, After: true}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw}, cur.Spec.ServerID, dangers, nil

	case "restore":
		cur, err := s.deps.Inbounds.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreinbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Inbound 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Inbound %d 没有被删除，不需要恢复", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Inbound", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawInboundSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: true, After: false}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw, InboundSpec: &cur.Spec}, cur.Spec.ServerID, dangers, nil
	}
	return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "不支持的操作 %q", op.Op)
}

func (s *Service) evalOutbound(ctx context.Context, op Operation, checkVersion bool) (evaluatedItem, int64, []v1.Danger, error) {
	maskedFields := map[string]bool{"settings": true}
	switch op.Op {
	case "create":
		var spec v1.OutboundSpec
		if err := v1.DecodeSpec(v1.Kind(coreoutbounds.KindName), op.Spec, &spec); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		norm, err := coreoutbounds.NormalizeSpec(spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if err := s.ensureActiveServer(ctx, norm.ServerID); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if _, err := s.deps.Outbounds.ByServerTagIncludeDeleted(ctx, norm.ServerID, norm.Tag); err == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "服务器 %d 下的出站 tag %q 已被占用", norm.ServerID, norm.Tag)
		} else if !errors.Is(err, coreoutbounds.ErrNotFound) {
			return evaluatedItem{}, 0, nil, err
		}
		afterRaw, afterMap, err := marshalRawOutboundSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{}
		for k, v := range afterMap {
			diff[k] = FieldChange{Before: nil, After: v}
		}
		name := fmt.Sprintf("%d/%s", norm.ServerID, norm.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, Name: name, ServerID: norm.ServerID, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff), AfterSpec: afterRaw,
		}
		return evaluatedItem{Op: op, Planned: p, OutboundSpec: &norm}, norm.ServerID, nil, nil

	case "update":
		cur, err := s.deps.Outbounds.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreoutbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Outbound 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeConflict, "Outbound %d 处于软删除状态，不能修改 spec（请先 restore）", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Outbound", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		var tmp v1.OutboundSpec
		if err := v1.DecodeSpec(v1.Kind(coreoutbounds.KindName), op.Spec, &tmp); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		beforeRaw, beforeMap, err := marshalRawOutboundSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedMap, explicitKeys, err := mergeSpecMap(beforeMap, op.Spec, maskedFields)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedBytes, err := json.Marshal(mergedMap)
		if err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeInternal, "合并 Outbound spec 失败", err)
		}
		var merged v1.OutboundSpec
		if err := json.Unmarshal(mergedBytes, &merged); err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeBadRequest, "Outbound.spec 字段类型不合法", err)
		}
		if merged.ServerID != cur.Spec.ServerID {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Outbound %d 不允许跨服务器修改 server_id", op.ID)
		}
		norm, err := coreoutbounds.NormalizeSpec(merged)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if norm.Tag != cur.Spec.Tag {
			if _, err := s.deps.Outbounds.ByServerTagIncludeDeleted(ctx, norm.ServerID, norm.Tag); err == nil {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "服务器 %d 下的出站 tag %q 已被占用", norm.ServerID, norm.Tag)
			} else if !errors.Is(err, coreoutbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, err
			}
		}
		afterRaw, afterMap, err := marshalRawOutboundSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := computeFieldDiff(beforeMap, afterMap, explicitKeys)
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: afterRaw,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw, OutboundSpec: &norm}, cur.Spec.ServerID, nil, nil

	case "delete":
		cur, err := s.deps.Outbounds.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreoutbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Outbound 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Outbound %d 已经是软删除状态", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Outbound", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawOutboundSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: false, After: true}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw}, cur.Spec.ServerID, dangers, nil

	case "restore":
		cur, err := s.deps.Outbounds.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, coreoutbounds.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Outbound 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Outbound %d 没有被删除，不需要恢复", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Outbound", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawOutboundSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: true, After: false}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Tag)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw, OutboundSpec: &cur.Spec}, cur.Spec.ServerID, dangers, nil
	}
	return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "不支持的操作 %q", op.Op)
}

func (s *Service) evalRoutingRule(ctx context.Context, op Operation, checkVersion bool) (evaluatedItem, int64, []v1.Danger, error) {
	switch op.Op {
	case "create":
		var spec v1.RoutingRuleSpec
		if err := v1.DecodeSpec(v1.Kind(corerouting.KindName), op.Spec, &spec); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		norm, err := corerouting.NormalizeSpec(spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if err := s.ensureActiveServer(ctx, norm.ServerID); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		afterRaw, afterMap, err := marshalRawRoutingRuleSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{}
		for k, v := range afterMap {
			diff[k] = FieldChange{Before: nil, After: v}
		}
		name := fmt.Sprintf("RoutingRule@server:%d", norm.ServerID)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, Name: name, ServerID: norm.ServerID, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff), AfterSpec: afterRaw,
		}
		return evaluatedItem{Op: op, Planned: p, RoutingRuleSpec: &norm}, norm.ServerID, nil, nil

	case "update":
		cur, err := s.deps.RoutingRules.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, corerouting.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "RoutingRule 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeConflict, "RoutingRule %d 处于软删除状态，不能修改 spec（请先 restore）", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("RoutingRule", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		var tmp v1.RoutingRuleSpec
		if err := v1.DecodeSpec(v1.Kind(corerouting.KindName), op.Spec, &tmp); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		beforeRaw, beforeMap, err := marshalRawRoutingRuleSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedMap, explicitKeys, err := mergeSpecMap(beforeMap, op.Spec, nil)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedBytes, err := json.Marshal(mergedMap)
		if err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeInternal, "合并 RoutingRule spec 失败", err)
		}
		var merged v1.RoutingRuleSpec
		if err := json.Unmarshal(mergedBytes, &merged); err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeBadRequest, "RoutingRule.spec 字段类型不合法", err)
		}
		if merged.ServerID != cur.Spec.ServerID {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "RoutingRule %d 不允许跨服务器修改 server_id", op.ID)
		}
		norm, err := corerouting.NormalizeSpec(merged)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		afterRaw, afterMap, err := marshalRawRoutingRuleSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := computeFieldDiff(beforeMap, afterMap, explicitKeys)
		name := fmt.Sprintf("RoutingRule#%d", op.ID)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: afterRaw,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw, RoutingRuleSpec: &norm}, cur.Spec.ServerID, nil, nil

	case "delete":
		cur, err := s.deps.RoutingRules.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, corerouting.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "RoutingRule 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "RoutingRule %d 已经是软删除状态", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("RoutingRule", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawRoutingRuleSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: false, After: true}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("RoutingRule#%d", op.ID)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw}, cur.Spec.ServerID, dangers, nil

	case "restore":
		cur, err := s.deps.RoutingRules.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, corerouting.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "RoutingRule 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "RoutingRule %d 没有被删除，不需要恢复", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("RoutingRule", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		beforeRaw, _, err := marshalRawRoutingRuleSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: true, After: false}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("RoutingRule#%d", op.ID)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw, RoutingRuleSpec: &cur.Spec}, cur.Spec.ServerID, dangers, nil
	}
	return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "不支持的操作 %q", op.Op)
}

func (s *Service) evalWebsite(ctx context.Context, op Operation, checkVersion bool) (evaluatedItem, int64, []v1.Danger, error) {
	if s.deps.Websites == nil {
		return evaluatedItem{}, 0, nil, v1.New(v1.CodeInternal, "Website 仓储未装配")
	}
	switch op.Op {
	case "create":
		var spec v1.WebsiteSpec
		if err := v1.DecodeSpec(v1.Kind(corewebsites.KindName), op.Spec, &spec); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		norm, err := corewebsites.NormalizeSpec(spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		srv, err := s.getActiveServer(ctx, norm.ServerID)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if err := nginx.ValidateWebsiteSpec(norm, srv.Status.SameHostAsMaster); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if _, err := nginx.RenderWebsite(0, norm, srv.Status.SameHostAsMaster); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if _, err := s.deps.Websites.ByServerDomainIncludeDeleted(ctx, norm.ServerID, norm.Domain); err == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "服务器 %d 下的站点域名 %q 已被占用（含软删除对象）", norm.ServerID, norm.Domain)
		} else if !errors.Is(err, corewebsites.ErrNotFound) {
			return evaluatedItem{}, 0, nil, err
		}
		afterRaw, afterMap, err := marshalRawWebsiteSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{}
		for k, v := range afterMap {
			diff[k] = FieldChange{Before: nil, After: v}
		}
		name := fmt.Sprintf("%d/%s", norm.ServerID, norm.Domain)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, Name: name, ServerID: norm.ServerID, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff), AfterSpec: afterRaw,
		}
		return evaluatedItem{
			Op: op, Planned: p, WebsiteSpec: &norm, TouchesSameHostNginx: srv.Status.SameHostAsMaster,
		}, norm.ServerID, nil, nil

	case "update":
		cur, err := s.deps.Websites.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, corewebsites.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Website 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeConflict, "Website %d 处于软删除状态，不能修改 spec（请先 restore）", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Website", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		var tmp v1.WebsiteSpec
		if err := v1.DecodeSpec(v1.Kind(corewebsites.KindName), op.Spec, &tmp); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		beforeRaw, beforeMap, err := marshalRawWebsiteSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedMap, explicitKeys, err := mergeSpecMap(beforeMap, op.Spec, nil)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		mergedBytes, err := json.Marshal(mergedMap)
		if err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeInternal, "合并 Website spec 失败", err)
		}
		var merged v1.WebsiteSpec
		if err := json.Unmarshal(mergedBytes, &merged); err != nil {
			return evaluatedItem{}, 0, nil, v1.Wrap(v1.CodeBadRequest, "Website.spec 字段类型不合法", err)
		}
		if merged.ServerID != cur.Spec.ServerID {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Website %d 不允许跨服务器修改 server_id", op.ID)
		}
		norm, err := corewebsites.NormalizeSpec(merged)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		srv, err := s.getActiveServer(ctx, norm.ServerID)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if err := nginx.ValidateWebsiteSpec(norm, srv.Status.SameHostAsMaster); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if _, err := nginx.RenderWebsite(op.ID, norm, srv.Status.SameHostAsMaster); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		removedDomain := ""
		if norm.Domain != cur.Spec.Domain {
			if _, err := s.deps.Websites.ByServerDomainIncludeDeleted(ctx, norm.ServerID, norm.Domain); err == nil {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNameTaken, "服务器 %d 下的站点域名 %q 已被占用", norm.ServerID, norm.Domain)
			} else if !errors.Is(err, corewebsites.ErrNotFound) {
				return evaluatedItem{}, 0, nil, err
			}
			removedDomain = cur.Spec.Domain
		}
		afterRaw, afterMap, err := marshalRawWebsiteSpec(norm)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := computeFieldDiff(beforeMap, afterMap, explicitKeys)
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Domain)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, Spec: op.Spec,
			FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: afterRaw,
		}
		return evaluatedItem{
			Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw,
			WebsiteSpec: &norm, WebsiteRemovedDomain: removedDomain, TouchesSameHostNginx: srv.Status.SameHostAsMaster,
		}, cur.Spec.ServerID, nil, nil

	case "delete":
		cur, err := s.deps.Websites.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, corewebsites.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Website 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt != nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Website %d 已经是软删除状态", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Website", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		srv, err := s.deps.Servers.ByIDIncludeDeleted(ctx, cur.Spec.ServerID)
		if err != nil && !errors.Is(err, coreservers.ErrNotFound) {
			return evaluatedItem{}, 0, nil, err
		}
		sameHost := srv != nil && srv.Status.SameHostAsMaster
		beforeRaw, _, err := marshalRawWebsiteSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: false, After: true}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Domain)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, Dangers: dangers,
		}
		return evaluatedItem{
			Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw,
			WebsiteRemovedDomain: cur.Spec.Domain, TouchesSameHostNginx: sameHost,
		}, cur.Spec.ServerID, dangers, nil

	case "restore":
		cur, err := s.deps.Websites.ByIDIncludeDeleted(ctx, op.ID)
		if err != nil {
			if errors.Is(err, corewebsites.ErrNotFound) {
				return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeNotFound, "Website 里没有 id 为 %d 的对象", op.ID)
			}
			return evaluatedItem{}, 0, nil, err
		}
		if cur.Metadata.DeletedAt == nil {
			return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "Website %d 没有被删除，不需要恢复", op.ID)
		}
		if checkVersion && cur.Metadata.ResourceVersion != op.ResourceVersion {
			return evaluatedItem{}, 0, nil, versionConflictErr("Website", op.ResourceVersion, cur.Metadata.ResourceVersion)
		}
		srv, err := s.getActiveServer(ctx, cur.Spec.ServerID)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		if err := nginx.ValidateWebsiteSpec(cur.Spec, srv.Status.SameHostAsMaster); err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		beforeRaw, _, err := marshalRawWebsiteSpec(cur.Spec)
		if err != nil {
			return evaluatedItem{}, 0, nil, err
		}
		diff := map[string]FieldChange{"deleted": {Before: true, After: false}}
		dangers := []v1.Danger{v1.DangerDelete}
		name := fmt.Sprintf("%d/%s", cur.Spec.ServerID, cur.Spec.Domain)
		p := PlannedObject{
			Op: op.Op, Kind: op.Kind, ID: op.ID, Name: name, ServerID: cur.Spec.ServerID,
			ExpectedVersion: op.ResourceVersion, FieldDiff: diff, FieldDiffHash: hashFieldDiff(diff),
			BeforeSpec: beforeRaw, AfterSpec: beforeRaw, Dangers: dangers,
		}
		specCopy := cur.Spec
		return evaluatedItem{
			Op: op, Planned: p, CurrentVersion: cur.Metadata.ResourceVersion, CurrentBefore: beforeRaw,
			WebsiteSpec: &specCopy, TouchesSameHostNginx: srv.Status.SameHostAsMaster,
		}, cur.Spec.ServerID, dangers, nil
	}
	return evaluatedItem{}, 0, nil, v1.Newf(v1.CodeBadRequest, "不支持的操作 %q", op.Op)
}

func (s *Service) ensureActiveServer(ctx context.Context, serverID int64) error {
	_, err := s.getActiveServer(ctx, serverID)
	return err
}

func (s *Service) getActiveServer(ctx context.Context, serverID int64) (*coreservers.Server, error) {
	srv, err := s.deps.Servers.ByIDIncludeDeleted(ctx, serverID)
	if err != nil {
		if errors.Is(err, coreservers.ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "目标服务器 server_id=%d 不存在", serverID)
		}
		return nil, err
	}
	if srv.Metadata.DeletedAt != nil {
		return nil, v1.Newf(v1.CodeConflict, "目标服务器 server_id=%d 处于软删除状态", serverID)
	}
	return srv, nil
}

func (s *Service) simulateServerConfig(ctx context.Context, serverID int64, items []evaluatedItem, kernelAffected, websiteAffected bool) (ServerConfigDiff, error) {
	srv, err := s.deps.Servers.ByIDIncludeDeleted(ctx, serverID)
	if err != nil {
		return ServerConfigDiff{}, err
	}
	ibs, err := s.deps.Inbounds.ListByServer(ctx, serverID)
	if err != nil {
		return ServerConfigDiff{}, err
	}
	obs, err := s.deps.Outbounds.ListByServer(ctx, serverID)
	if err != nil {
		return ServerConfigDiff{}, err
	}
	rrs, err := s.deps.RoutingRules.ListByServer(ctx, serverID)
	if err != nil {
		return ServerConfigDiff{}, err
	}

	beforeIn := make([]v1.InboundSpec, len(ibs))
	ibByID := make(map[int64]v1.InboundSpec, len(ibs))
	for i, x := range ibs {
		beforeIn[i] = x.Spec
		ibByID[x.Metadata.ID] = x.Spec
	}
	beforeOut := make([]v1.OutboundSpec, len(obs))
	obByID := make(map[int64]v1.OutboundSpec, len(obs))
	for i, x := range obs {
		beforeOut[i] = x.Spec
		obByID[x.Metadata.ID] = x.Spec
	}
	beforeRR := make([]v1.RoutingRuleSpec, len(rrs))
	rrByID := make(map[int64]v1.RoutingRuleSpec, len(rrs))
	for i, x := range rrs {
		beforeRR[i] = x.Spec
		rrByID[x.Metadata.ID] = x.Spec
	}

	beforeRendered, err := singbox.Render(singbox.ServerInput{
		Server:       srv.Spec,
		Inbounds:     beforeIn,
		Outbounds:    beforeOut,
		RoutingRules: beforeRR,
	})
	if err != nil {
		return ServerConfigDiff{}, err
	}

	afterServer := srv.Spec
	var extraIn []v1.InboundSpec
	var extraOut []v1.OutboundSpec
	var extraRR []v1.RoutingRuleSpec

	for _, it := range items {
		switch it.Op.Kind {
		case coreservers.KindName:
			if it.Op.ID == serverID && it.ServerSpec != nil {
				afterServer = *it.ServerSpec
			}
		case coreinbounds.KindName:
			if it.Planned.ServerID != serverID {
				continue
			}
			switch it.Op.Op {
			case "create":
				extraIn = append(extraIn, *it.InboundSpec)
			case "update", "restore":
				ibByID[it.Op.ID] = *it.InboundSpec
			case "delete":
				delete(ibByID, it.Op.ID)
			}
		case coreoutbounds.KindName:
			if it.Planned.ServerID != serverID {
				continue
			}
			switch it.Op.Op {
			case "create":
				extraOut = append(extraOut, *it.OutboundSpec)
			case "update", "restore":
				obByID[it.Op.ID] = *it.OutboundSpec
			case "delete":
				delete(obByID, it.Op.ID)
			}
		case corerouting.KindName:
			if it.Planned.ServerID != serverID {
				continue
			}
			switch it.Op.Op {
			case "create":
				extraRR = append(extraRR, *it.RoutingRuleSpec)
			case "update", "restore":
				rrByID[it.Op.ID] = *it.RoutingRuleSpec
			case "delete":
				delete(rrByID, it.Op.ID)
			}
		}
	}

	afterIn := make([]v1.InboundSpec, 0, len(ibByID)+len(extraIn))
	for _, x := range ibByID {
		afterIn = append(afterIn, x)
	}
	afterIn = append(afterIn, extraIn...)

	afterOut := make([]v1.OutboundSpec, 0, len(obByID)+len(extraOut))
	for _, x := range obByID {
		afterOut = append(afterOut, x)
	}
	afterOut = append(afterOut, extraOut...)

	afterRR := make([]v1.RoutingRuleSpec, 0, len(rrByID)+len(extraRR))
	for _, x := range rrByID {
		afterRR = append(afterRR, x)
	}
	afterRR = append(afterRR, extraRR...)

	afterRendered, err := singbox.Render(singbox.ServerInput{
		Server:       afterServer,
		Inbounds:     afterIn,
		Outbounds:    afterOut,
		RoutingRules: afterRR,
	})
	if err != nil {
		return ServerConfigDiff{}, err
	}

	var websiteDiffs []WebsiteConfigDiff
	if websiteAffected && s.deps.Websites != nil {
		ws, err := s.deps.Websites.ListByServer(ctx, serverID)
		if err != nil {
			return ServerConfigDiff{}, err
		}
		byID := make(map[int64]v1.WebsiteSpec, len(ws))
		for _, w := range ws {
			byID[w.Metadata.ID] = w.Spec
		}
		var extraWS []v1.WebsiteSpec
		removedSet := map[string]bool{}
		for _, it := range items {
			if it.Op.Kind != corewebsites.KindName || it.Planned.ServerID != serverID {
				continue
			}
			switch it.Op.Op {
			case "create":
				if it.WebsiteSpec != nil {
					extraWS = append(extraWS, *it.WebsiteSpec)
				}
			case "update", "restore":
				if it.WebsiteSpec != nil {
					byID[it.Op.ID] = *it.WebsiteSpec
				}
				if it.WebsiteRemovedDomain != "" {
					removedSet[it.WebsiteRemovedDomain] = true
				}
			case "delete":
				delete(byID, it.Op.ID)
				if it.WebsiteRemovedDomain != "" {
					removedSet[it.WebsiteRemovedDomain] = true
				}
			}
		}
		type wsEntry struct {
			id   int64
			spec v1.WebsiteSpec
		}
		entries := make([]wsEntry, 0, len(byID)+len(extraWS))
		activeDomains := map[string]bool{}
		for id, sp := range byID {
			entries = append(entries, wsEntry{id: id, spec: sp})
			activeDomains[sp.Domain] = true
		}
		for _, sp := range extraWS {
			entries = append(entries, wsEntry{id: 0, spec: sp})
			activeDomains[sp.Domain] = true
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].spec.Domain != entries[j].spec.Domain {
				return entries[i].spec.Domain < entries[j].spec.Domain
			}
			return entries[i].id < entries[j].id
		})
		for _, e := range entries {
			rs, err := nginx.RenderWebsite(e.id, e.spec, srv.Status.SameHostAsMaster)
			if err != nil {
				return ServerConfigDiff{}, err
			}
			websiteDiffs = append(websiteDiffs, WebsiteConfigDiff{
				Domain:   rs.Domain,
				Action:   "apply",
				ConfPath: rs.ConfPath,
				Conf:     rs.Conf,
			})
		}
		removedDomains := make([]string, 0, len(removedSet))
		for d := range removedSet {
			if !activeDomains[d] {
				removedDomains = append(removedDomains, d)
			}
		}
		sort.Strings(removedDomains)
		for _, d := range removedDomains {
			websiteDiffs = append(websiteDiffs, WebsiteConfigDiff{
				Domain:   d,
				Action:   "remove",
				ConfPath: nginx.ManagedConfPath(d),
			})
		}
	}

	configChanged := false
	if kernelAffected {
		configChanged = srv.Status.AppliedHash != afterRendered.Hash || beforeRendered.Hash != afterRendered.Hash
	}

	return ServerConfigDiff{
		ServerID:            serverID,
		ServerName:          afterServer.Name,
		ExpectedCurrentHash: srv.Status.AppliedHash,
		CurrentRenderedHash: beforeRendered.Hash,
		NextHash:            afterRendered.Hash,
		ConfigChanged:       configChanged,
		FirstApplyEligible:  srv.Status.FirstApplyEligible,
		Inbounds:            afterRendered.Inbounds,
		BeforeConfig:        string(beforeRendered.Pretty),
		AfterConfig:         string(afterRendered.Pretty),
		WebsiteChanged:      websiteAffected,
		Websites:            websiteDiffs,
	}, nil
}

func (s *Service) computeDangers(ctx context.Context, id v1.Identity, ev *evalResult, force bool, now time.Time) ([]v1.Danger, string, *BatchWindowInfo, error) {
	dangerSet := map[v1.Danger]bool{}
	for _, d := range ev.BaseDangers {
		dangerSet[d] = true
	}
	if force {
		dangerSet[v1.DangerPermission] = true
	}
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
		used, err = s.deps.Batch.WindowUsed(ctx, id, since)
		if err != nil {
			return nil, "", nil, err
		}
	}
	if ev.AffectedCount > int64(th.Single) || used+ev.AffectedCount > int64(th.Window) {
		dangerSet[v1.DangerBatch] = true
	}
	dangers := orderedDangers(dangerSet)
	confirmHint := ""
	if len(dangers) > 0 {
		onlyBatch := len(dangers) == 1 && dangers[0] == v1.DangerBatch
		if onlyBatch || ev.AffectedCount > 1 {
			confirmHint = strconv.FormatInt(ev.AffectedCount, 10)
		} else if len(ev.Items) == 1 {
			confirmHint = ev.Items[0].Planned.Name
		} else {
			confirmHint = strconv.FormatInt(ev.AffectedCount, 10)
		}
	}
	bw := &BatchWindowInfo{
		Used:            used,
		Affected:        ev.AffectedCount,
		SingleThreshold: th.Single,
		WindowThreshold: th.Window,
		WindowMinutes:   th.WindowMinutes,
	}
	return dangers, confirmHint, bw, nil
}

func (s *Service) plansList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	status := strings.TrimSpace(inv.String("status", ""))
	switch status {
	case "", coreplans.StatusPending, coreplans.StatusApplied, coreplans.StatusExpired, coreplans.StatusRejected:
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "不支持的状态过滤 %q（支持 pending、applied、expired、rejected）", status)
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
	now := s.now()
	total, err := s.deps.Plans.Count(ctx, status, now)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.Plans.List(ctx, status, now, beforeID, page.Limit+1)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].Metadata.ID)
	}
	for _, r := range rows {
		res.Items = append(res.Items, PlanSummary{
			ID:            r.Metadata.ID,
			Status:        coreplans.EffectiveStatus(r, now),
			AffectedCount: r.Spec.AffectedCount,
			ApplyID:       r.Status.ApplyID,
			ExpiresAt:     r.Spec.ExpiresAt,
			CreatedAt:     r.Metadata.CreatedAt,
		})
	}
	return res, nil
}

func (s *Service) plansGet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(inv.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "计划 id 必须是正整数，得到 %q", inv.Arg(0))
	}
	p, err := s.deps.Plans.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	var diff PlanDiffData
	_ = json.Unmarshal(p.Spec.Diff, &diff)
	return buildPlanView(p, diff, nil, nil, s.now(), v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets)), nil
}

func buildPlanView(p *coreplans.Plan, diff PlanDiffData, bw *BatchWindowInfo, skipped []SkippedObject, now time.Time, revealSecrets bool) *PlanView {
	objs := make([]PlannedObject, len(diff.Objects))
	for i, o := range diff.Objects {
		if !revealSecrets {
			o = redactPlannedObject(o)
		}
		objs[i] = o
	}
	srvs := make([]ServerConfigDiff, len(diff.Servers))
	for i, sv := range diff.Servers {
		if !revealSecrets {
			if sv.BeforeConfig != "" {
				sv.BeforeConfig = string(singbox.RedactJSON([]byte(sv.BeforeConfig), true))
			}
			if sv.AfterConfig != "" {
				sv.AfterConfig = string(singbox.RedactJSON([]byte(sv.AfterConfig), true))
			}
		}
		srvs[i] = sv
	}
	dangers := diff.RequiredDangers
	if dangers == nil {
		dangers = []v1.Danger{}
	}
	batchID := ""
	if diff.BatchMeta != nil {
		batchID = diff.BatchMeta.BatchID
	}
	return &PlanView{
		PlanID:          p.Metadata.ID,
		BatchID:         batchID,
		Status:          coreplans.EffectiveStatus(p, now),
		AffectedCount:   p.Spec.AffectedCount,
		ApplyID:         p.Status.ApplyID,
		ExpiresAt:       p.Spec.ExpiresAt,
		CreatedAt:       p.Metadata.CreatedAt,
		RequiredDangers: dangers,
		ConfirmHint:     diff.ConfirmHint,
		RequiresHuman:   diff.RequiresHuman,
		BatchWindow:     bw,
		Objects:         objs,
		Servers:         srvs,
		Skipped:         skipped,
	}
}

func redactPlannedObject(o PlannedObject) PlannedObject {
	if len(o.Spec) > 0 {
		o.Spec = singbox.RedactJSON(o.Spec, false)
	}
	if len(o.BeforeSpec) > 0 {
		o.BeforeSpec = singbox.RedactJSON(o.BeforeSpec, false)
	}
	if len(o.AfterSpec) > 0 {
		o.AfterSpec = singbox.RedactJSON(o.AfterSpec, false)
	}
	if len(o.FieldDiff) > 0 {
		redacted := make(map[string]FieldChange, len(o.FieldDiff))
		for k, fc := range o.FieldDiff {
			if singbox.IsSensitiveKey(k) || k == "tls" || k == "settings" {
				redacted[k] = FieldChange{Before: singbox.RedactValue(fc.Before), After: singbox.RedactValue(fc.After)}
			} else {
				redacted[k] = fc
			}
		}
		o.FieldDiff = redacted
	}
	return o
}

func orderedDangers(set map[v1.Danger]bool) []v1.Danger {
	out := []v1.Danger{}
	for _, d := range []v1.Danger{v1.DangerDelete, v1.DangerPermission, v1.DangerBatch} {
		if set[d] {
			out = append(out, d)
		}
	}
	return out
}

func versionConflictErr(kind string, expected, current int64) error {
	return v1.Newf(v1.CodeVersionConflict, "%s 已被别人改过：期望 resourceVersion %d，存储中是 %d", kind, expected, current).
		WithState("resourceVersion", current).
		WithNext("重新读取对象并重新执行 satchel plan")
}

// 原始未打码 Spec 序列化辅助函数（用于写前快照、diff 与合并，严禁包含 ServerStatus 三把令牌）。

func marshalRawServerSpec(spec v1.ServerSpec) (json.RawMessage, map[string]any, error) {
	return marshalCanonicalMap(spec)
}

func marshalRawInboundSpec(spec v1.InboundSpec) (json.RawMessage, map[string]any, error) {
	rawObj := struct {
		ServerID  int64           `json:"server_id"`
		Tag       string          `json:"tag"`
		Protocol  string          `json:"protocol"`
		Port      int64           `json:"port"`
		Listen    string          `json:"listen"`
		TLS       json.RawMessage `json:"tls"`
		Transport json.RawMessage `json:"transport"`
		Settings  json.RawMessage `json:"settings"`
		Enabled   bool            `json:"enabled"`
		SortOrder int64           `json:"sort_order"`
	}{
		ServerID: spec.ServerID, Tag: spec.Tag, Protocol: spec.Protocol, Port: spec.Port,
		Listen: spec.Listen, TLS: spec.TLS.Reveal(), Transport: spec.Transport,
		Settings: spec.Settings.Reveal(), Enabled: spec.Enabled, SortOrder: spec.SortOrder,
	}
	return marshalCanonicalMap(rawObj)
}

func marshalRawOutboundSpec(spec v1.OutboundSpec) (json.RawMessage, map[string]any, error) {
	rawObj := struct {
		ServerID      int64           `json:"server_id"`
		Tag           string          `json:"tag"`
		Protocol      string          `json:"protocol"`
		Settings      json.RawMessage `json:"settings"`
		IsWarp        bool            `json:"is_warp"`
		BalancerGroup string          `json:"balancer_group"`
		Probe         json.RawMessage `json:"probe"`
	}{
		ServerID: spec.ServerID, Tag: spec.Tag, Protocol: spec.Protocol,
		Settings: spec.Settings.Reveal(), IsWarp: spec.IsWarp,
		BalancerGroup: spec.BalancerGroup, Probe: spec.Probe,
	}
	return marshalCanonicalMap(rawObj)
}

func marshalRawRoutingRuleSpec(spec v1.RoutingRuleSpec) (json.RawMessage, map[string]any, error) {
	return marshalCanonicalMap(spec)
}

func marshalRawWebsiteSpec(spec v1.WebsiteSpec) (json.RawMessage, map[string]any, error) {
	return marshalCanonicalMap(spec)
}

func marshalCanonicalMap(v any) (json.RawMessage, map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, nil, v1.Wrap(v1.CodeInternal, "序列化 spec 失败", err)
	}
	canon, err := xrpc.CanonicalJSON(raw)
	if err != nil {
		return nil, nil, v1.Wrap(v1.CodeInternal, "规范化 spec 失败", err)
	}
	dec := json.NewDecoder(bytes.NewReader(canon))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, nil, v1.Wrap(v1.CodeInternal, "解析 spec 映射失败", err)
	}
	return canon, m, nil
}

// mergeSpecMap 将 opSpec 中显式给出的字段合并到 beforeMap 上；若打码字段值为 "***" 则保持原值不变。
func mergeSpecMap(beforeMap map[string]any, opSpec json.RawMessage, maskedFields map[string]bool) (map[string]any, map[string]bool, error) {
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(opSpec, &rawFields); err != nil {
		return nil, nil, v1.Wrap(v1.CodeBadRequest, "spec 必须是 JSON 对象", err)
	}
	merged := make(map[string]any, len(beforeMap))
	for k, v := range beforeMap {
		merged[k] = v
	}
	explicit := make(map[string]bool, len(rawFields))
	for k, rawVal := range rawFields {
		trimmed := bytes.TrimSpace(rawVal)
		if maskedFields[k] && bytes.Equal(trimmed, []byte(`"`+v1.Redacted+`"`)) {
			// 打码标记表示保持原值不变，不覆盖、不计入变化。
			continue
		}
		explicit[k] = true
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		var val any
		if err := dec.Decode(&val); err != nil {
			return nil, nil, v1.Wrap(v1.CodeBadRequest, "字段 "+k+" 不是合法的 JSON", err)
		}
		merged[k] = val
	}
	return merged, explicit, nil
}

func computeFieldDiff(beforeMap, afterMap map[string]any, explicitKeys map[string]bool) map[string]FieldChange {
	diff := map[string]FieldChange{}
	keys := make([]string, 0, len(explicitKeys))
	for k := range explicitKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		bRaw, _ := json.Marshal(beforeMap[k])
		aRaw, _ := json.Marshal(afterMap[k])
		bCanon, _ := xrpc.CanonicalJSON(bRaw)
		aCanon, _ := xrpc.CanonicalJSON(aRaw)
		if !bytes.Equal(bCanon, aCanon) {
			diff[k] = FieldChange{Before: beforeMap[k], After: afterMap[k]}
		}
	}
	return diff
}

func hashFieldDiff(diff map[string]FieldChange) string {
	raw, _ := json.Marshal(diff)
	return xrpc.CanonicalConfigHash(raw)
}
