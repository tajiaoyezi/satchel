package settings

import "time"

// heartbeatIntervalDefault 是 system_config.heartbeat_interval 列的库默认值（tables_ops.go）；列的默认值在 DDL 里、不在默认值表里。
const heartbeatIntervalDefault = 30

// HeartbeatInterval 是节点的心跳间隔（node-channel「心跳与在线状态」）：认证成功时下发给节点。
// 写入时已保证不小于 5；不是正数时（导入或手改的坏值）按默认 30 秒取。
func (st *State) HeartbeatInterval() time.Duration {
	if n, ok := st.Values["heartbeat_interval"].(int64); ok && n >= 1 {
		return time.Duration(n) * time.Second
	}
	return heartbeatIntervalDefault * time.Second
}

// trafficCollectIntervalDefault 是 system_config.traffic_collect_interval 列的库默认值（tables_ops.go）。
const trafficCollectIntervalDefault = 60

// TrafficCollectInterval 是主控向节点拉取计数快照的间隔（node-traffic「定时拉取计数快照」）。
// 写入时已保证不小于 10；不是正数时（导入或手改的坏值）按默认 60 秒取。
func (st *State) TrafficCollectInterval() time.Duration {
	if n, ok := st.Values["traffic_collect_interval"].(int64); ok && n >= 1 {
		return time.Duration(n) * time.Second
	}
	return trafficCollectIntervalDefault * time.Second
}

const (
	speedCollectIntervalDefault = 3
	dashboardRefreshDefaultMS   = 5000
	dashboardRefreshMinMS       = 1000
	dashboardRefreshMaxMS       = 60000
)

// SpeedCollectInterval 是节点实时网速上报与 pull 轮询采集间隔（node-telemetry）。
// 写入时已保证不小于 1；不是正数时按默认 3 秒取。
func (st *State) SpeedCollectInterval() time.Duration {
	if n, ok := st.Values["speed_collect_interval"].(int64); ok && n >= 1 {
		return time.Duration(n) * time.Second
	}
	return speedCollectIntervalDefault * time.Second
}

// AgentLogEnabled 报告节点调试日志（DEBUG 级别）是否开启（node-telemetry）。
func (st *State) AgentLogEnabled() bool {
	if b, ok := st.Values["agent_log_enabled"].(bool); ok {
		return b
	}
	return false
}

// DashboardRefreshIntervalMS 返回仪表盘 WebSocket 刷新周期（毫秒），钳位在 [1000, 60000]，默认 5000。
func (st *State) DashboardRefreshIntervalMS() int {
	if n, ok := st.Values["dashboard_refresh_interval_ms"].(int64); ok {
		if n < dashboardRefreshMinMS {
			return dashboardRefreshMinMS
		}
		if n > dashboardRefreshMaxMS {
			return dashboardRefreshMaxMS
		}
		return int(n)
	}
	return dashboardRefreshDefaultMS
}

// DashboardRefreshInterval 返回仪表盘 WebSocket 刷新周期的 time.Duration 形式。
func (st *State) DashboardRefreshInterval() time.Duration {
	return time.Duration(st.DashboardRefreshIntervalMS()) * time.Millisecond
}
