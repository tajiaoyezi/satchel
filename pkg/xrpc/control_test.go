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
