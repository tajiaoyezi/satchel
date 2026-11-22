package xrpc

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 网络探测三项工具与返程路由测路的共享上下限常量，主控与节点统一遵循。
const (
	PortScanMaxPorts           = 256
	PortScanDefaultTimeoutMS   = 1000
	PortScanMinTimeoutMS       = 100
	PortScanMaxTimeoutMS       = 5000
	PortScanDefaultConcurrency = 32
	PortScanMaxConcurrency     = 64

	ValidateSiteDefaultPort      = 443
	ValidateSiteDefaultTimeoutMS = 10000
	ValidateSiteMinTimeoutMS     = 1000
	ValidateSiteMaxTimeoutMS     = 30000

	DomainLatencyMaxDomains       = 200
	DomainLatencyDefaultPort      = 443
	DomainLatencyMaxConcurrency   = 16
	DomainLatencyDefaultTimeoutMS = 5000
	DomainLatencyMinTimeoutMS     = 500
	DomainLatencyMaxTimeoutMS     = 15000

	ReturnRouteDefaultMaxHops   = 30
	ReturnRouteMaxHops          = 64
	ReturnRouteDefaultTimeoutMS = 3000
	ReturnRouteMinTimeoutMS     = 500
	ReturnRouteMaxTimeoutMS     = 15000
)

// DefaultPortScanPorts 是未指定 --ports 时的默认常用端口清单。
var DefaultPortScanPorts = []int{22, 80, 443, 8080, 8443}

