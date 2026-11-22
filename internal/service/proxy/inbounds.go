package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// InboundSpecView 是对外输出的入站规格视图（按调用者 secrets scope 决定 tls/settings 是否打码）。
type InboundSpecView struct {
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
}

// InboundDerived 是查询入站时实时派生的非敏感客户端连接元数据（不写入 kernel.json）。
type InboundDerived struct {
	RealityPublicKey string `json:"reality_public_key,omitempty"`
}

// InboundView 是 inbound list / inbound get 的完整对象视图。
type InboundView struct {
	APIVersion string          `json:"apiVersion"`
	Kind       v1.Kind         `json:"kind"`
	Metadata   v1.Metadata     `json:"metadata"`
	Spec       InboundSpecView `json:"spec"`
	Derived    *InboundDerived `json:"derived,omitempty"`
	ServerName string          `json:"server_name,omitempty"`
}

func makeInboundView(ib *coreinbounds.Inbound, serverName string, revealSecrets bool) InboundView {
	tlsRaw := ib.Spec.TLS.Reveal()
	setRaw := ib.Spec.Settings.Reveal()
	var derived *InboundDerived
	if pub := deriveRealityPubFromTLS(tlsRaw); pub != "" {
		derived = &InboundDerived{RealityPublicKey: pub}
	}
	return InboundView{
		APIVersion: ib.APIVersion,
		Kind:       ib.Kind,
		Metadata:   ib.Metadata,
		Spec: InboundSpecView{
			ServerID:  ib.Spec.ServerID,
			Tag:       ib.Spec.Tag,
			Protocol:  ib.Spec.Protocol,
			Port:      ib.Spec.Port,
			Listen:    ib.Spec.Listen,
			TLS:       maskOrRevealJSON(tlsRaw, revealSecrets),
			Transport: nonEmptyJSONObj(ib.Spec.Transport),
			Settings:  maskOrRevealJSON(setRaw, revealSecrets),
			Enabled:   ib.Spec.Enabled,
			SortOrder: ib.Spec.SortOrder,
		},
		Derived:    derived,
		ServerName: serverName,
	}
}

func maskOrRevealJSON(raw json.RawMessage, reveal bool) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return json.RawMessage(`{}`)
	}
	if reveal {
		return trimmed
	}
	return json.RawMessage(`"***"`)
}

