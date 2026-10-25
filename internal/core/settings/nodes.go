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
