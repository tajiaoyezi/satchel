package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	corepresets "github.com/satchel/satchel/internal/core/routingpresets"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// RoutingRuleView 是 routing list / routing get 的输出视图（附带全兜底与遮蔽告警提示）。
type RoutingRuleView struct {
	APIVersion        string             `json:"apiVersion"`
	Kind              v1.Kind            `json:"kind"`
	Metadata          v1.Metadata        `json:"metadata"`
	Spec              v1.RoutingRuleSpec `json:"spec"`
	ServerName        string             `json:"server_name,omitempty"`
	IsInboundCatchAll bool               `json:"is_inbound_catch_all"`
	ShadowedBy        []int64            `json:"shadowed_by,omitempty"`
}

func (s *Service) routingList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	limit, offset, err := pageLimitOffset(inv)
	if err != nil {
		return nil, err
	}
	var srvID int64
	if ref := strings.TrimSpace(inv.String("server-name", "")); ref != "" {
		srv, err := s.resolveServer(ctx, ref)
		if err != nil {
			return nil, err
		}
		srvID = srv.Metadata.ID
	}
	rows, total, err := s.deps.RoutingRules.List(ctx, corerouting.ListFilter{
		ServerID:       srvID,
		IncludeDeleted: inv.Bool("include-deleted"),
	}, limit, offset)
	if err != nil {
		return nil, err
	}

	// 按服务器分组计算全量规则（跨分页不漏算前序兜底遮蔽）。
	hintsByServer := map[int64]map[int64]singbox.RoutingRuleHint{}
	for _, r := range rows {
		if _, done := hintsByServer[r.Spec.ServerID]; done {
			continue
		}
		allForSrv, err := s.deps.RoutingRules.ListByServer(ctx, r.Spec.ServerID)
		if err != nil {
			return nil, err
		}
		inputs := make([]singbox.RoutingRuleInput, len(allForSrv))
		for i, x := range allForSrv {
			inputs[i] = singbox.RoutingRuleInput{
				ID:       x.Metadata.ID,
				Match:    x.Spec.Match,
				Enabled:  x.Spec.Enabled,
				CatchAll: x.Spec.CatchAll,
			}
		}
		hintsByServer[r.Spec.ServerID] = singbox.AnalyzeRoutingRules(inputs)
	}

	nameByID := s.serverNameMap(ctx)
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		hint := hintsByServer[r.Spec.ServerID][r.Metadata.ID]
		items = append(items, RoutingRuleView{
			APIVersion:        r.APIVersion,
			Kind:              r.Kind,
			Metadata:          r.Metadata,
			Spec:              r.Spec,
			ServerName:        nameByID[r.Spec.ServerID],
			IsInboundCatchAll: hint.IsInboundCatchAll,
			ShadowedBy:        hint.ShadowedBy,
		})
	}
	return &command.PageResult{
		Items:      items,
		Total:      total,
		NextCursor: nextOffsetCursor(offset, len(rows), total),
	}, nil
}

func (s *Service) routingGet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := parsePositiveID(inv.Arg(0), "RoutingRule")
	if err != nil {
		return nil, err
	}
	rr, err := s.deps.RoutingRules.ByID(ctx, id)
	if errors.Is(err, corerouting.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "RoutingRule 里没有 id 为 %d 的对象", id)
	}
	if err != nil {
		return nil, err
	}
	name := ""
	if srv, err := s.deps.Servers.ByID(ctx, rr.Spec.ServerID); err == nil {
		name = srv.Spec.Name
	}
	var hint singbox.RoutingRuleHint
	if allForSrv, err := s.deps.RoutingRules.ListByServer(ctx, rr.Spec.ServerID); err == nil {
		inputs := make([]singbox.RoutingRuleInput, len(allForSrv))
		for i, x := range allForSrv {
			inputs[i] = singbox.RoutingRuleInput{ID: x.Metadata.ID, Match: x.Spec.Match, Enabled: x.Spec.Enabled, CatchAll: x.Spec.CatchAll}
		}
		hint = singbox.AnalyzeRoutingRules(inputs)[rr.Metadata.ID]
	}
	return RoutingRuleView{
		APIVersion:        rr.APIVersion,
		Kind:              rr.Kind,
		Metadata:          rr.Metadata,
		Spec:              rr.Spec,
		ServerName:        name,
		IsInboundCatchAll: hint.IsInboundCatchAll,
		ShadowedBy:        hint.ShadowedBy,
	}, nil
}

func (s *Service) routingPresetBuiltin(ctx context.Context, _ *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	items := singbox.BuiltinRoutingPresets()
	return map[string]any{
		"items": items,
		"total": len(items),
	}, nil
}

func (s *Service) routingPresetList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	limit, offset, err := pageLimitOffset(inv)
	if err != nil {
		return nil, err
	}
	ownerFilter := strings.TrimSpace(inv.String("owner", ""))
	rows, total, err := s.deps.RoutingPresets.List(ctx, ownerFilter, limit, offset)
	if err != nil {
		return nil, err
	}
	items := make([]any, len(rows))
	for i := range rows {
		items[i] = rows[i]
	}
	return &command.PageResult{
		Items:      items,
		Total:      total,
		NextCursor: nextOffsetCursor(offset, len(rows), total),
	}, nil
}

func (s *Service) routingPresetSave(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(inv.String("name", ""))
	ruleRaw := strings.TrimSpace(inv.String("rule", ""))
	if name == "" {
		return nil, v1.New(v1.CodeBadRequest, "必须提供 --name")
	}
	if ruleRaw == "" {
		return nil, v1.New(v1.CodeBadRequest, "必须提供 --rule JSON 对象")
	}
	if err := singbox.ValidateRoutingMatch(json.RawMessage(ruleRaw)); err != nil {
		return nil, err
	}
	owner, err := s.resolvePresetOwner(ctx, inv.String("owner", ""))
	if err != nil {
		return nil, err
	}
	return s.deps.RoutingPresets.Save(ctx, owner, name, json.RawMessage(ruleRaw))
}

func (s *Service) routingPresetDelete(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := parsePositiveID(inv.Arg(0), "routing preset")
	if err != nil {
		return nil, err
	}
	if err := s.deps.RoutingPresets.Delete(ctx, id); errors.Is(err, corepresets.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "没有 id 为 %d 的自定义路由预设", id)
	} else if err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "id": id}, nil
}
