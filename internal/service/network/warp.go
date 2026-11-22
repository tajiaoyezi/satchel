package network

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// WarpOutboundSummary 描述服务器上已配置的 WARP 出站摘要。
type WarpOutboundSummary struct {
	ID       int64  `json:"id"`
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
}

// WarpStatusView 是 server warp status 的返回视图（不含私钥/token/license）。
type WarpStatusView struct {
	Server               string                `json:"server"`
	ServerWarpInstalled  bool                  `json:"server_warp_installed"`
	WarpOutbounds        []WarpOutboundSummary `json:"warp_outbounds"`
	AntiChinaRulePresent bool                  `json:"anti_china_rule_present"`
	*xrpc.WarpStatusResult
}

// WarpInstallView 是 server warp install 长任务的终态输出。
type WarpInstallView struct {
	Server string `json:"server"`
	*xrpc.WarpInstallResult
}

// WarpAuthorizeView 是 server warp authorize 的返回视图。
type WarpAuthorizeView struct {
	Server string `json:"server"`
	*xrpc.WarpAuthorizeResult
}

// WarpRemoveView 是 server warp remove 的返回视图。
type WarpRemoveView struct {
	Server                 string   `json:"server"`
	RemainingWarpOutbounds []string `json:"remaining_warp_outbounds"`
	*xrpc.WarpRemoveResult
}

