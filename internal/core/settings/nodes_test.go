package settings

import (
	"testing"
	"time"
)

func TestNodeTelemetrySettingsGetters(t *testing.T) {
	empty := &State{Values: map[string]any{}}
	if empty.SpeedCollectInterval() != 3*time.Second {
		t.Fatalf("默认 speed_collect_interval 应为 3s，得到 %v", empty.SpeedCollectInterval())
	}
	if empty.AgentLogEnabled() {
		t.Fatal("默认 agent_log_enabled 应为 false")
	}
	if empty.DashboardRefreshIntervalMS() != 5000 || empty.DashboardRefreshInterval() != 5*time.Second {
		t.Fatalf("默认 dashboard_refresh_interval_ms 应为 5000，得到 %d", empty.DashboardRefreshIntervalMS())
	}

	st := &State{Values: map[string]any{
		"speed_collect_interval":        int64(7),
		"agent_log_enabled":             true,
		"dashboard_refresh_interval_ms": int64(2500),
	}}
	if st.SpeedCollectInterval() != 7*time.Second || !st.AgentLogEnabled() || st.DashboardRefreshIntervalMS() != 2500 {
		t.Fatalf("设置读取不对：speed=%v log=%v dash=%d", st.SpeedCollectInterval(), st.AgentLogEnabled(), st.DashboardRefreshIntervalMS())
	}

	clampedLow := &State{Values: map[string]any{"dashboard_refresh_interval_ms": int64(100), "speed_collect_interval": int64(0)}}
	if clampedLow.DashboardRefreshIntervalMS() != 1000 || clampedLow.SpeedCollectInterval() != 3*time.Second {
		t.Fatalf("下限钳位不对：dash=%d speed=%v", clampedLow.DashboardRefreshIntervalMS(), clampedLow.SpeedCollectInterval())
	}
	clampedHigh := &State{Values: map[string]any{"dashboard_refresh_interval_ms": int64(999999)}}
	if clampedHigh.DashboardRefreshIntervalMS() != 60000 {
		t.Fatalf("上限钳位不对：dash=%d", clampedHigh.DashboardRefreshIntervalMS())
	}
}
