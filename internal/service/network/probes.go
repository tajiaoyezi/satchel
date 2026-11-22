package network

import (
	"context"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// ServerScanView 是 server scan 的返回视图。
type ServerScanView struct {
	Server string `json:"server"`
	*xrpc.NetworkPortScanResult
}

// WebsiteValidateView 是 website validate 的返回视图。
type WebsiteValidateView struct {
	Server string `json:"server"`
	*xrpc.NetworkValidateSiteResult
}

// DomainLatencyView 是 server domain-latency 的返回视图。
type DomainLatencyView struct {
	Server string `json:"server"`
	*xrpc.NetworkDomainLatencyResult
}

func (s *Service) serverScan(ctx context.Context, inv *command.Invocation) (any, error) {
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

	host := strings.TrimSpace(inv.String("host", ""))
	if host == "" {
		host = "127.0.0.1"
	}
	host, err = validateHost(host)
	if err != nil {
		return nil, err
	}

	ports, err := xrpc.ParsePortSpecs(inv.Strings("ports"), xrpc.PortScanMaxPorts)
	if err != nil {
		return nil, v1.New(v1.CodeBadRequest, err.Error())
	}

	timeoutMS := xrpc.PortScanDefaultTimeoutMS
	if d := inv.Duration("timeout", 0); d != 0 {
		timeoutMS = int(d / time.Millisecond)
		if timeoutMS < xrpc.PortScanMinTimeoutMS || timeoutMS > xrpc.PortScanMaxTimeoutMS {
			return nil, v1.Newf(v1.CodeBadRequest, "--timeout 必须在 %dms..%dms 之间", xrpc.PortScanMinTimeoutMS, xrpc.PortScanMaxTimeoutMS)
		}
	}

	conc := inv.Int("concurrency", 0)
	if conc == 0 {
		conc = xrpc.PortScanDefaultConcurrency
	}
	if conc < 1 || conc > xrpc.PortScanMaxConcurrency {
		return nil, v1.Newf(v1.CodeBadRequest, "--concurrency 必须在 1..%d 之间", xrpc.PortScanMaxConcurrency)
	}

	res, err := s.deps.Nodes.CallNetworkPortScan(ctx, srv.Metadata.ID, xrpc.NetworkPortScanParams{
		Host:        host,
		Ports:       ports,
		TimeoutMS:   timeoutMS,
		Concurrency: conc,
	})
	if err != nil {
		return nil, err
	}
	return &ServerScanView{Server: srv.Spec.Name, NetworkPortScanResult: res}, nil
}

func (s *Service) websiteValidate(ctx context.Context, inv *command.Invocation) (any, error) {
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

	domain, err := validateHost(inv.String("domain", ""))
	if err != nil {
		return nil, v1.New(v1.CodeBadRequest, "--domain 无效："+err.Error())
	}
	port := inv.Int("port", xrpc.ValidateSiteDefaultPort)
	if port < 1 || port > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--port %d 必须在 1..65535 之间", port)
	}
	sni := strings.TrimSpace(inv.String("sni", ""))
	if sni != "" {
		sni, err = validateHost(sni)
		if err != nil {
			return nil, v1.New(v1.CodeBadRequest, "--sni 无效："+err.Error())
		}
	}
	timeoutMS := xrpc.ValidateSiteDefaultTimeoutMS
	if d := inv.Duration("timeout", 0); d != 0 {
		timeoutMS = int(d / time.Millisecond)
		if timeoutMS < xrpc.ValidateSiteMinTimeoutMS || timeoutMS > xrpc.ValidateSiteMaxTimeoutMS {
			return nil, v1.Newf(v1.CodeBadRequest, "--timeout 必须在 %dms..%dms 之间", xrpc.ValidateSiteMinTimeoutMS, xrpc.ValidateSiteMaxTimeoutMS)
		}
	}

	res, err := s.deps.Nodes.CallNetworkValidateSite(ctx, srv.Metadata.ID, xrpc.NetworkValidateSiteParams{
		Domain:    domain,
		Port:      port,
		SNI:       sni,
		TimeoutMS: timeoutMS,
	})
	if err != nil {
		return nil, err
	}
	return &WebsiteValidateView{Server: srv.Spec.Name, NetworkValidateSiteResult: res}, nil
}

func (s *Service) serverDomainLatency(ctx context.Context, inv *command.Invocation) (any, error) {
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

	rawDomains := inv.Strings("domains")
	if len(rawDomains) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "必须提供至少一个 --domains 域名")
	}
	seen := make(map[string]struct{}, len(rawDomains))
	domains := make([]string, 0, len(rawDomains))
	for _, entry := range rawDomains {
		d, herr := validateHost(entry)
		if herr != nil {
			return nil, v1.New(v1.CodeBadRequest, "--domains 包含非法项："+herr.Error())
		}
		key := strings.ToLower(d)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		domains = append(domains, d)
		if len(domains) > xrpc.DomainLatencyMaxDomains {
			return nil, v1.Newf(v1.CodeBadRequest, "--domains 去重后最多允许 %d 个域名", xrpc.DomainLatencyMaxDomains)
		}
	}

	port := inv.Int("port", xrpc.DomainLatencyDefaultPort)
	if port < 1 || port > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--port %d 必须在 1..65535 之间", port)
	}
	mode := strings.ToLower(strings.TrimSpace(inv.String("mode", "tls")))
	if mode == "" {
		mode = "tls"
	}
	if mode != "tls" && mode != "tcp" {
		return nil, v1.Newf(v1.CodeBadRequest, "--mode %q 不支持（仅允许 tls 或 tcp）", mode)
	}
	timeoutMS := xrpc.DomainLatencyDefaultTimeoutMS
	if d := inv.Duration("timeout", 0); d != 0 {
		timeoutMS = int(d / time.Millisecond)
		if timeoutMS < xrpc.DomainLatencyMinTimeoutMS || timeoutMS > xrpc.DomainLatencyMaxTimeoutMS {
			return nil, v1.Newf(v1.CodeBadRequest, "--timeout 必须在 %dms..%dms 之间", xrpc.DomainLatencyMinTimeoutMS, xrpc.DomainLatencyMaxTimeoutMS)
		}
	}
	conc := inv.Int("concurrency", xrpc.DomainLatencyMaxConcurrency)
	if conc == 0 {
		conc = xrpc.DomainLatencyMaxConcurrency
	}
	if conc < 1 || conc > xrpc.DomainLatencyMaxConcurrency {
		return nil, v1.Newf(v1.CodeBadRequest, "--concurrency 必须在 1..%d 之间", xrpc.DomainLatencyMaxConcurrency)
	}

	res, err := s.deps.Nodes.CallNetworkDomainLatency(ctx, srv.Metadata.ID, xrpc.NetworkDomainLatencyParams{
		Domains:     domains,
		Port:        port,
		Mode:        mode,
		TimeoutMS:   timeoutMS,
		Concurrency: conc,
	})
	if err != nil {
		return nil, err
	}
	return &DomainLatencyView{Server: srv.Spec.Name, NetworkDomainLatencyResult: res}, nil
}
