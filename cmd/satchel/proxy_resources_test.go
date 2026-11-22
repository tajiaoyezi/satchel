package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/pkg/xrpc"
)

func TestProxyResourcesEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		pub := masterPub(t, h)
		tok1 := h.addServer("hk-p1")
		tok2 := h.addServer("hk-p2")

		// 启动两个模拟 WS 节点，响应 config.apply 与 outbound.probe。
		startFakeNode := func(tok string) {
			var mu sync.Mutex
			var curHash string
			var gen uint64
			methods := xrpc.Methods{
				xrpc.MethodConfigApply: func(_ context.Context, raw json.RawMessage) (any, error) {
					var p xrpc.ConfigApplyParams
					_ = json.Unmarshal(raw, &p)
					mu.Lock()
					defer mu.Unlock()
					curHash = xrpc.CanonicalConfigHash([]byte(p.Config))
					gen++
					return xrpc.ConfigApplyResult{Applied: true, AppliedHash: curHash, Generation: gen, KernelRunning: true, Inbounds: []string{}}, nil
				},
				xrpc.MethodOutboundProbe: func(_ context.Context, raw json.RawMessage) (any, error) {
					var p xrpc.OutboundProbeParams
					_ = json.Unmarshal(raw, &p)
					return xrpc.OutboundProbeResult{
						Tag: p.Tag, URL: p.URL, OK: true, LatencyMS: 19, StatusCode: 204, ProbedAt: time.Now().UTC(),
					}, nil
				},
			}
			ag := mustDial(t, h)
			if res := ag.authenticate(pub, tok, xrpc.ControlProtocol); !res.OK {
				t.Fatalf("agent auth failed: %+v", res)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() {
				for {
					m, err := ag.recv(30 * time.Second)
					if err != nil || ctx.Err() != nil {
						return
					}
					if m.Type == xrpc.TypeCall {
						var call xrpc.Call
						if xrpc.Decode(m, &call) == nil {
							ag.send(xrpc.TypeReply, methods.Serve(ctx, call))
						}
					}
				}
			}()
		}
		startFakeNode(tok1)
		startFakeNode(tok2)
		h.waitStatus("hk-p1", "connected")
		h.waitStatus("hk-p2", "connected")

		// 初始化一个管理员账号供 routing preset 归属使用。
		if status, fields, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil); status != 200 {
			t.Fatalf("setup init failed: %d %v", status, fields)
		}

		// 1. inbound protocols / examples / keygen / build -> plan -> apply -> inbound get (derived public key)
		if out, stderr, code := h.cli("inbound", "protocols", "--json"); code != 0 || !strings.Contains(out, `"vless"`) {
			t.Fatalf("inbound protocols failed (%d): %s %s", code, out, stderr)
		}
		if out, stderr, code := h.cli("inbound", "examples", "--protocol", "vless", "--json"); code != 0 || !strings.Contains(out, `"vless-reality-vision"`) {
			t.Fatalf("inbound examples failed (%d): %s %s", code, out, stderr)
		}
		if out, stderr, code := h.cli("inbound", "keygen", "--type", "all", "--json"); code != 0 || !strings.Contains(out, `"reality_public_key"`) {
			t.Fatalf("inbound keygen failed (%d): %s %s", code, out, stderr)
		}
		buildOut, stderr, code := h.cli("inbound", "build", "--server-name", "hk-p1", "--protocol", "vless", "--port", "18443", "--security", "reality", "--sni", "www.microsoft.com", "--json")
		if code != 0 {
			t.Fatalf("inbound build failed (%d): %s", code, stderr)
		}
		var buildRes struct {
			Credentials struct {
				RealityPublicKey string `json:"reality_public_key"`
			} `json:"credentials"`
			PlanOp json.RawMessage `json:"plan_op"`
		}
		if err := json.Unmarshal([]byte(buildOut), &buildRes); err != nil || buildRes.Credentials.RealityPublicKey == "" {
			t.Fatalf("parse inbound build failed: %v %s", err, buildOut)
		}
		planOut, stderr, code := h.cli("plan", "--spec", string(buildRes.PlanOp), "--json")
		if code != 0 {
			t.Fatalf("plan from build failed (%d): %s", code, stderr)
		}
		var pv struct {
			PlanID int64 `json:"plan_id"`
		}
		_ = json.Unmarshal([]byte(planOut), &pv)
		if _, stderr, code := h.cli("apply", "--plan", strconv.FormatInt(pv.PlanID, 10), "--json"); code != 0 {
			t.Fatalf("apply failed (%d): %s", code, stderr)
		}
		listOut, stderr, code := h.cli("inbound", "list", "--server-name", "hk-p1", "--json")
		if code != 0 || !strings.Contains(listOut, buildRes.Credentials.RealityPublicKey) {
			t.Fatalf("inbound list should include derived reality_public_key (%d): %s %s", code, listOut, stderr)
		}

		// 2. inbound batch plan + apply -> batch_inbounds + audit_logs.plan_id
		bpOut, stderr, code := h.cli("inbound", "batch", "plan", "--servers", "hk-p1", "--servers", "hk-p2", "--protocol", "trojan", "--port", "28443", "--security", "tls", "--tag", "batch-tr", "--json")
		if code != 0 {
			t.Fatalf("inbound batch plan failed (%d): %s", code, stderr)
		}
		var bpv struct {
			PlanID  int64  `json:"plan_id"`
			BatchID string `json:"batch_id"`
		}
		if err := json.Unmarshal([]byte(bpOut), &bpv); err != nil || bpv.PlanID <= 0 || bpv.BatchID == "" {
			t.Fatalf("bad batch plan response: %s", bpOut)
		}
		if _, stderr, code := h.cli("apply", "--plan", strconv.FormatInt(bpv.PlanID, 10), "--json"); code != 0 {
			t.Fatalf("apply batch inbound failed (%d): %s", code, stderr)
		}
		blOut, stderr, code := h.cli("inbound", "batch", "list", "--batch-id", bpv.BatchID, "--json")
		if code != 0 || !strings.Contains(blOut, `"total":2`) {
			t.Fatalf("inbound batch list expected 2 (%d): %s %s", code, blOut, stderr)
		}
		// 核验 audit_logs.plan_id 在 inbound batch plan 与 apply 两条记录中都存在。
		cnt, err := h.db.NewSelect().TableExpr("audit_logs").
			Where("command IN (?, ?) AND plan_id = ?", "inbound batch plan", "apply", bpv.PlanID).
			Count(context.Background())
		if err != nil || cnt != 2 {
			t.Fatalf("expected 2 audit rows with plan_id=%d, got %d err=%v", bpv.PlanID, cnt, err)
		}

		// 3. outbound landing plan + apply -> outbound probe + routing list
		lpOut, stderr, code := h.cli("outbound", "landing", "plan", "--server-name", "hk-p1", "--outbound-tag", "land-socks", "--inbound-tag", "vless-18443", "--protocol", "socks", "--settings", `{"server":"127.0.0.1","server_port":1080}`, "--json")
		if code != 0 {
			t.Fatalf("outbound landing plan failed (%d): %s", code, stderr)
		}
		var lpv struct {
			PlanID int64 `json:"plan_id"`
		}
		_ = json.Unmarshal([]byte(lpOut), &lpv)
		if _, stderr, code := h.cli("apply", "--plan", strconv.FormatInt(lpv.PlanID, 10), "--json"); code != 0 {
			t.Fatalf("apply landing plan failed (%d): %s", code, stderr)
		}
		obListOut, stderr, code := h.cli("outbound", "list", "--server-name", "hk-p1", "--json")
		if code != 0 {
			t.Fatalf("outbound list failed (%d): %s", code, stderr)
		}
		var obList struct {
			Items []struct {
				Metadata struct {
					ID int64 `json:"id"`
				} `json:"metadata"`
			} `json:"items"`
		}
		_ = json.Unmarshal([]byte(obListOut), &obList)
		if len(obList.Items) != 1 {
			t.Fatalf("expected 1 outbound, got %s", obListOut)
		}
		obID := strconv.FormatInt(obList.Items[0].Metadata.ID, 10)
		if pOut, stderr, code := h.cli("outbound", "probe", obID, "--url", "http://127.0.0.1/204", "--json"); code != 0 || !strings.Contains(pOut, `"ok":true`) {
			t.Fatalf("outbound probe failed (%d): %s %s", code, pOut, stderr)
		}
		if gOut, stderr, code := h.cli("outbound", "get", obID, "--json"); code != 0 || !strings.Contains(gOut, `"last_probe_ok":true`) || !strings.Contains(gOut, `"resourceVersion":1`) {
			t.Fatalf("outbound get after probe mismatch (%d): %s %s", code, gOut, stderr)
		}

		// 4. routing list + routing preset builtin/save/list/delete
		if rOut, stderr, code := h.cli("routing", "list", "--server-name", "hk-p1", "--json"); code != 0 || !strings.Contains(rOut, `"is_inbound_catch_all":true`) {
			t.Fatalf("routing list mismatch (%d): %s %s", code, rOut, stderr)
		}
		if bOut, stderr, code := h.cli("routing", "preset", "builtin", "--json"); code != 0 || !strings.Contains(bOut, `"block-bt"`) {
			t.Fatalf("routing preset builtin mismatch (%d): %s %s", code, bOut, stderr)
		}
		saveOut, stderr, code := h.cli("routing", "preset", "save", "--name", "ex-org", "--rule", `{"domain_suffix":["example.org"]}`, "--json")
		if code != 0 {
			t.Fatalf("routing preset save failed (%d): %s", code, stderr)
		}
		var saved struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(saveOut), &saved)
		pid := strconv.FormatInt(saved.ID, 10)
		if plOut, stderr, code := h.cli("routing", "preset", "list", "--json"); code != 0 || !strings.Contains(plOut, `"ex-org"`) {
			t.Fatalf("routing preset list mismatch (%d): %s %s", code, plOut, stderr)
		}
		if _, stderr, code := h.cli("routing", "preset", "delete", pid, "--confirm", pid, "--json"); code != 0 {
			t.Fatalf("routing preset delete failed (%d): %s", code, stderr)
		}
	})
}