func (s *Service) warpStatus(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Nodes == nil {
		return nil, v1.New(v1.CodeInternal, "节点通道服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	res, err := s.deps.Nodes.CallWarpStatus(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}

	warpTags := map[string]bool{}
	outSummaries := make([]WarpOutboundSummary, 0)
	if s.deps.Outbounds != nil {
		obs, oerr := s.deps.Outbounds.ListByServer(ctx, srv.Metadata.ID)
		if oerr != nil {
			return nil, oerr
		}
		for _, ob := range obs {
			if ob.Spec.IsWarp {
				warpTags[ob.Spec.Tag] = true
				outSummaries = append(outSummaries, WarpOutboundSummary{
					ID: ob.Metadata.ID, Tag: ob.Spec.Tag, Protocol: ob.Spec.Protocol,
				})
			}
		}
	}

	antiChina := false
	if s.deps.RoutingRules != nil && len(warpTags) > 0 {
		if rrs, rerr := s.deps.RoutingRules.ListByServer(ctx, srv.Metadata.ID); rerr == nil {
			for _, rr := range rrs {
				if rr.Spec.Enabled && warpTags[rr.Spec.OutboundTag] &&
					strings.Contains(string(rr.Spec.Match), "geosite-google") {
					antiChina = true
					break
				}
			}
		}
	}

	return &WarpStatusView{
		Server:               srv.Spec.Name,
		ServerWarpInstalled:  srv.Status.WarpInstalled,
		WarpOutbounds:        outSummaries,
		AntiChinaRulePresent: antiChina,
		WarpStatusResult:     res,
	}, nil
}

func (s *Service) warpInstall(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Nodes == nil || s.deps.StartJob == nil {
		return nil, v1.New(v1.CodeInternal, "节点通道或长任务服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}

	mtu := inv.Int("mtu", 0)
	if mtu != 0 && (mtu < xrpc.MinWarpMTU || mtu > xrpc.MaxWarpMTU) {
		return nil, v1.Newf(v1.CodeBadRequest, "--mtu %d 必须在 %d..%d 之间", mtu, xrpc.MinWarpMTU, xrpc.MaxWarpMTU)
	}
	endpoint := strings.TrimSpace(inv.String("endpoint", ""))
	if endpoint != "" {
		if err := validateEndpoint(endpoint); err != nil {
			return nil, err
		}
	}
	apiBase := strings.TrimSpace(inv.String("api-base", ""))
	if apiBase != "" {
		if err := validateHTTPURL(apiBase, "--api-base"); err != nil {
			return nil, err
		}
	}
	params := xrpc.WarpInstallParams{
		LicenseKey: strings.TrimSpace(inv.String("license-key", "")),
		MTU:        mtu,
		Endpoint:   endpoint,
		APIBase:    apiBase,
		Force:      inv.Bool("reinstall"),
	}

	return s.deps.StartJob(ctx, srv.Metadata.ID, inv, func(jobCtx context.Context) (any, error) {
		res, cerr := s.deps.Nodes.CallWarpInstall(jobCtx, srv.Metadata.ID, params)
		if cerr != nil {
			return nil, cerr
		}
		if res.Installed {
			if uerr := s.deps.Servers.SetWarpInstalled(jobCtx, srv.Metadata.ID, true); uerr != nil {
				return nil, uerr
			}
		}
		return &WarpInstallView{Server: srv.Spec.Name, WarpInstallResult: res}, nil
	}, nil)
}

func (s *Service) warpAuthorize(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Nodes == nil {
		return nil, v1.New(v1.CodeInternal, "节点通道服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}

	licenseKey := strings.TrimSpace(inv.String("license-key", ""))
	privKey := strings.TrimSpace(inv.String("private-key", ""))
	addrV4 := strings.TrimSpace(inv.String("address-v4", ""))
	hasLicense := licenseKey != ""
	hasImport := privKey != "" || addrV4 != ""
	if hasLicense && hasImport {
		return nil, v1.New(v1.CodeBadRequest, "--license-key 与离线导入参数（--private-key / --address-v4）不能同时指定")
	}
	if !hasLicense && !hasImport {
		return nil, v1.New(v1.CodeBadRequest, "必须指定 --license-key（绑定 WARP+）或同时指定 --private-key 与 --address-v4（离线导入）")
	}

	mtu := inv.Int("mtu", 0)
	if mtu != 0 && (mtu < xrpc.MinWarpMTU || mtu > xrpc.MaxWarpMTU) {
		return nil, v1.Newf(v1.CodeBadRequest, "--mtu %d 必须在 %d..%d 之间", mtu, xrpc.MinWarpMTU, xrpc.MaxWarpMTU)
	}
	endpoint := strings.TrimSpace(inv.String("endpoint", ""))
	if endpoint != "" {
		if err := validateEndpoint(endpoint); err != nil {
			return nil, err
		}
	}
	apiBase := strings.TrimSpace(inv.String("api-base", ""))
	if apiBase != "" {
		if err := validateHTTPURL(apiBase, "--api-base"); err != nil {
			return nil, err
		}
	}
	reserved, err := parseReservedFlag(inv.String("reserved", ""))
	if err != nil {
		return nil, err
	}

	res, err := s.deps.Nodes.CallWarpAuthorize(ctx, srv.Metadata.ID, xrpc.WarpAuthorizeParams{
		LicenseKey:    licenseKey,
		APIBase:       apiBase,
		PrivateKey:    privKey,
		AddressV4:     addrV4,
		AddressV6:     strings.TrimSpace(inv.String("address-v6", "")),
		PeerPublicKey: strings.TrimSpace(inv.String("peer-public-key", "")),
		Endpoint:      endpoint,
		Reserved:      reserved,
		MTU:           mtu,
	})
	if err != nil {
		return nil, err
	}
	if res.Installed {
		if uerr := s.deps.Servers.SetWarpInstalled(ctx, srv.Metadata.ID, true); uerr != nil {
			return nil, uerr
		}
	}
	return &WarpAuthorizeView{Server: srv.Spec.Name, WarpAuthorizeResult: res}, nil
}

func (s *Service) warpRemove(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Nodes == nil {
		return nil, v1.New(v1.CodeInternal, "节点通道服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	res, err := s.deps.Nodes.CallWarpRemove(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Servers.SetWarpInstalled(ctx, srv.Metadata.ID, false); err != nil {
		return nil, err
	}
	remaining := make([]string, 0)
	if s.deps.Outbounds != nil {
		if obs, oerr := s.deps.Outbounds.ListByServer(ctx, srv.Metadata.ID); oerr == nil {
			for _, ob := range obs {
				if ob.Spec.IsWarp {
					remaining = append(remaining, ob.Spec.Tag)
				}
			}
		}
	}
	return &WarpRemoveView{
		Server:                 srv.Spec.Name,
		RemainingWarpOutbounds: remaining,
		WarpRemoveResult:       res,
	}, nil
}

func (s *Service) warpOutboundPlan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Nodes == nil || s.deps.Planner == nil || s.deps.Outbounds == nil {
		return nil, v1.New(v1.CodeInternal, "WARP 出站规划所需服务未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}

	tag := strings.TrimSpace(inv.String("tag", "warp"))
	if tag == "" {
		tag = "warp"
	}
	if tag == "direct" || tag == "block" || tag == "reject" {
		return nil, v1.Newf(v1.CodeBadRequest, "--tag %q 为 sing-box 保留出站名，请改用其它名称", tag)
	}
	antiChina := inv.Bool("anti-china")
	inboundTag := strings.TrimSpace(inv.String("inbound-tag", ""))
	if inboundTag != "" && !antiChina {
		return nil, v1.New(v1.CodeBadRequest, "--inbound-tag 仅在启用 --anti-china 时使用")
	}
	sortOrder := inv.Int("sort-order", 40)
	if sortOrder <= 0 {
		sortOrder = 40
	}

	if inboundTag != "" && s.deps.Inbounds != nil {
		ib, ierr := s.deps.Inbounds.ByServerTagIncludeDeleted(ctx, srv.Metadata.ID, inboundTag)
		if errors.Is(ierr, coreinbounds.ErrNotFound) || (ierr == nil && ib.Metadata.DeletedAt != nil) {
			return nil, v1.Newf(v1.CodeNotFound, "服务器 %q 上没有活跃入站 %q", srv.Spec.Name, inboundTag)
		}
		if ierr != nil {
			return nil, ierr
		}
	}

	creds, err := s.deps.Nodes.CallWarpCredentials(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	if !creds.Installed || strings.TrimSpace(creds.PrivateKey) == "" || strings.TrimSpace(creds.AddressV4) == "" {
		return nil, v1.New(v1.CodeConflict, "节点尚未安装有效的 WARP 凭据")
	}

	wgSettings, err := buildSingBoxWireGuardSettings(creds)
	if err != nil {
		return nil, err
	}

	outSpecMap := map[string]any{
		"server_id": srv.Metadata.ID,
		"tag":       tag,
		"protocol":  "wireguard",
		"is_warp":   true,
		"settings":  wgSettings,
	}

	var ops []map[string]any
	existing, err := s.deps.Outbounds.ByServerTagIncludeDeleted(ctx, srv.Metadata.ID, tag)
	switch {
	case errors.Is(err, coreoutbounds.ErrNotFound):
		ops = append(ops, map[string]any{
			"op":   "create",
			"kind": coreoutbounds.KindName,
			"spec": outSpecMap,
		})
	case err != nil:
		return nil, err
	case existing.Metadata.DeletedAt != nil:
		return nil, v1.Newf(v1.CodeNameTaken, "服务器 %q 上已存在软删除的同名出站 %q（id=%d），请先恢复或改用其它 --tag", srv.Spec.Name, tag, existing.Metadata.ID)
	default:
		ops = append(ops, map[string]any{
			"op":              "update",
			"kind":            coreoutbounds.KindName,
			"id":              existing.Metadata.ID,
			"resourceVersion": existing.Metadata.ResourceVersion,
			"spec":            outSpecMap,
		})
	}

	if antiChina {
		match := map[string]any{
			"rule_set": []string{"geosite-google", "geosite-meta"},
		}
		for _, p := range singbox.BuiltinRoutingPresets() {
			if p.Key == "warp-google-meta" {
				var pm map[string]any
				if json.Unmarshal(p.Match, &pm) == nil && pm != nil {
					match = pm
				}
				break
			}
		}
		if inboundTag != "" {
			match["inbound"] = []string{inboundTag}
		}
		ops = append(ops, map[string]any{
			"op":   "create",
			"kind": corerouting.KindName,
			"spec": map[string]any{
				"server_id":    srv.Metadata.ID,
				"sort_order":   sortOrder,
				"match":        match,
				"outbound_tag": tag,
				"enabled":      true,
			},
		})
	}

	rawOps, err := json.Marshal(ops)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "构造 WARP Plan 失败", err)
	}
	return s.deps.Planner.CreatePlanFromJSON(ctx, string(rawOps), "", "")
}

func buildSingBoxWireGuardSettings(c *xrpc.WarpCredentialsResult) (map[string]any, error) {
	ep := strings.TrimSpace(c.Endpoint)
	if ep == "" {
		ep = xrpc.DefaultWarpEndpoint
	}
	host, portStr, err := net.SplitHostPort(ep)
	if err != nil {
		return nil, v1.Newf(v1.CodeBadRequest, "节点 WARP endpoint %q 格式非法", ep)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "节点 WARP endpoint 端口 %q 非法", portStr)
	}
	peerPub := strings.TrimSpace(c.PeerPublicKey)
	if peerPub == "" {
		peerPub = xrpc.DefaultWarpPeerPublicKey
	}
	mtu := c.MTU
	if mtu == 0 {
		mtu = xrpc.DefaultWarpMTU
	}
	addrs := []string{strings.TrimSpace(c.AddressV4)}
	if v6 := strings.TrimSpace(c.AddressV6); v6 != "" {
		addrs = append(addrs, v6)
	}
	peer := map[string]any{
		"address":     host,
		"port":        port,
		"public_key":  peerPub,
		"allowed_ips": []string{"0.0.0.0/0", "::/0"},
	}
	if len(c.Reserved) == 3 {
		peer["reserved"] = append([]int(nil), c.Reserved...)
	}
	return map[string]any{
		"mtu":         mtu,
		"address":     addrs,
		"private_key": strings.TrimSpace(c.PrivateKey),
		"peers":       []map[string]any{peer},
	}, nil
}

func parseReservedFlag(raw string) ([]int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return nil, v1.Newf(v1.CodeBadRequest, "--reserved %q 必须为 3 个逗号分隔的 0..255 整数（如 12,34,56）", raw)
	}
	out := make([]int, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || n > 255 {
			return nil, v1.Newf(v1.CodeBadRequest, "--reserved 包含非法字节 %q（必须在 0..255 之间）", part)
		}
		out[i] = n
	}
	return out, nil
}

func validateEndpoint(raw string) error {
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil || strings.TrimSpace(host) == "" {
		return v1.Newf(v1.CodeBadRequest, "--endpoint %q 必须是 host:port 格式", raw)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return v1.Newf(v1.CodeBadRequest, "--endpoint 端口 %q 必须在 1..65535 之间", portStr)
	}
	return nil
}

func validateHTTPURL(raw, flagName string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return v1.Newf(v1.CodeBadRequest, "%s %q 必须是合法的 http:// 或 https:// 地址", flagName, raw)
	}
	return nil
}
