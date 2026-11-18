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

func TestLifecycleRPCsAndRuntimeFields(t *testing.T) {
	started := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)

	authRaw, err := json.Marshal(Auth{
		Token:           "tok",
		AgentVersion:    "0.2.0",
		ControlProtocol: ControlProtocol,
		InDocker:        true,
		MachineID:       "mid-123",
		MasterURL:       "https://panel.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	var authGot Auth
	if err := json.Unmarshal(authRaw, &authGot); err != nil {
		t.Fatal(err)
	}
	if !authGot.InDocker || authGot.MachineID != "mid-123" || authGot.MasterURL != "https://panel.example.com" {
		t.Fatalf("Auth 新增字段往返不对：%+v", authGot)
	}

	hbRaw, _ := json.Marshal(Heartbeat{
		BootTime:     started,
		AgentVersion: "0.2.0",
		InDocker:     true,
		MachineID:    "mid-123",
		MasterURL:    "https://panel.example.com",
	})
	var hbGot Heartbeat
	if err := json.Unmarshal(hbRaw, &hbGot); err != nil || hbGot.MachineID != "mid-123" || !hbGot.InDocker {
		t.Fatalf("Heartbeat 新增字段往返不对：%+v %v", hbGot, err)
	}

	// 验证 v0.1.0 旧格式反序列化保持向后兼容。
	var compatAuth Auth
	if err := json.Unmarshal([]byte(`{"token":"t1","agent_version":"0.1.0","control_protocol":1}`), &compatAuth); err != nil {
		t.Fatal(err)
	}
	if compatAuth.InDocker || compatAuth.MachineID != "" || compatAuth.MasterURL != "" {
		t.Fatalf("旧版 Auth 默认值不对：%+v", compatAuth)
	}

	m := Methods{
		MethodAgentInfo: func(context.Context, json.RawMessage) (any, error) {
			return AgentInfo{
				AgentVersion:    "0.2.0",
				ControlProtocol: ControlProtocol,
				InDocker:        true,
				MachineID:       "mid-123",
				MasterURL:       "https://panel.example.com",
				ExecRunning:     true,
			}, nil
		},
		MethodAgentUpdate: func(_ context.Context, p json.RawMessage) (any, error) {
			var in AgentUpdateParams
			if err := json.Unmarshal(p, &in); err != nil {
				return nil, err
			}
			return AgentUpdateAck{
				Accepted:        true,
				PreviousVersion: "0.1.0",
				TargetVersion:   in.Version,
				Source:          "cdn",
				DiskState:       DiskStateNewUnconfirmed,
				BatchContinue:   false,
			}, nil
		},
		MethodAgentUninstall: func(_ context.Context, p json.RawMessage) (any, error) {
			var in AgentUninstallParams
			if err := json.Unmarshal(p, &in); err != nil {
				return nil, err
			}
			return AgentUninstallAck{Accepted: true, ScheduledAt: started, Spawner: "systemd-run"}, nil
		},
		MethodAgentSwitchPorts: func(_ context.Context, p json.RawMessage) (any, error) {
			var in SwitchPortsParams
			if err := json.Unmarshal(p, &in); err != nil {
				return nil, err
			}
			return SwitchPortsAck{ListenPort: in.ListenPort, PullPort: in.PullPort, UpdatedAt: started}, nil
		},
		MethodAgentProbeMaster: func(_ context.Context, p json.RawMessage) (any, error) {
			var in ProbeMasterParams
			if err := json.Unmarshal(p, &in); err != nil {
				return nil, err
			}
			return ProbeMasterResult{URL: in.URL, Reachable: true, LatencyMS: 12}, nil
		},
		MethodAgentUpdateMaster: func(_ context.Context, p json.RawMessage) (any, error) {
			var in UpdateMasterParams
			if err := json.Unmarshal(p, &in); err != nil {
				return nil, err
			}
			return UpdateMasterAck{MasterURL: in.MasterURL, UpdatedAt: started}, nil
		},
	}

	infoReply := m.Serve(context.Background(), Call{ID: "info", Method: MethodAgentInfo})
	if infoReply.Error != nil {
		t.Fatal(infoReply.Error)
	}
	var info AgentInfo
	if err := json.Unmarshal(infoReply.Result, &info); err != nil || !info.ExecRunning || info.MachineID != "mid-123" {
		t.Fatalf("AgentInfo 结果不对：%+v %v", info, err)
	}

	updParams, _ := json.Marshal(AgentUpdateParams{Version: "0.2.0", Channel: "stable", UseCDN: true})
	updReply := m.Serve(context.Background(), Call{ID: "upd", Method: MethodAgentUpdate, Params: updParams})
	if updReply.Error != nil {
		t.Fatal(updReply.Error)
	}
	var updAck AgentUpdateAck
	if err := json.Unmarshal(updReply.Result, &updAck); err != nil || !updAck.Accepted || updAck.DiskState != DiskStateNewUnconfirmed {
		t.Fatalf("AgentUpdateAck 结果不对：%+v %v", updAck, err)
	}

	unParams, _ := json.Marshal(AgentUninstallParams{DetachDelayMS: 1000})
	if reply := m.Serve(context.Background(), Call{ID: "un", Method: MethodAgentUninstall, Params: unParams}); reply.Error != nil {
		t.Fatal(reply.Error)
	}

	spParams, _ := json.Marshal(SwitchPortsParams{ListenPort: 24889, PullPort: 24890})
	spReply := m.Serve(context.Background(), Call{ID: "sp", Method: MethodAgentSwitchPorts, Params: spParams})
	if spReply.Error != nil {
		t.Fatal(spReply.Error)
	}
	var spAck SwitchPortsAck
	if err := json.Unmarshal(spReply.Result, &spAck); err != nil || spAck.ListenPort != 24889 || spAck.PullPort != 24890 {
		t.Fatalf("SwitchPortsAck 结果不对：%+v %v", spAck, err)
	}

	prParams, _ := json.Marshal(ProbeMasterParams{URL: "https://new.example.com", TimeoutMS: 3000})
	prReply := m.Serve(context.Background(), Call{ID: "pr", Method: MethodAgentProbeMaster, Params: prParams})
	if prReply.Error != nil {
		t.Fatal(prReply.Error)
	}
	var prRes ProbeMasterResult
	if err := json.Unmarshal(prReply.Result, &prRes); err != nil || !prRes.Reachable || prRes.LatencyMS != 12 {
		t.Fatalf("ProbeMasterResult 结果不对：%+v %v", prRes, err)
	}

	umParams, _ := json.Marshal(UpdateMasterParams{MasterURL: "https://new.example.com"})
	umReply := m.Serve(context.Background(), Call{ID: "um", Method: MethodAgentUpdateMaster, Params: umParams})
	if umReply.Error != nil {
		t.Fatal(umReply.Error)
	}
	var umAck UpdateMasterAck
	if err := json.Unmarshal(umReply.Result, &umAck); err != nil || umAck.MasterURL != "https://new.example.com" {
		t.Fatalf("UpdateMasterAck 结果不对：%+v %v", umAck, err)
	}
}
