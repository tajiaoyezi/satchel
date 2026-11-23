package xrpc

import (
	"slices"
	"strings"
	"time"
)

// 节点固定只读诊断集（10 项），主控与节点共享同一份名称清单。
const (
	DiagItemPorts        = "ports"
	DiagItemServices     = "services"
	DiagItemLogs         = "logs"
	DiagItemConfig       = "config"
	DiagItemConnectivity = "connectivity"
	DiagItemFirewall     = "firewall"
	DiagItemClock        = "clock"
	DiagItemResources    = "resources"
	DiagItemCerts        = "certs"
	DiagItemSpeedtest    = "speedtest"
)

// DiagItems 按展示顺序列出全部 10 项固定诊断项。
var DiagItems = []string{
	DiagItemPorts,
	DiagItemServices,
	DiagItemLogs,
	DiagItemConfig,
	DiagItemConnectivity,
	DiagItemFirewall,
	DiagItemClock,
	DiagItemResources,
	DiagItemCerts,
	DiagItemSpeedtest,
}

// ValidDiagItem 报告 name 是否属于固定诊断项清单。
func ValidDiagItem(name string) bool {
	return slices.Contains(DiagItems, strings.TrimSpace(strings.ToLower(name)))
}

// NormalizeDiagItem 规范化诊断项名称（去首尾空白并转小写）。
func NormalizeDiagItem(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
}

// 诊断集单项状态常量。
const (
	DiagStatusOK           = "ok"
	DiagStatusFailed       = "failed"
	DiagStatusNotAvailable = "not_available"
)

// DiagRunParams 是 diag.run 的参数。Items 为空时默认运行全部诊断项；MasterTimeUnixMS 用于计算节点相对主控的时钟偏差。
type DiagRunParams struct {
	Items            []string `json:"items,omitempty"`
	MasterTimeUnixMS int64    `json:"master_time_unix_ms,omitempty"`
	MasterURL        string   `json:"master_url,omitempty"`
	LogLines         int      `json:"log_lines,omitempty"`
	ItemTimeoutMS    int      `json:"item_timeout_ms,omitempty"`
	IncludeSecrets   bool     `json:"include_secrets,omitempty"`
}

// DiagItemResult 描述单项诊断结果。日志与配置等非信任文本项显式标注 ContentType="data" 与 UntrustedData=true。
type DiagItemResult struct {
	Name          string `json:"name"`
	OK            bool   `json:"ok"`
	Status        string `json:"status"` // "ok" | "failed" | "not_available"
	DurationMS    int64  `json:"duration_ms"`
	ContentType   string `json:"content_type,omitempty"`   // "data" 表示纯数据字段
	UntrustedData bool   `json:"untrusted_data,omitempty"` // true 表示采集自节点日志/配置等非信任输入
	Data          any    `json:"data,omitempty"`
	Error         string `json:"error,omitempty"`
}

// DiagRunResult 是 diag.run 的回复。
type DiagRunResult struct {
	CollectedAt time.Time        `json:"collected_at"`
	NodeTimeUTC time.Time        `json:"node_time_utc"`
	Items       []DiagItemResult `json:"items"`
}
