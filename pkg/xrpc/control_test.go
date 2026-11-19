package xrpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 截止时间省略与带上两种都能原样往返；省略时线上没有 deadline 这个键。
func TestLimiterParamsRoundTrip(t *testing.T) {
	deadline := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, in := range []LimiterSetParams{
		{Inbound: "vless-in", Credential: "a", Source: SourceMaster, BPS: 5_000_000},
		{Inbound: "vless-in", Credential: "a", Source: SourceStatic, BPS: 0, Deadline: &deadline},
	} {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "deadline") != (in.Deadline != nil) {
			t.Fatalf("没有截止时间时不应当带 deadline：%s", raw)
		}
		var out LimiterSetParams
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out.Inbound != in.Inbound || out.Credential != in.Credential || out.Source != in.Source || out.BPS != in.BPS ||
			(out.Deadline == nil) != (in.Deadline == nil) || (in.Deadline != nil && !out.Deadline.Equal(*in.Deadline)) {
			t.Fatalf("往返之后不一样：%+v → %s → %+v", in, raw, out)
		}
	}
}

func TestValidSource(t *testing.T) {
	for _, s := range []string{"static", "master", "node_rule"} {
		if !ValidSource(s) {
			t.Fatalf("%s 应当是合法的来源", s)
		}
	}
	for _, s := range []string{"", "boss", "Master", "node-rule"} {
		if ValidSource(s) {
			t.Fatalf("%q 不应当是合法的来源", s)
		}
	}
}

// 快照里内核没在跑时省略版本与启动时间。
func TestTrafficSnapshotRoundTrip(t *testing.T) {
	started := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	in := TrafficSnapshot{BootID: "b1", Generation: 2, At: started.Add(time.Hour), KernelRunning: true, KernelVersion: "1.14.4",
		KernelStartedAt: &started, Counts: []TrafficCount{{Inbound: "in1", Outbound: "out1", Credential: "a", Up: 1, Down: 2}}}
	raw, _ := json.Marshal(in)
	var out TrafficSnapshot
	if err := json.Unmarshal(raw, &out); err != nil || out.BootID != "b1" || out.Generation != 2 || len(out.Counts) != 1 ||
		out.Counts[0] != in.Counts[0] || !out.KernelStartedAt.Equal(started) || out.KernelVersion != "1.14.4" {
		t.Fatalf("往返之后不一样：%s %+v %v", raw, out, err)
	}
	idle, _ := json.Marshal(TrafficSnapshot{BootID: "b1"})
	if strings.Contains(string(idle), "kernel_version") || strings.Contains(string(idle), "kernel_started_at") {
		t.Fatalf("内核没在跑时不应当带版本与启动时间：%s", idle)
	}
}

func TestKernelRestartedRoundTrip(t *testing.T) {
	started := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	for _, in := range []KernelRestarted{
		{Running: true, Version: "1.14.4", StartedAt: &started, Inbounds: []string{"in1", "in2"}},
		{Running: false, Inbounds: []string{}},
	} {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "started_at") != (in.StartedAt != nil) {
			t.Fatalf("started_at 省略与带上不对：%s", raw)
		}
		var out KernelRestarted
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out.Running != in.Running || out.Version != in.Version || len(out.Inbounds) != len(in.Inbounds) ||
			(out.StartedAt == nil) != (in.StartedAt == nil) || (in.StartedAt != nil && !out.StartedAt.Equal(*in.StartedAt)) {
			t.Fatalf("往返之后不一样：%+v → %s → %+v", in, raw, out)
		}
	}
}

func TestTelemetryContractsAndCaps(t *testing.T) {
	if ControlProtocol != 1 {
		t.Fatalf("ControlProtocol 应当保持 1，得到 %d", ControlProtocol)
	}
	if AgentLogsMaxReplyBytes >= ReadLimit {
		t.Fatalf("AgentLogsMaxReplyBytes=%d 必须远小于 ReadLimit=%d", AgentLogsMaxReplyBytes, ReadLimit)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	cfg := &AgentConfig{
		HeartbeatInterval:      15,
		SpeedCollectInterval:   3,
		TrafficCollectInterval: 60,
		AgentLogEnabled:        true,
	}
	authRaw, err := json.Marshal(AuthResult{OK: true, HeartbeatInterval: 15, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	var authOut AuthResult
	if err := json.Unmarshal(authRaw, &authOut); err != nil || !authOut.OK || authOut.HeartbeatInterval != 15 || authOut.Config == nil || !authOut.Config.AgentLogEnabled {
		t.Fatalf("AuthResult 往返失败：%s %+v %v", authRaw, authOut, err)
	}

	speed := SpeedReport{
		UploadSpeed:        102400,
		DownloadSpeed:      204800,
		SystemRxBytes:      999999,
		SystemTxBytes:      888888,
		SystemBootTimeUnix: 1700000000,
		At:                 now,
	}
	httpRPC := AgentHTTPRPC{
		Method:          HTTPRPCMethodSpeed,
		Token:           "tok",
		AgentVersion:    "0.2.0",
		ControlProtocol: ControlProtocol,
		Speed:           &speed,
	}
	rawRPC, err := json.Marshal(httpRPC)
	if err != nil {
		t.Fatal(err)
	}
	var rpcOut AgentHTTPRPC
	if err := json.Unmarshal(rawRPC, &rpcOut); err != nil || rpcOut.Speed == nil || rpcOut.Speed.UploadSpeed != 102400 || rpcOut.Speed.SystemBootTimeUnix != 1700000000 {
		t.Fatalf("AgentHTTPRPC Speed 往返失败：%s %+v %v", rawRPC, rpcOut, err)
	}

	info := SystemInfo{
		Hostname: "hk-1", OS: "Debian 12", Kernel: "6.1.0", Arch: "amd64",
		UptimeSeconds: 3600, CPUModel: "EPYC", CPUCores: 2, CPUThreads: 4, CPUPct: 12.5,
		LoadAvg: "0.10 0.20 0.30", Load1: 0.1, Load5: 0.2, Load15: 0.3,
		MemTotalBytes: 2 << 30, MemUsedBytes: 1 << 30, MemAvailableBytes: 1 << 30,
		DiskTotalBytes: 20 << 30, DiskUsedBytes: 5 << 30, DiskAvailableBytes: 15 << 30,
		TCPFastOpenServer: true, AgentVersion: "0.2.0", InDocker: false, CollectedAt: now,
	}
	infoRaw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var infoOut SystemInfo
	if err := json.Unmarshal(infoRaw, &infoOut); err != nil || infoOut.Hostname != "hk-1" || !infoOut.TCPFastOpenServer || !infoOut.CollectedAt.Equal(now) {
		t.Fatalf("SystemInfo 往返失败：%s %+v %v", infoRaw, infoOut, err)
	}

	logsRes := AgentLogsResult{
		Service: "agent",
		File:    "satchel-agent.log",
		Items:   []AgentLogEntry{{Time: now.Format(time.RFC3339), Level: "DEBUG", Msg: "ok", Attrs: map[string]string{"k": "v"}}},
		Total:   1,
	}
	logsRaw, err := json.Marshal(logsRes)
	if err != nil {
		t.Fatal(err)
	}
	var logsOut AgentLogsResult
	if err := json.Unmarshal(logsRaw, &logsOut); err != nil || len(logsOut.Items) != 1 || logsOut.Items[0].Attrs["k"] != "v" {
		t.Fatalf("AgentLogsResult 往返失败：%s %+v %v", logsRaw, logsOut, err)
	}
}
