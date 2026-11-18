package xrpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestHTTPPayloadsAndTokenRotateRoundTrip(t *testing.T) {
	started := time.Date(2026, 11, 18, 0, 10, 0, 0, time.UTC)
	rpcReq := AgentHTTPRPC{
		Method:           HTTPRPCMethodTraffic,
		Token:            "tok-secret",
		AgentVersion:     "0.2.0",
		ControlProtocol:  ControlProtocol,
		MinMasterVersion: "0.1.0",
		Heartbeat:        &HTTPHeartbeatPayload{BootTime: started},
		Traffic:          &TrafficSnapshot{BootID: "b1", Generation: 1, At: started},
	}
	raw, err := json.Marshal(rpcReq)
	if err != nil {
		t.Fatal(err)
	}
	var got AgentHTTPRPC
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Method != HTTPRPCMethodTraffic || got.Token != "tok-secret" || got.Heartbeat == nil || !got.Heartbeat.BootTime.Equal(started) || got.Traffic == nil || got.Traffic.BootID != "b1" {
		t.Fatalf("AgentHTTPRPC 往返不一致：%+v", got)
	}

	m := Methods{
		MethodAgentHealth: func(context.Context, json.RawMessage) (any, error) {
			return AgentHealth{Status: "ok", Mode: "pull"}, nil
		},
		MethodTokenRotate: func(_ context.Context, p json.RawMessage) (any, error) {
			var params TokenRotateParams
			if err := json.Unmarshal(p, &params); err != nil {
				return nil, err
			}
			if params.Token != "t2" || params.AgentToken != "a2" || params.PullToken != "p2" {
				t.Fatalf("TokenRotateParams 不对：%+v", params)
			}
			return TokenRotateAck{UpdatedAt: started}, nil
		},
	}
	hReply := m.Serve(context.Background(), Call{ID: "1", Method: MethodAgentHealth})
	if hReply.Error != nil {
		t.Fatalf("agent.health 应当成功：%+v", hReply.Error)
	}
	var hRes AgentHealth
	if err := json.Unmarshal(hReply.Result, &hRes); err != nil || hRes.Status != "ok" || hRes.Mode != "pull" {
		t.Fatalf("agent.health 结果不对：%s %v", hReply.Result, err)
	}

	rotRaw, _ := json.Marshal(TokenRotateParams{Token: "t2", AgentToken: "a2", PullToken: "p2"})
	rReply := m.Serve(context.Background(), Call{ID: "2", Method: MethodTokenRotate, Params: rotRaw})
	if rReply.Error != nil {
		t.Fatalf("token.rotate 应当成功：%+v", rReply.Error)
	}
	var ack TokenRotateAck
	if err := json.Unmarshal(rReply.Result, &ack); err != nil || !ack.UpdatedAt.Equal(started) {
		t.Fatalf("token.rotate 结果不对：%s %v", rReply.Result, err)
	}
}
