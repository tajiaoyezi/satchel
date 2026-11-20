package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

func TestConfigTxEndToEndCLIAndAuditPlanID(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		pub := masterPub(t, h)
		tok := h.addServer("hk-tx")
		srv := h.server("hk-tx")

		// 启动一个模拟 WS 节点，响应 config.status / config.test / config.apply / config.files / config.read。
		var mu sync.Mutex
		var curConfig string
		var curHash string
		var gen uint64

		methods := xrpc.Methods{
			xrpc.MethodConfigStatus: func(_ context.Context, _ json.RawMessage) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				now := time.Now().UTC()
				return xrpc.ConfigStatusResult{
					Exists: curConfig != "", Path: "/etc/satchel-agent/kernel.json", Hash: curHash,
					Size: int64(len(curConfig)), Modified: &now, Inbounds: []string{"in-1"}, KernelRunning: curConfig != "", Generation: gen,
				}, nil
			},
			xrpc.MethodConfigTest: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.ConfigTestParams
				_ = json.Unmarshal(raw, &p)
				return xrpc.ConfigTestResult{OK: true, Inbounds: []string{"in-1"}, Hash: xrpc.CanonicalConfigHash([]byte(p.Config))}, nil
			},
			xrpc.MethodConfigApply: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.ConfigApplyParams
				_ = json.Unmarshal(raw, &p)
				mu.Lock()
				defer mu.Unlock()
				if !p.ForceInitial && p.ExpectedCurrentHash != "" && p.ExpectedCurrentHash != curHash {
					return nil, &xrpc.Error{Code: xrpc.CodeConflict, Message: "hash mismatch"}
				}
				curConfig = p.Config
				curHash = xrpc.CanonicalConfigHash([]byte(p.Config))
				gen++
				return xrpc.ConfigApplyResult{
					Applied: true, AppliedHash: curHash, Generation: gen, KernelRunning: true, Inbounds: []string{"in-1"},
				}, nil
			},
			xrpc.MethodConfigFiles: func(_ context.Context, _ json.RawMessage) (any, error) {
				return xrpc.ConfigFilesResult{Files: []xrpc.ConfigFileInfo{
					{Scope: "kernel", Path: "/etc/satchel-agent/kernel.json", Name: "kernel.json", Size: 64, Modified: time.Now().UTC()},
				}}, nil
			},
			xrpc.MethodConfigRead: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.ConfigReadParams
				_ = json.Unmarshal(raw, &p)
				if strings.HasSuffix(p.Path, "/config.yaml") {
					return nil, &xrpc.Error{Code: xrpc.CodeNotFound, Message: "blocked"}
				}
				mu.Lock()
				cfg := curConfig
				hsh := curHash
				mu.Unlock()
				return xrpc.ConfigReadResult{Scope: "kernel", Path: p.Path, Content: cfg, Size: int64(len(cfg)), Modified: time.Now().UTC(), Hash: hsh}, nil
			},
		}

		ag := mustDial(t, h)
		if res := ag.authenticate(pub, tok, xrpc.ControlProtocol); !res.OK {
			t.Fatalf("agent auth failed: %+v", res)
		}
		h.waitStatus("hk-tx", "connected")
		agentCtx, agentCancel := context.WithCancel(context.Background())
		t.Cleanup(agentCancel)
		go func() {
			for {
				m, err := ag.recv(30 * time.Second)
				if err != nil || agentCtx.Err() != nil {
					return
				}
				if m.Type == xrpc.TypeCall {
					var call xrpc.Call
					if xrpc.Decode(m, &call) == nil {
						reply := methods.Serve(agentCtx, call)
						ag.send(xrpc.TypeReply, reply)
					}
				}
			}
		}()

		// 1. CLI --file 本地读入并内联发往主控。
		specPath := filepath.Join(t.TempDir(), "change.json")
		specBytes, _ := json.Marshal([]map[string]any{
			{"op": "update", "kind": "Server", "id": srv.Metadata.ID, "resourceVersion": 1, "spec": map[string]any{"core_log_level": "info"}},
			{"op": "create", "kind": "Inbound", "spec": map[string]any{
				"server_id": srv.Metadata.ID, "tag": "in-1", "protocol": "vless", "port": 8443, "listen": "127.0.0.1", "enabled": true,
				"settings": map[string]any{"users": []map[string]any{{"name": "u1", "uuid": "e2e-uuid-secret"}}},
			}},
		})
		if err := os.WriteFile(specPath, specBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := h.cli("plan", "--file", specPath, "--json")
		if code != 0 {
			t.Fatalf("plan --file failed (%d): %s", code, stderr)
		}
		var planResp struct {
			PlanID int64 `json:"plan_id"`
		}
		if err := json.Unmarshal([]byte(stdout), &planResp); err != nil || planResp.PlanID <= 0 {
			t.Fatalf("plan_id parse failed: %v %s", err, stdout)
		}

		// 2. apply --plan <id> 落地配置。
		stdout, stderr, code = h.cli("apply", "--plan", strconv.FormatInt(planResp.PlanID, 10), "--json")
		if code != 0 {
			t.Fatalf("apply failed (%d): %s", code, stderr)
		}
		var applyResp struct {
			ApplyID string `json:"apply_id"`
		}
		if err := json.Unmarshal([]byte(stdout), &applyResp); err != nil || applyResp.ApplyID == "" {
			t.Fatalf("apply_id missing: %s", stdout)
		}

		// 3. 验证 audit_logs.plan_id 在 plan 和 apply 两条审计记录中都正确写入。
		planAuditCount, err := h.db.NewSelect().TableExpr("audit_logs").
			Where("command IN (?, ?) AND plan_id = ?", "plan", "apply", planResp.PlanID).
			Count(context.Background())
		if err != nil || planAuditCount != 2 {
			t.Fatalf("plan 与 apply 审计记录应当均写入 plan_id=%d，实际命中 %d 条 err=%v", planResp.PlanID, planAuditCount, err)
		}

		// 4. server config status / files / read / test 全部打通。
		stdout, stderr, code = h.cli("server", "config", "status", "hk-tx", "--json")
		if code != 0 || !strings.Contains(stdout, `"in_sync":true`) {
			t.Fatalf("server config status failed (%d): stdout=%s stderr=%s", code, stdout, stderr)
		}
		stdout, stderr, code = h.cli("server", "config", "files", "hk-tx", "--json")
		if code != 0 || !strings.Contains(stdout, `kernel.json`) {
			t.Fatalf("server config files failed (%d): stdout=%s stderr=%s", code, stdout, stderr)
		}
		stdout, stderr, code = h.cli("server", "config", "read", "hk-tx", "--path", "/etc/satchel-agent/kernel.json", "--json")
		if code != 0 || !strings.Contains(stdout, `e2e-uuid-secret`) {
			t.Fatalf("本机管理员(含 secrets) config read 失败 (%d): stdout=%s stderr=%s", code, stdout, stderr)
		}
		if e := h.cliErr("server", "config", "read", "hk-tx", "--path", "/etc/satchel-agent/config.yaml"); e.Code != v1.CodeNotFound {
			t.Fatalf("config.yaml 应报 not_found：%+v", e)
		}
		stdout, stderr, code = h.cli("server", "config", "test", "hk-tx", "--json")
		if code != 0 || !strings.Contains(stdout, `"ok":true`) {
			t.Fatalf("server config test failed (%d): stdout=%s stderr=%s", code, stdout, stderr)
		}

		// 5. snapshot list / get / rollback + apply 收敛。
		stdout, stderr, code = h.cli("snapshot", "list", "--apply-id", applyResp.ApplyID, "--json")
		if code != 0 || !strings.Contains(stdout, `"Server"`) {
			t.Fatalf("snapshot list failed (%d): stdout=%s stderr=%s", code, stdout, stderr)
		}
		stdout, stderr, code = h.cli("snapshot", "rollback", "--apply-id", applyResp.ApplyID, "--json")
		if code != 0 {
			t.Fatalf("snapshot rollback failed (%d): %s", code, stderr)
		}
		var rbPlan struct {
			PlanID int64 `json:"plan_id"`
		}
		if err := json.Unmarshal([]byte(stdout), &rbPlan); err != nil || rbPlan.PlanID <= 0 {
			t.Fatalf("rollback plan_id missing: %s", stdout)
		}
		if _, stderr, code = h.cli("apply", "--plan", strconv.FormatInt(rbPlan.PlanID, 10), "--json"); code != 0 {
			t.Fatalf("rollback apply failed (%d): %s", code, stderr)
		}
		if after := h.server("hk-tx"); after.Spec.CoreLogLevel != "warn" || after.Metadata.ResourceVersion != 3 {
			t.Fatalf("回滚后服务器状态不对：level=%s ver=%d", after.Spec.CoreLogLevel, after.Metadata.ResourceVersion)
		}

		// 6. settings batch-threshold set。
		stdout, stderr, code = h.cli("settings", "batch-threshold", "set", "batch_single_threshold", "12", "--resource-version", "1", "--confirm", "batch_single_threshold", "--json")
		if code != 0 || !strings.Contains(stdout, `"batch_single_threshold":12`) {
			t.Fatalf("settings batch-threshold set failed (%d): stdout=%s stderr=%s", code, stdout, stderr)
		}
	})
}