func nonEmptyJSONObj(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`)
	}
	return trimmed
}

func deriveRealityPubFromTLS(tlsRaw json.RawMessage) string {
	trimmed := bytes.TrimSpace(tlsRaw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(trimmed, &obj) != nil {
		return ""
	}
	realMap, ok := obj["reality"].(map[string]any)
	if !ok {
		return ""
	}
	priv, _ := realMap["private_key"].(string)
	if strings.TrimSpace(priv) == "" {
		return ""
	}
	pub, err := singbox.DeriveRealityPublicKey(priv)
	if err != nil {
		return ""
	}
	return pub
}

func (s *Service) inboundList(ctx context.Context, inv *command.Invocation) (any, error) {
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
	rows, total, err := s.deps.Inbounds.List(ctx, coreinbounds.ListFilter{
		ServerID:       srvID,
		Protocol:       inv.String("protocol", ""),
		IncludeDeleted: inv.Bool("include-deleted"),
	}, limit, offset)
	if err != nil {
		return nil, err
	}
	nameByID := s.serverNameMap(ctx)
	reveal := v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets)
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, makeInboundView(r, nameByID[r.Spec.ServerID], reveal))
	}
	return &command.PageResult{
		Items:      items,
		Total:      total,
		NextCursor: nextOffsetCursor(offset, len(rows), total),
	}, nil
}

func (s *Service) inboundGet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := parsePositiveID(inv.Arg(0), "Inbound")
	if err != nil {
		return nil, err
	}
	ib, err := s.deps.Inbounds.ByID(ctx, id)
	if errors.Is(err, coreinbounds.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "Inbound 里没有 id 为 %d 的对象", id)
	}
	if err != nil {
		return nil, err
	}
	name := ""
	if srv, err := s.deps.Servers.ByID(ctx, ib.Spec.ServerID); err == nil {
		name = srv.Spec.Name
	}
	return makeInboundView(ib, name, v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets)), nil
}

func (s *Service) inboundProtocols(ctx context.Context, _ *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return singbox.ProtocolMatrix(), nil
}

func (s *Service) inboundExamples(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	items := singbox.ListExamples(
		inv.String("protocol", ""),
		inv.String("transport", ""),
		inv.String("security", ""),
	)
	return map[string]any{
		"items": items,
		"total": len(items),
	}, nil
}

func (s *Service) inboundKeygen(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return singbox.Keygen(inv.String("type", "all"), inv.String("cipher", "2022-blake3-aes-128-gcm"))
}

func (s *Service) isSameHost443Protected(ctx context.Context, srv *coreservers.Server) bool {
	masterURL := ""
	if s.deps.Settings != nil {
		if st, err := s.deps.Settings.Load(ctx); err == nil && st != nil {
			masterURL = st.Gates().MasterURL
		}
	}
	return nginx.IsSameHost443Protected(srv.Status.SameHostAsMaster, srv.Status.Status, srv.Spec.Domain, masterURL)
}

func (s *Service) prepareBuildRequest(ctx context.Context, srv *coreservers.Server, req singbox.BuildInboundRequest) (singbox.BuildInboundRequest, error) {
	if req.Port < 0 || req.Port > 65535 {
		return req, v1.Newf(v1.CodeBadRequest, "port %d 必须在 1..65535 之间（或省略/0 表示自动分配）", req.Port)
	}
	protect443 := s.isSameHost443Protected(ctx, srv)
	if req.Port == 443 && protect443 {
		return req, v1.New(v1.CodeConflict, "主控同机节点禁止分配或监听 443 端口（防止抢占主控 HTTPS 443 端口）")
	}

	proto := strings.ToLower(strings.TrimSpace(req.Protocol))
	info, ok := singbox.LookupProtocol(proto)
	if !ok {
		return req, v1.Newf(v1.CodeBadRequest, "不支持的入站协议 %q（可用 inbound protocols 查看支持清单）", req.Protocol)
	}
	transport := strings.ToLower(strings.TrimSpace(req.Transport))
	if transport == "" {
		transport = info.Transports[0]
	}
	security := strings.ToLower(strings.TrimSpace(req.Security))
	if security == "" {
		if len(info.Securities) == 1 {
			security = info.Securities[0]
		} else if proto == "vless" && transport == "tcp" {
			security = "reality"
		} else {
			security = info.Securities[0]
		}
	}
	isWS := transport == "ws"
	isUDPProto := proto == "hysteria" || proto == "hysteria2" || proto == "tuic"
	mode := strings.ToLower(strings.TrimSpace(srv.Spec.StealMode))
	if mode == "" {
		mode = "tunnel"
	}
	stealActive := srv.Spec.Use443 && !protect443

	// 仅在需要自动分配端口或推导偷自己默认参数时读取同机已有入站。
	if req.Port == 0 {
		existing, err := s.deps.Inbounds.ListByServer(ctx, srv.Metadata.ID)
		if err != nil {
			return req, err
		}
		used := make(map[int]bool, len(existing)+1)
		tcp443Used := false
		udp443Used := false
		for _, ib := range existing {
			if !ib.Spec.Enabled {
				continue
			}
			used[int(ib.Spec.Port)] = true
			if ib.Spec.Port == 443 {
				switch strings.ToLower(strings.TrimSpace(ib.Spec.Protocol)) {
				case "hysteria", "hysteria2", "tuic":
					udp443Used = true
				case "direct":
					lbl := singbox.DirectNetworkLabel(ib.Spec.Settings.Reveal())
					if lbl != "udp" {
						tcp443Used = true
					}
					if lbl != "tcp" {
						udp443Used = true
					}
				case "shadowsocks", "socks", "mixed":
					tcp443Used = true
					udp443Used = true
				default:
					tcp443Used = true
				}
			}
		}
		if stealActive {
			used[nginx.StealListenPort] = true
		}

		switch {
		case stealActive && isWS:
			if strings.TrimSpace(req.Listen) == "" {
				req.Listen = "127.0.0.1"
			}
			p, err := singbox.AllocateFreePort(used, int(srv.Spec.PortRangeMin), int(srv.Spec.PortRangeMax), true)
			if err != nil {
				return req, err
			}
			req.Port = p

		case stealActive && mode == "fallback" && !isUDPProto && (security == "reality" || security == "tls") && !tcp443Used:
			req.Port = 443

		case stealActive && isUDPProto && !udp443Used:
			req.Port = 443

		case stealActive && mode == "tunnel" && !isUDPProto && (security == "reality" || security == "tls"):
			if strings.TrimSpace(req.Listen) == "" {
				req.Listen = "127.0.0.1"
			}
			// 若 tunnel-in 已有兜底转发内部端口且尚未被任何活跃入站占用，优先复用该内部端口。
			reused := 0
			if s.deps.RoutingRules != nil {
				if rrs, rerr := s.deps.RoutingRules.ListByServer(ctx, srv.Metadata.ID); rerr == nil {
					if cand := findTunnelFallbackInternalPort(rrs); cand > 0 && cand != 443 && !used[cand] {
						reused = cand
					}
				}
			}
			if reused > 0 {
				req.Port = reused
			} else {
				p, err := singbox.AllocateFreePort(used, int(srv.Spec.PortRangeMin), int(srv.Spec.PortRangeMax), true)
				if err != nil {
					return req, err
				}
				req.Port = p
			}

		default:
			p, err := singbox.AllocateFreePort(used, int(srv.Spec.PortRangeMin), int(srv.Spec.PortRangeMax), protect443)
			if err != nil {
				return req, err
			}
			req.Port = p
		}
	}

	// 开启偷自己（use_443=true）时为非 WS 的 Reality/TLS 入站补齐默认 sni 与 127.0.0.1:8001 handshake（仅填充未显式传入项）。
	if stealActive && !isWS && (security == "reality" || security == "tls") {
		dom := strings.TrimSpace(srv.Spec.Domain)
		if strings.TrimSpace(req.SNI) == "" && dom != "" {
			req.SNI = dom
		}
		if security == "reality" && strings.TrimSpace(req.Handshake) == "" {
			req.Handshake = fmt.Sprintf("127.0.0.1:%d", nginx.StealListenPort)
		}
		if security == "tls" && dom != "" {
			normDom := strings.ToLower(strings.TrimSuffix(dom, "."))
			if strings.TrimSpace(req.CertPath) == "" {
				req.CertPath = fmt.Sprintf("%s/%s.crt", nginx.DefaultCertDir, normDom)
			}
			if strings.TrimSpace(req.KeyPath) == "" {
				req.KeyPath = fmt.Sprintf("%s/%s.key", nginx.DefaultCertDir, normDom)
			}
		}
	}
	return req, nil
}

func findTunnelFallbackInternalPort(rrs []*corerouting.RoutingRule) int {
	for _, rr := range rrs {
		if !rr.Spec.Enabled || rr.Spec.CatchAll {
			continue
		}
		var m map[string]any
		if json.Unmarshal(rr.Spec.Match, &m) != nil {
			continue
		}
		inList, _ := m["inbound"].([]any)
		isTunnelIn := false
		for _, item := range inList {
			if s, ok := item.(string); ok && s == singbox.StealTunnelInboundTag {
				isTunnelIn = true
				break
			}
		}
		if !isTunnelIn {
			continue
		}
		// 跳过域名分流规则（指向 8001），取无 domain 条件的兜底内部端口。
		if _, hasDom := m["domain"]; hasDom {
			continue
		}
		if _, hasDomSuf := m["domain_suffix"]; hasDomSuf {
			continue
		}
		if addr, _ := m["override_address"].(string); addr != "127.0.0.1" {
			continue
		}
		if fp, ok := m["override_port"].(float64); ok && fp >= 1 && fp <= 65535 && int(fp) != nginx.StealListenPort {
			return int(fp)
		}
	}
	return 0
}

func (s *Service) inboundBuild(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.resolveServer(ctx, inv.String("server-name", ""))
	if err != nil {
		return nil, err
	}
	req, err := s.prepareBuildRequest(ctx, srv, singbox.BuildInboundRequest{
		ServerID:    srv.Metadata.ID,
		Tag:         inv.String("tag", ""),
		Protocol:    inv.String("protocol", ""),
		Port:        inv.Int("port", 0),
		Listen:      inv.String("listen", ""),
		Transport:   inv.String("transport", ""),
		Security:    inv.String("security", ""),
		SNI:         inv.String("sni", ""),
		Handshake:   inv.String("handshake", ""),
		Path:        inv.String("path", ""),
		ServiceName: inv.String("service-name", ""),
		Cipher:      inv.String("cipher", ""),
		Flow:        inv.String("flow", ""),
		CertPath:    inv.String("cert-path", ""),
		KeyPath:     inv.String("key-path", ""),
		UserName:    inv.String("user-name", ""),
		Password:    inv.String("password", ""),
		UUID:        inv.String("uuid", ""),
		SortOrder:   inv.Int("sort-order", 0),
	})
	if err != nil {
		return nil, err
	}
	built, err := singbox.BuildInbound(req)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"server":      srv.Spec.Name,
		"server_id":   srv.Metadata.ID,
		"spec":        built.PlanOp["spec"],
		"credentials": built.Credentials,
		"plan_op":     built.PlanOp,
	}, nil
}

func (s *Service) inboundBatchList(ctx context.Context, inv *command.Invocation) (any, error) {
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
	rows, total, err := s.deps.BatchInbounds.List(ctx, inv.String("batch-id", ""), srvID, limit, offset)
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

func (s *Service) inboundBatchPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Planner == nil {
		return nil, v1.New(v1.CodeInternal, "配置计划服务未装配")
	}
	targets, err := s.resolveTargetServers(ctx, inv)
	if err != nil {
		return nil, err
	}
	tplRaw := strings.TrimSpace(inv.String("spec", ""))
	ops := make([]map[string]any, 0, len(targets))

	if tplRaw != "" {
		// 模板 JSON 模式：将同一 InboundSpec 模板绑定到每台目标服务器。
		var baseSpec map[string]any
		if err := json.Unmarshal([]byte(tplRaw), &baseSpec); err != nil || baseSpec == nil {
			return nil, v1.New(v1.CodeBadRequest, "--spec 必须是合法的 Inbound spec JSON 对象")
		}
		for _, srv := range targets {
			cloned := make(map[string]any, len(baseSpec)+1)
			for k, v := range baseSpec {
				cloned[k] = v
			}
			cloned["server_id"] = srv.Metadata.ID
			if _, ok := cloned["enabled"]; !ok {
				cloned["enabled"] = true
			}
			ops = append(ops, map[string]any{
				"op":   "create",
				"kind": coreinbounds.KindName,
				"spec": cloned,
			})
		}
	} else {
		// 向导参数模式：为每台服务器独立生成新鲜凭据/密钥并适配各服务器端口范围与偷自己默认值。
		for _, srv := range targets {
			req, err := s.prepareBuildRequest(ctx, srv, singbox.BuildInboundRequest{
				ServerID:    srv.Metadata.ID,
				Tag:         inv.String("tag", ""),
				Protocol:    inv.String("protocol", ""),
				Port:        inv.Int("port", 0),
				Listen:      inv.String("listen", ""),
				Transport:   inv.String("transport", ""),
				Security:    inv.String("security", ""),
				SNI:         inv.String("sni", ""),
				Handshake:   inv.String("handshake", ""),
				Path:        inv.String("path", ""),
				ServiceName: inv.String("service-name", ""),
				Cipher:      inv.String("cipher", ""),
				Flow:        inv.String("flow", ""),
				CertPath:    inv.String("cert-path", ""),
				KeyPath:     inv.String("key-path", ""),
				UserName:    inv.String("user-name", ""),
				Password:    inv.String("password", ""),
				UUID:        inv.String("uuid", ""),
				SortOrder:   inv.Int("sort-order", 0),
			})
			if err != nil {
				return nil, err
			}
			built, err := singbox.BuildInbound(req)
			if err != nil {
				return nil, err
			}
			ops = append(ops, built.PlanOp)
		}
	}
	opsJSON, err := json.Marshal(ops)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码批量入站计划失败", err)
	}
	return s.deps.Planner.CreatePlanFromJSON(ctx, string(opsJSON), coreinbounds.KindName, newBatchID())
}

func (s *Service) serverNameMap(ctx context.Context) map[int64]string {
	m := map[int64]string{}
	srvs, err := s.deps.Servers.ListAll(ctx)
	if err != nil {
		return m
	}
	for _, srv := range srvs {
		m[srv.Metadata.ID] = srv.Spec.Name
	}
	return m
}
