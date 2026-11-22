package servers

import (
	"context"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	networkProbeTimeout = 65 * time.Second
	warpInstallTimeout  = 70 * time.Second
)

// CallNetworkPortScan 调用节点 network.port_scan 管理口 RPC。
func (s *Service) CallNetworkPortScan(ctx context.Context, serverID int64, params xrpc.NetworkPortScanParams) (*xrpc.NetworkPortScanResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NetworkPortScanResult
	if err := s.call(ctx, srv, xrpc.MethodNetworkPortScan, params, &res, 35*time.Second); err != nil {
		return nil, err
	}
	if res.OpenPorts == nil {
		res.OpenPorts = []int{}
	}
	if res.Results == nil {
		res.Results = []xrpc.PortScanItem{}
	}
	return &res, nil
}

// CallNetworkValidateSite 调用节点 network.validate_site 管理口 RPC。
func (s *Service) CallNetworkValidateSite(ctx context.Context, serverID int64, params xrpc.NetworkValidateSiteParams) (*xrpc.NetworkValidateSiteResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	timeout := time.Duration(params.TimeoutMS)*time.Millisecond + 5*time.Second
	if timeout < callTimeout {
		timeout = callTimeout
	}
	if timeout > 40*time.Second {
		timeout = 40 * time.Second
	}
	var res xrpc.NetworkValidateSiteResult
	if err := s.call(ctx, srv, xrpc.MethodNetworkValidateSite, params, &res, timeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallNetworkDomainLatency 调用节点 network.domain_latency 管理口 RPC。
func (s *Service) CallNetworkDomainLatency(ctx context.Context, serverID int64, params xrpc.NetworkDomainLatencyParams) (*xrpc.NetworkDomainLatencyResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NetworkDomainLatencyResult
	if err := s.call(ctx, srv, xrpc.MethodNetworkDomainLatency, params, &res, networkProbeTimeout); err != nil {
		return nil, err
	}
	if res.Results == nil {
		res.Results = []xrpc.DomainLatencyItem{}
	}
	if res.Fastest == nil {
		res.Fastest = []xrpc.DomainLatencyItem{}
	}
	return &res, nil
}

// CallNetworkReturnRouteTest 调用节点 network.return_route_test 管理口 RPC。
func (s *Service) CallNetworkReturnRouteTest(ctx context.Context, serverID int64, params xrpc.NetworkReturnRouteTestParams) (*xrpc.NetworkReturnRouteTestResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NetworkReturnRouteTestResult
	if err := s.call(ctx, srv, xrpc.MethodNetworkReturnRouteTest, params, &res, 50*time.Second); err != nil {
		return nil, err
	}
	if res.Hops == nil {
		res.Hops = []xrpc.ReturnRouteHop{}
	}
	return &res, nil
}

// CallWarpStatus 调用节点 warp.status 管理口 RPC。
func (s *Service) CallWarpStatus(ctx context.Context, serverID int64) (*xrpc.WarpStatusResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.WarpStatusResult
	if err := s.call(ctx, srv, xrpc.MethodWarpStatus, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallWarpInstall 调用节点 warp.install 管理口 RPC。
func (s *Service) CallWarpInstall(ctx context.Context, serverID int64, params xrpc.WarpInstallParams) (*xrpc.WarpInstallResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.WarpInstallResult
	if err := s.call(ctx, srv, xrpc.MethodWarpInstall, params, &res, warpInstallTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallWarpAuthorize 调用节点 warp.authorize 管理口 RPC。
func (s *Service) CallWarpAuthorize(ctx context.Context, serverID int64, params xrpc.WarpAuthorizeParams) (*xrpc.WarpAuthorizeResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.WarpAuthorizeResult
	if err := s.call(ctx, srv, xrpc.MethodWarpAuthorize, params, &res, warpInstallTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallWarpRemove 调用节点 warp.remove 管理口 RPC。
func (s *Service) CallWarpRemove(ctx context.Context, serverID int64) (*xrpc.WarpRemoveResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.WarpRemoveResult
	if err := s.call(ctx, srv, xrpc.MethodWarpRemove, xrpc.WarpRemoveParams{}, &res, callTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallWarpCredentials 调用节点 warp.credentials 管理口 RPC（仅用于主控构造 WARP WireGuard 出站 Plan）。
func (s *Service) CallWarpCredentials(ctx context.Context, serverID int64) (*xrpc.WarpCredentialsResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.WarpCredentialsResult
	if err := s.call(ctx, srv, xrpc.MethodWarpCredentials, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}