// ParsePortSpecs 将端口表达式（单端口如 "443" 或闭区间如 "8000-8010"）解析为去重升序端口列表，并强制不超过 maxPorts。
func ParsePortSpecs(specs []string, maxPorts int) ([]int, error) {
	if maxPorts <= 0 {
		maxPorts = PortScanMaxPorts
	}
	if len(specs) == 0 {
		out := make([]int, len(DefaultPortScanPorts))
		copy(out, DefaultPortScanPorts)
		return out, nil
	}
	seen := make(map[int]struct{}, len(specs))
	for _, raw := range specs {
		for _, tok := range strings.Split(raw, ",") {
			part := strings.TrimSpace(tok)
			if part == "" {
				return nil, fmt.Errorf("端口列表不能包含空白项")
			}
			if loStr, hiStr, isRange := strings.Cut(part, "-"); isRange {
				lo, err1 := strconv.Atoi(strings.TrimSpace(loStr))
				hi, err2 := strconv.Atoi(strings.TrimSpace(hiStr))
				if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
					return nil, fmt.Errorf("非法端口区间 %q（需满足 1 <= 起始 <= 结束 <= 65535）", part)
				}
				if hi-lo+1 > maxPorts {
					return nil, fmt.Errorf("端口区间 %q 超出单次扫描上限 %d 个端口", part, maxPorts)
				}
				for p := lo; p <= hi; p++ {
					seen[p] = struct{}{}
					if len(seen) > maxPorts {
						return nil, fmt.Errorf("待扫描端口总数超出单次上限 %d", maxPorts)
					}
				}
			} else {
				p, err := strconv.Atoi(part)
				if err != nil || p < 1 || p > 65535 {
					return nil, fmt.Errorf("非法端口号 %q（必须在 1..65535 之间）", part)
				}
				seen[p] = struct{}{}
				if len(seen) > maxPorts {
					return nil, fmt.Errorf("待扫描端口总数超出单次上限 %d", maxPorts)
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("至少提供一个待扫描端口")
	}
	out := make([]int, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}

// NetworkPortScanParams 是 network.port_scan 的参数。
type NetworkPortScanParams struct {
	Host        string `json:"host,omitempty"`
	Ports       []int  `json:"ports"`
	TimeoutMS   int    `json:"timeout_ms,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

// PortScanItem 描述单个端口的扫描结果。
type PortScanItem struct {
	Port      int    `json:"port"`
	Open      bool   `json:"open"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Banner    string `json:"banner,omitempty"`
	Error     string `json:"error,omitempty"`
}

// NetworkPortScanResult 是 network.port_scan 的回复。
type NetworkPortScanResult struct {
	Host         string         `json:"host"`
	ScannedCount int            `json:"scanned_count"`
	OpenPorts    []int          `json:"open_ports"`
	ClosedCount  int            `json:"closed_count"`
	Results      []PortScanItem `json:"results"`
	DurationMS   int64          `json:"duration_ms"`
}

// NetworkValidateSiteParams 是 network.validate_site 的参数。
type NetworkValidateSiteParams struct {
	Domain    string `json:"domain"`
	Port      int    `json:"port,omitempty"`
	SNI       string `json:"sni,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// NetworkValidateSiteResult 是 network.validate_site 的回复。
type NetworkValidateSiteResult struct {
	Domain          string     `json:"domain"`
	Port            int        `json:"port"`
	SNI             string     `json:"sni"`
	OK              bool       `json:"ok"`
	Reachable       bool       `json:"reachable"`
	LatencyMS       int64      `json:"latency_ms"`
	TLSVersion      string     `json:"tls_version,omitempty"`
	ALPN            string     `json:"alpn,omitempty"`
	HTTP2           bool       `json:"http2"`
	CertValid       bool       `json:"cert_valid"`
	CertSubject     string     `json:"cert_subject,omitempty"`
	CertIssuer      string     `json:"cert_issuer,omitempty"`
	CertDNSNames    []string   `json:"cert_dns_names,omitempty"`
	CertExpiresAt   *time.Time `json:"cert_expires_at,omitempty"`
	CertDaysLeft    int        `json:"cert_days_left,omitempty"`
	HTTPStatus      int        `json:"http_status,omitempty"`
	ServerHeader    string     `json:"server_header,omitempty"`
	RealitySuitable bool       `json:"reality_suitable"`
	Error           string     `json:"error,omitempty"`
	CheckedAt       time.Time  `json:"checked_at"`
}

// NetworkDomainLatencyParams 是 network.domain_latency 的参数。
type NetworkDomainLatencyParams struct {
	Domains     []string `json:"domains"`
	Port        int      `json:"port,omitempty"`
	Mode        string   `json:"mode,omitempty"` // "tls" | "tcp"
	TimeoutMS   int      `json:"timeout_ms,omitempty"`
	Concurrency int      `json:"concurrency,omitempty"`
}

// DomainLatencyItem 描述单个域名的延迟探测结果。
type DomainLatencyItem struct {
	Domain     string `json:"domain"`
	OK         bool   `json:"ok"`
	LatencyMS  int64  `json:"latency_ms,omitempty"`
	TLSVersion string `json:"tls_version,omitempty"`
	ALPN       string `json:"alpn,omitempty"`
	Error      string `json:"error,omitempty"`
}

// NetworkDomainLatencyResult 是 network.domain_latency 的回复。
type NetworkDomainLatencyResult struct {
	Mode        string              `json:"mode"`
	Port        int                 `json:"port"`
	Total       int                 `json:"total"`
	OKCount     int                 `json:"ok_count"`
	FailedCount int                 `json:"failed_count"`
	Results     []DomainLatencyItem `json:"results"`
	Fastest     []DomainLatencyItem `json:"fastest"`
	DurationMS  int64               `json:"duration_ms"`
}

// NetworkReturnRouteTestParams 是 network.return_route_test 的参数。
type NetworkReturnRouteTestParams struct {
	Carrier   string `json:"carrier"`
	TargetIP  string `json:"target_ip,omitempty"`
	Port      int    `json:"port,omitempty"`
	MaxHops   int    `json:"max_hops,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// ReturnRouteHop 描述返程追踪的单跳信息。
type ReturnRouteHop struct {
	Hop       int    `json:"hop"`
	IP        string `json:"ip,omitempty"`
	ASN       string `json:"asn,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Note      string `json:"note,omitempty"`
}

// NetworkReturnRouteTestResult 是 network.return_route_test 的回复。
type NetworkReturnRouteTestResult struct {
	Carrier   string           `json:"carrier"`
	TargetIP  string           `json:"target_ip"`
	RouteType string           `json:"route_type"`
	Region    string           `json:"region,omitempty"`
	EntryIP   string           `json:"entry_ip,omitempty"`
	EntryASN  string           `json:"entry_asn,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	Hops      []ReturnRouteHop `json:"hops,omitempty"`
	TestedAt  time.Time        `json:"tested_at"`
}
