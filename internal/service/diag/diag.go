package diag

import (
	"context"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// DiagResponse 是 server diag <name> [item] 的返回对象。
type DiagResponse struct {
	Server      string                `json:"server"`
	ServerID    int64                 `json:"server_id"`
	CollectedAt time.Time             `json:"collected_at"`
	NodeTimeUTC time.Time             `json:"node_time_utc"`
	Items       []xrpc.DiagItemResult `json:"items"`
}

func (s *Service) serverDiag(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}

	var items []string
	if rawItem := strings.TrimSpace(inv.Arg(1)); rawItem != "" {
		norm := xrpc.NormalizeDiagItem(rawItem)
		if !xrpc.ValidDiagItem(norm) {
			return nil, v1.Newf(v1.CodeBadRequest, "未知诊断项 %q", rawItem).
				WithNext("可选诊断项：" + strings.Join(xrpc.DiagItems, ", "))
		}
		items = []string{norm}
	}

	includeSecrets := v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets)
	now := s.now()
	params := xrpc.DiagRunParams{
		Items:            items,
		MasterTimeUnixMS: now.UnixMilli(),
		MasterURL:        s.loadMasterURL(ctx),
		IncludeSecrets:   includeSecrets,
	}
	res, err := s.deps.Nodes.CallDiagRun(ctx, srv.Metadata.ID, params)
	if err != nil {
		return nil, err
	}

	outItems := make([]xrpc.DiagItemResult, len(res.Items))
	for i, it := range res.Items {
		if it.Name == xrpc.DiagItemLogs || it.Name == xrpc.DiagItemConfig {
			it.ContentType = "data"
			it.UntrustedData = true
		}
		// 纵深防御：调用者无 secrets 权限时，主控再次对 config 项敏感字段与文本执行脱敏。
		if !includeSecrets && it.Name == xrpc.DiagItemConfig && it.Data != nil {
			it.Data = redactAny(it.Data)
		}
		outItems[i] = it
	}

	return &DiagResponse{
		Server:      srv.Spec.Name,
		ServerID:    srv.Metadata.ID,
		CollectedAt: res.CollectedAt,
		NodeTimeUTC: res.NodeTimeUTC,
		Items:       outItems,
	}, nil
}
