package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// OutboundSpecView 是对外输出的出站规格视图（按调用者 secrets scope 决定 settings 是否打码）。
type OutboundSpecView struct {
	ServerID      int64           `json:"server_id"`
	Tag           string          `json:"tag"`
	Protocol      string          `json:"protocol"`
	Settings      json.RawMessage `json:"settings"`
	IsWarp        bool            `json:"is_warp"`
	BalancerGroup string          `json:"balancer_group"`
	Probe         json.RawMessage `json:"probe"`
}

// OutboundView 是 outbound list / outbound get 的完整对象视图。
type OutboundView struct {
	APIVersion string            `json:"apiVersion"`
	Kind       v1.Kind           `json:"kind"`
	Metadata   v1.Metadata       `json:"metadata"`
	Spec       OutboundSpecView  `json:"spec"`
	Status     v1.OutboundStatus `json:"status"`
	ServerName string            `json:"server_name,omitempty"`
}

func makeOutboundView(ob *coreoutbounds.Outbound, serverName string, revealSecrets bool) OutboundView {
	return OutboundView{
		APIVersion: ob.APIVersion,
		Kind:       ob.Kind,
		Metadata:   ob.Metadata,
		Spec: OutboundSpecView{
			ServerID:      ob.Spec.ServerID,
			Tag:           ob.Spec.Tag,
			Protocol:      ob.Spec.Protocol,
			Settings:      maskOrRevealJSON(ob.Spec.Settings.Reveal(), revealSecrets),
			IsWarp:        ob.Spec.IsWarp,
			BalancerGroup: ob.Spec.BalancerGroup,
			Probe:         nonEmptyJSONObj(ob.Spec.Probe),
		},
		Status:     ob.Status,
		ServerName: serverName,
	}
}

func (s *Service) outboundList(ctx context.Context, inv *command.Invocation) (any, error) {
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
	rows, total, err := s.deps.Outbounds.List(ctx, coreoutbounds.ListFilter{
		ServerID:       srvID,
		BalancerGroup:  inv.String("balancer-group", ""),
		IncludeDeleted: inv.Bool("include-deleted"),
	}, limit, offset)
	if err != nil {
		return nil, err
	}
	nameByID := s.serverNameMap(ctx)
	reveal := v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets)
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, makeOutboundView(r, nameByID[r.Spec.ServerID], reveal))
	}
	return &command.PageResult{
		Items:      items,
		Total:      total,
		NextCursor: nextOffsetCursor(offset, len(rows), total),
	}, nil
}

func (s *Service) outboundGet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := parsePositiveID(inv.Arg(0), "Outbound")
	if err != nil {
		return nil, err
	}
	ob, err := s.deps.Outbounds.ByID(ctx, id)
	if errors.Is(err, coreoutbounds.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "Outbound 里没有 id 为 %d 的对象", id)
	}
	if err != nil {
		return nil, err
	}
	name := ""
	if srv, err := s.deps.Servers.ByID(ctx, ob.Spec.ServerID); err == nil {
		name = srv.Spec.Name
	}
	return makeOutboundView(ob, name, v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets)), nil
}

