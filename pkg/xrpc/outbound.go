package xrpc

import "time"

// DefaultOutboundProbeURL 与超时上下限：供主控与节点共享一致口径。
const (
	DefaultOutboundProbeURL       = "https://www.gstatic.com/generate_204"
	DefaultOutboundProbeTimeoutMS = 10000
	MinOutboundProbeTimeoutMS     = 1000
	MaxOutboundProbeTimeoutMS     = 30000
)

// OutboundProbeParams 是 outbound.probe 的参数：经节点运行中 sing-box 内核的指定出站执行单次 HTTP 探活。
type OutboundProbeParams struct {
	Tag       string `json:"tag"`
	URL       string `json:"url,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// OutboundProbeResult 是 outbound.probe 的回复。
type OutboundProbeResult struct {
	Tag        string    `json:"tag"`
	URL        string    `json:"url"`
	OK         bool      `json:"ok"`
	LatencyMS  int64     `json:"latency_ms"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	ProbedAt   time.Time `json:"probed_at"`
}
