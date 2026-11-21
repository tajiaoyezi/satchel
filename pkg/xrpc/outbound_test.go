package xrpc

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOutboundProbePayloadRoundTrip(t *testing.T) {
	p := OutboundProbeParams{
		Tag:       "land-jp",
		URL:       DefaultOutboundProbeURL,
		TimeoutMS: DefaultOutboundProbeTimeoutMS,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var gotP OutboundProbeParams
	if err := json.Unmarshal(raw, &gotP); err != nil {
		t.Fatal(err)
	}
	if gotP != p {
		t.Fatalf("params mismatch: got %+v want %+v", gotP, p)
	}

	now := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	r := OutboundProbeResult{
		Tag:        "land-jp",
		URL:        DefaultOutboundProbeURL,
		OK:         true,
		LatencyMS:  42,
		StatusCode: 204,
		ProbedAt:   now,
	}
	rawR, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var gotR OutboundProbeResult
	if err := json.Unmarshal(rawR, &gotR); err != nil {
		t.Fatal(err)
	}
	if gotR.Tag != r.Tag || !gotR.OK || gotR.LatencyMS != 42 || gotR.StatusCode != 204 || !gotR.ProbedAt.Equal(now) {
		t.Fatalf("result mismatch: got %+v want %+v", gotR, r)
	}
	if MethodOutboundProbe != "outbound.probe" {
		t.Fatalf("unexpected method constant: %q", MethodOutboundProbe)
	}
}