// OutboundProbeView 是 outbound probe 的输出。
type OutboundProbeView struct {
	ID         int64     `json:"id"`
	Server     string    `json:"server"`
	Tag        string    `json:"tag"`
	URL        string    `json:"url"`
	OK         bool      `json:"ok"`
	LatencyMS  int64     `json:"latency_ms"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	ProbedAt   time.Time `json:"probed_at"`
}

func (s *Service) outboundProbe(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := parsePositiveID(inv.Arg(0), "Outbound")
	if err != nil {
		return nil, err
	}
	ob, err := s.deps.Outbounds.ByID(ctx, id)
	if errors.Is(err, coreoutbounds.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "Outbound 里没有 id 为 %d 的对象", id)
	}
	if err != nil {
		return nil, err
	}
	srv, err := s.deps.Servers.ByID(ctx, ob.Spec.ServerID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "出站所属服务器 id=%d 不存在", ob.Spec.ServerID)
	}
	if s.deps.Nodes == nil {
		return nil, v1.New(v1.CodeUnavailable, "节点通道未装配")
	}
	probeURL := strings.TrimSpace(inv.String("url", ""))
	if probeURL == "" {
		// 若出站 probe 配置中指定了 url 则优先复用，否则使用默认探测地址。
		var pCfg map[string]any
		if json.Unmarshal(ob.Spec.Probe, &pCfg) == nil {
			if u, ok := pCfg["url"].(string); ok && strings.TrimSpace(u) != "" {
				probeURL = strings.TrimSpace(u)
			}
		}
	}
	if probeURL == "" {
		probeURL = xrpc.DefaultOutboundProbeURL
	}
	if !strings.HasPrefix(probeURL, "http://") && !strings.HasPrefix(probeURL, "https://") {
		return nil, v1.Newf(v1.CodeBadRequest, "--url 必须是 http:// 或 https:// 地址，得到 %q", probeURL)
	}
	timeout := inv.Duration("timeout", 10*time.Second)
	if timeout < time.Second {
		timeout = time.Second
	}
	if timeout > 30*time.Second {
		return nil, v1.New(v1.CodeBadRequest, "--timeout 不能超过 30s")
	}

	res, err := s.deps.Nodes.CallOutboundProbe(ctx, srv.Metadata.ID, xrpc.OutboundProbeParams{
		Tag:       ob.Spec.Tag,
		URL:       probeURL,
		TimeoutMS: int(timeout / time.Millisecond),
	})
	if err != nil {
		return nil, err
	}
	probedAt := res.ProbedAt
	if probedAt.IsZero() {
		probedAt = s.now()
	}
	var latPtr *int64
	if res.OK || res.LatencyMS > 0 {
		v := res.LatencyMS
		latPtr = &v
	}
	if err := s.deps.Outbounds.UpdateProbeStatus(ctx, ob.Metadata.ID, res.OK, latPtr, probedAt); err != nil {
		return nil, err
	}
	return &OutboundProbeView{
		ID:         ob.Metadata.ID,
		Server:     srv.Spec.Name,
		Tag:        ob.Spec.Tag,
		URL:        res.URL,
		OK:         res.OK,
		LatencyMS:  res.LatencyMS,
		StatusCode: res.StatusCode,
		Error:      res.Error,
		ProbedAt:   probedAt,
	}, nil
}

func (s *Service) outboundLandingPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}
	srcSrv, err := s.resolveServer(ctx, inv.String("server-name", ""))
	if err != nil {
		return nil, err
	}
	outTag := strings.TrimSpace(inv.String("outbound-tag", ""))
	if outTag == "" {
		return nil, v1.New(v1.CodeBadRequest, "必须指定 --outbound-tag")
	}

	targetSrvRef := strings.TrimSpace(inv.String("target-server", ""))
	targetInTag := strings.TrimSpace(inv.String("target-inbound", ""))
	manualProto := strings.TrimSpace(inv.String("protocol", ""))
	manualSettingsRaw := strings.TrimSpace(inv.String("settings", ""))

	var outProto string
	var outSettings map[string]any

	if targetSrvRef != "" || targetInTag != "" {
		if targetSrvRef == "" || targetInTag == "" {
			return nil, v1.New(v1.CodeBadRequest, "引用落地服务器时必须同时提供 --target-server 与 --target-inbound")
		}
		if manualProto != "" || manualSettingsRaw != "" {
			return nil, v1.New(v1.CodeBadRequest, "--target-server/--target-inbound 与手工 --protocol/--settings 不能同时使用")
		}
		dstSrv, err := s.resolveServer(ctx, targetSrvRef)
		if err != nil {
			return nil, err
		}
		dstIn, err := s.deps.Inbounds.ByServerTag(ctx, dstSrv.Metadata.ID, targetInTag)
		if errors.Is(err, coreinbounds.ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "落地服务器 %s 上没有入站 %q", dstSrv.Spec.Name, targetInTag)
		}
		if err != nil {
			return nil, err
		}
		outProto, outSettings, err = deriveOutboundFromInbound(dstSrv, dstIn)
		if err != nil {
			return nil, err
		}
	} else {
		if manualProto == "" {
			return nil, v1.New(v1.CodeBadRequest, "必须指定 --protocol（或改用 --target-server + --target-inbound 自动派生）")
		}
		outProto = strings.ToLower(manualProto)
		outSettings = map[string]any{}
		if manualSettingsRaw != "" {
			if err := json.Unmarshal([]byte(manualSettingsRaw), &outSettings); err != nil || outSettings == nil {
				return nil, v1.New(v1.CodeBadRequest, "--settings 必须是合法的 JSON 对象")
			}
		}
	}

	if detour := strings.TrimSpace(inv.String("detour", "")); detour != "" {
		outSettings["detour"] = detour
	}

	inboundTag := strings.TrimSpace(inv.String("inbound-tag", ""))
	sortOrder := inv.Int("sort-order", 100)
	ruleSpec := map[string]any{
		"server_id":    srcSrv.Metadata.ID,
		"sort_order":   sortOrder,
		"outbound_tag": outTag,
		"enabled":      true,
	}
	if inboundTag != "" {
		ruleSpec["match"] = map[string]any{"inbound": []string{inboundTag}}
		ruleSpec["catch_all"] = false
	} else {
		ruleSpec["match"] = map[string]any{}
		ruleSpec["catch_all"] = true
	}

	ops := []map[string]any{
		{
			"op":   "create",
			"kind": coreoutbounds.KindName,
			"spec": map[string]any{
				"server_id":      srcSrv.Metadata.ID,
				"tag":            outTag,
				"protocol":       outProto,
				"settings":       outSettings,
				"balancer_group": strings.TrimSpace(inv.String("balancer-group", "")),
			},
		},
		{
			"op":   "create",
			"kind": corerouting.KindName,
			"spec": ruleSpec,
		},
	}
	opsJSON, err := json.Marshal(ops)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码落地节点计划失败", err)
	}
	return s.deps.Planner.CreatePlanFromJSON(ctx, string(opsJSON), "", "")
}

func (s *Service) outboundBatchList(ctx context.Context, inv *command.Invocation) (any, error) {
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
	rows, total, err := s.deps.BatchOutbounds.List(ctx, inv.String("batch-id", ""), srvID, limit, offset)
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

func (s *Service) outboundBatchPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}
	targets, err := s.resolveTargetServers(ctx, stringSliceFlag(inv, "servers"), inv.Bool("all"))
	if err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(inv.String("tag", ""))
	proto := strings.ToLower(strings.TrimSpace(inv.String("protocol", "")))
	if tag == "" || proto == "" {
		return nil, v1.New(v1.CodeBadRequest, "必须同时提供 --tag 与 --protocol")
	}
	settingsObj := map[string]any{}
	if raw := strings.TrimSpace(inv.String("settings", "")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &settingsObj); err != nil || settingsObj == nil {
			return nil, v1.New(v1.CodeBadRequest, "--settings 必须是合法的 JSON 对象")
		}
	}
	probeObj := map[string]any{}
	if raw := strings.TrimSpace(inv.String("probe", "")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &probeObj); err != nil || probeObj == nil {
			return nil, v1.New(v1.CodeBadRequest, "--probe 必须是合法的 JSON 对象")
		}
	}
	bg := strings.TrimSpace(inv.String("balancer-group", ""))

	ops := make([]map[string]any, 0, len(targets))
	for _, srv := range targets {
		ops = append(ops, map[string]any{
			"op":   "create",
			"kind": coreoutbounds.KindName,
			"spec": map[string]any{
				"server_id":      srv.Metadata.ID,
				"tag":            tag,
				"protocol":       proto,
				"settings":       settingsObj,
				"balancer_group": bg,
				"probe":          probeObj,
			},
		})
	}
	opsJSON, err := json.Marshal(ops)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码批量出站计划失败", err)
	}
	return s.deps.Planner.CreatePlanFromJSON(ctx, string(opsJSON), coreoutbounds.KindName, newBatchID())
}

// deriveOutboundFromInbound 根据另一台 Satchel 服务器上的目标入站规格自动派生客户端出站配置。
func deriveOutboundFromInbound(dstSrv *coreservers.Server, ib *coreinbounds.Inbound) (string, map[string]any, error) {
	host := firstNonEmpty(dstSrv.Spec.Domain, derefStr(dstSrv.Status.IPAddress), dstSrv.Spec.DomainV6, dstSrv.Status.IPAddressV6)
	if host == "" {
		return "", nil, v1.Newf(v1.CodeBadRequest, "落地服务器 %s 尚未配置域名或记录到公网 IP，请先配置 domain 或改用手工 --protocol/--settings", dstSrv.Spec.Name)
	}
	proto := strings.ToLower(strings.TrimSpace(ib.Spec.Protocol))
	var inSet, inTLS, inTr map[string]any
	_ = json.Unmarshal(ib.Spec.Settings.Reveal(), &inSet)
	_ = json.Unmarshal(ib.Spec.TLS.Reveal(), &inTLS)
	_ = json.Unmarshal(ib.Spec.Transport, &inTr)

	out := map[string]any{
		"server":      host,
		"server_port": ib.Spec.Port,
	}
	firstUser := func() map[string]any {
		users, _ := inSet["users"].([]any)
		if len(users) == 0 {
			return nil
		}
		u, _ := users[0].(map[string]any)
		return u
	}

	switch proto {
	case "vless":
		u := firstUser()
		if u == nil || stringField(u["uuid"]) == "" {
			return "", nil, v1.Newf(v1.CodeBadRequest, "目标入站 %s 缺少用户 uuid，无法自动派生出站", ib.Spec.Tag)
		}
		out["uuid"] = stringField(u["uuid"])
		if flow := stringField(u["flow"]); flow != "" {
			out["flow"] = flow
		}
	case "vmess":
		u := firstUser()
		if u == nil || stringField(u["uuid"]) == "" {
			return "", nil, v1.Newf(v1.CodeBadRequest, "目标入站 %s 缺少用户 uuid，无法自动派生出站", ib.Spec.Tag)
		}
		out["uuid"] = stringField(u["uuid"])
		out["security"] = "auto"
	case "trojan", "hysteria2", "anytls":
		u := firstUser()
		if u == nil || stringField(u["password"]) == "" {
			return "", nil, v1.Newf(v1.CodeBadRequest, "目标入站 %s 缺少用户 password，无法自动派生出站", ib.Spec.Tag)
		}
		out["password"] = stringField(u["password"])
	case "tuic":
		u := firstUser()
		if u == nil || stringField(u["uuid"]) == "" || stringField(u["password"]) == "" {
			return "", nil, v1.Newf(v1.CodeBadRequest, "目标入站 %s 缺少用户 uuid/password，无法自动派生出站", ib.Spec.Tag)
		}
		out["uuid"] = stringField(u["uuid"])
		out["password"] = stringField(u["password"])
		out["congestion_control"] = "bbr"
	case "shadowsocks":
		method := stringField(inSet["method"])
		pw := stringField(inSet["password"])
		if method == "" || pw == "" {
			return "", nil, v1.Newf(v1.CodeBadRequest, "目标入站 %s 缺少 Shadowsocks method/password", ib.Spec.Tag)
		}
		if strings.HasPrefix(method, "2022-") {
			if u := firstUser(); u != nil && stringField(u["password"]) != "" {
				pw = pw + ":" + stringField(u["password"])
			}
		}
		out["method"] = method
		out["password"] = pw
	case "socks", "http":
		if u := firstUser(); u != nil {
			if un := stringField(u["username"]); un != "" {
				out["username"] = un
			}
			if pw := stringField(u["password"]); pw != "" {
				out["password"] = pw
			}
		}
	default:
		return "", nil, v1.Newf(v1.CodeBadRequest, "目标入站协议 %q 暂不支持自动派生客户端出站，请改用 --protocol + --settings 手工配置", proto)
	}

	// 派生客户端 TLS / Reality 配置
	if en, _ := inTLS["enabled"].(bool); en {
		clientTLS := map[string]any{"enabled": true}
		if sni := stringField(inTLS["server_name"]); sni != "" {
			clientTLS["server_name"] = sni
		} else {
			clientTLS["server_name"] = host
		}
		if realMap, ok := inTLS["reality"].(map[string]any); ok {
			if ren, _ := realMap["enabled"].(bool); ren {
				pub, err := singbox.DeriveRealityPublicKey(stringField(realMap["private_key"]))
				if err != nil {
					return "", nil, v1.Wrap(v1.CodeBadRequest, "派生目标入站 Reality 公钥失败", err)
				}
				clientReal := map[string]any{"enabled": true, "public_key": pub}
				if sids, ok := realMap["short_id"].([]any); ok && len(sids) > 0 {
					if sid := stringField(sids[0]); sid != "" {
						clientReal["short_id"] = sid
					}
				} else if sid := stringField(realMap["short_id"]); sid != "" {
					clientReal["short_id"] = sid
				}
				clientTLS["reality"] = clientReal
				clientTLS["utls"] = map[string]any{"enabled": true, "fingerprint": "chrome"}
			}
		}
		out["tls"] = clientTLS
	}

	if len(inTr) > 0 {
		out["transport"] = inTr
	}
	return proto, out, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

func stringField(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
