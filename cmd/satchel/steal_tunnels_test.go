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

func TestStealModeAndTunnelsEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		pub := masterPub(t, h)

		// 创建 4 台服务器：hk-1、jp-1、us-1（远程节点）和 local-1（主控同机节点）。
		tokHK := h.addServer("hk-1")
		tokJP := h.addServer("jp-1")
		tokUS := h.addServer("us-1")
		tokLocal := h.addServer("local-1")

		type fakeAgentState struct {
			mu          sync.Mutex
			configHash  string
			configRaw   string
			generation  uint64
			stealConf   string
			stealExist  bool
			callOrder   []string
			lastProtect bool
		}
		startNode := func(tok string) *fakeAgentState {
			st := &fakeAgentState{}
			methods := xrpc.Methods{
				xrpc.MethodConfigApply: func(_ context.Context, raw json.RawMessage) (any, error) {
					var p xrpc.ConfigApplyParams
					_ = json.Unmarshal(raw, &p)
					st.mu.Lock()
					defer st.mu.Unlock()
					st.callOrder = append(st.callOrder, "config")
					st.lastProtect = p.ProtectMaster443
					if p.ProtectMaster443 && strings.Contains(p.Config, `"listen_port": 443`) {
						return nil, &xrpc.Error{Code: xrpc.CodeConflict, Message: "master 443 protected"}
					}
					prev := st.configHash
					st.configRaw = p.Config
					st.configHash = xrpc.CanonicalConfigHash([]byte(p.Config))
					st.generation++
					return xrpc.ConfigApplyResult{
						Applied:       true,
						AppliedHash:   st.configHash,
						PreviousHash:  prev,
						CurrentHash:   st.configHash,
						Generation:    st.generation,
						KernelRunning: true,
						Inbounds:      []string{},
					}, nil
				},
				xrpc.MethodNginxWebsitesApply: func(_ context.Context, raw json.RawMessage) (any, error) {
					var p xrpc.NginxWebsitesApplyParams
					_ = json.Unmarshal(raw, &p)
					st.mu.Lock()
					defer st.mu.Unlock()
					st.callOrder = append(st.callOrder, "nginx")
					res := xrpc.NginxWebsitesApplyResult{
						Applied:  []xrpc.AppliedWebsiteFile{},
						Removed:  p.RemoveDomains,
						Reloaded: true,
					}
					if p.StealSite != nil {
						st.stealConf = p.StealSite.Conf
						st.stealExist = true
						res.StealApplied = true
						res.StealConfPath = "/etc/nginx/servers/satchel-steal.conf"
					}
					if p.RemoveSteal {
						if st.stealExist {
							st.stealConf = ""
							st.stealExist = false
							res.StealRemoved = true
							res.StealConfPath = "/etc/nginx/servers/satchel-steal.conf"
						}
					}
					return res, nil
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
			return st
		}

		hkState := startNode(tokHK)
		jpState := startNode(tokJP)
		usState := startNode(tokUS)
		_ = startNode(tokLocal)
		for _, n := range []string{"hk-1", "jp-1", "us-1", "local-1"} {
			h.waitStatus(n, "connected")
		}

		// 将 hk-1/jp-1/us-1 设为远程节点并赋予不同可达 IP，local-1 保持 same_host_as_master=true。
		for name, ip := range map[string]string{"hk-1": "198.51.100.11", "jp-1": "198.51.100.12", "us-1": "198.51.100.13"} {
			if _, err := h.db.NewUpdate().TableExpr("servers").
				Set("same_host_as_master = ?", false).
				Set("ip_address = ?", ip).
				Where("name = ?", name).Exec(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.db.NewUpdate().TableExpr("servers").
			Set("same_host_as_master = ?", true).
			Where("name = ?", "local-1").Exec(context.Background()); err != nil {
			t.Fatal(err)
		}

		applyPlan := func(t *testing.T, planJSON string) (applyID string) {
			t.Helper()
			var pv struct {
				PlanID      int64  `json:"plan_id"`
				ConfirmHint string `json:"confirm_hint"`
			}
			if err := json.Unmarshal([]byte(planJSON), &pv); err != nil || pv.PlanID <= 0 {
				t.Fatalf("解析 plan JSON 失败：%v\n%s", err, planJSON)
			}
			args := []string{"apply", "--plan", strconv.FormatInt(pv.PlanID, 10), "--json"}
			if pv.ConfirmHint != "" {
				args = append(args, "--confirm", pv.ConfirmHint)
			}
			out, stderr, code := h.cli(args...)
			if code != 0 {
				t.Fatalf("apply plan %d 失败 (code=%d): %s %s", pv.PlanID, code, out, stderr)
			}
			var ar struct {
				ApplyID string `json:"apply_id"`
			}
			_ = json.Unmarshal([]byte(out), &ar)
			return ar.ApplyID
		}

		// 1. 同机节点 443 内核守卫拦截：server steal plan 与直接 plan 443 入站均报 conflict。
		if out, stderr, code := h.cli("server", "steal", "plan", "local-1", "--mode", "tunnel", "--domain", "decoy.example.com", "--json"); code == 0 || !strings.Contains(out+stderr, `"conflict"`) {
			t.Fatalf("同机节点 server steal plan 应报 conflict，得到 code=%d out=%s stderr=%s", code, out, stderr)
		}
		localID := int64(0)
		if err := h.db.NewSelect().TableExpr("servers").Column("id").Where("name = ?", "local-1").Scan(context.Background(), &localID); err != nil {
			t.Fatal(err)
		}
		if out, stderr, code := h.cli("plan", "--spec", `{"op":"create","kind":"Inbound","spec":{"server_id":`+strconv.FormatInt(localID, 10)+`,"tag":"bad-443","protocol":"direct","port":443,"enabled":true}}`, "--json"); code == 0 || !strings.Contains(out+stderr, `"conflict"`) {
			t.Fatalf("同机节点 plan 443 入站应报 conflict，得到 code=%d out=%s stderr=%s", code, out, stderr)
		}

		// 2. 在 hk-1 建一条 VLESS Reality 入站 + 一条 VLESS WS 入站（均 via inbound build + plan + apply）。
		buildRealOut, stderr, code := h.cli("inbound", "build", "--server-name", "hk-1", "--protocol", "vless", "--port", "18443", "--security", "reality", "--sni", "www.microsoft.com", "--tag", "vless-real", "--json")
		if code != 0 {
			t.Fatalf("inbound build reality 失败：%s %s", buildRealOut, stderr)
		}
		buildWSOut, stderr, code := h.cli("inbound", "build", "--server-name", "hk-1", "--protocol", "vless", "--port", "20001", "--listen", "127.0.0.1", "--transport", "ws", "--path", "/ray", "--security", "none", "--tag", "vless-ws", "--json")
		if code != 0 {
			t.Fatalf("inbound build ws 失败：%s %s", buildWSOut, stderr)
		}
		var bReal, bWS struct {
			PlanOp json.RawMessage `json:"plan_op"`
		}
		_ = json.Unmarshal([]byte(buildRealOut), &bReal)
		_ = json.Unmarshal([]byte(buildWSOut), &bWS)
		initPlanOut, stderr, code := h.cli("plan", "--spec", "["+string(bReal.PlanOp)+","+string(bWS.PlanOp)+"]", "--json")
		if code != 0 {
			t.Fatalf("初始化入站 plan 失败：%s %s", initPlanOut, stderr)
		}
		applyPlan(t, initPlanOut)

		// 3. 偷取模式完整往返：tunnel -> fallback -> tunnel（按原 ID 恢复 tunnel-in 无重名冲突）-> off -> snapshot rollback！
		hkState.mu.Lock()
		hkState.callOrder = nil
		hkState.mu.Unlock()
		tunPlan1, stderr, code := h.cli("server", "steal", "plan", "hk-1", "--mode", "tunnel", "--domain", "decoy.example.com", "--json")
		if code != 0 {
			t.Fatalf("steal plan tunnel 失败：%s %s", tunPlan1, stderr)
		}
		applyPlan(t, tunPlan1)
		hkState.mu.Lock()
		if len(hkState.callOrder) < 2 || hkState.callOrder[0] != "nginx" || hkState.callOrder[1] != "config" {
			t.Fatalf("开启 tunnel 必须先同步 nginx 再推 kernel，实际：%v", hkState.callOrder)
		}
		if !hkState.stealExist || !strings.Contains(hkState.stealConf, "location /ray {") || strings.Contains(hkState.stealConf, "proxy_protocol") {
			t.Fatalf("steal nginx 配置异常：\n%s", hkState.stealConf)
		}
		if !strings.Contains(hkState.configRaw, `"tunnel-in"`) || !strings.Contains(hkState.configRaw, `"override_port": 8001`) {
			t.Fatalf("tunnel kernel.json 异常：\n%s", hkState.configRaw)
		}
		hkState.mu.Unlock()

		// 切 fallback
		fbPlan, stderr, code := h.cli("server", "steal", "plan", "hk-1", "--mode", "fallback", "--json")
		if code != 0 {
			t.Fatalf("steal plan fallback 失败：%s %s", fbPlan, stderr)
		}
		applyPlan(t, fbPlan)
		hkState.mu.Lock()
		if strings.Contains(hkState.configRaw, `"tunnel-in"`) || !strings.Contains(hkState.configRaw, `"server_port": 8001`) {
			t.Fatalf("fallback kernel.json 异常：\n%s", hkState.configRaw)
		}
		hkState.mu.Unlock()

		// 再次切回 tunnel（恢复已软删的 tunnel-in，无 name_taken 冲突！）
		tunPlan2, stderr, code := h.cli("server", "steal", "plan", "hk-1", "--mode", "tunnel", "--json")
		if code != 0 {
			t.Fatalf("再次切回 tunnel 失败：%s %s", tunPlan2, stderr)
		}
		if !strings.Contains(tunPlan2, `"op": "restore"`) && !strings.Contains(tunPlan2, `"op":"restore"`) {
			t.Fatalf("再次切回 tunnel 应恢复已软删 tunnel-in：\n%s", tunPlan2)
		}
		applyPlan(t, tunPlan2)

		// 查 server steal status
		stOut, stderr, code := h.cli("server", "steal", "status", "hk-1", "--json")
		if code != 0 || !strings.Contains(stOut, `"tunnel-in"`) || !strings.Contains(stOut, `satchel-steal.conf`) {
			t.Fatalf("server steal status 异常：%s %s", stOut, stderr)
		}

		// 切 off：先推 kernel 释放 443 再删除 satchel-steal.conf！
		hkState.mu.Lock()
		hkState.callOrder = nil
		hkState.mu.Unlock()
		offPlan, stderr, code := h.cli("server", "steal", "plan", "hk-1", "--mode", "off", "--json")
		if code != 0 {
			t.Fatalf("steal plan off 失败：%s %s", offPlan, stderr)
		}
		offApplyID := applyPlan(t, offPlan)
		hkState.mu.Lock()
		if len(hkState.callOrder) < 2 || hkState.callOrder[0] != "config" || hkState.callOrder[1] != "nginx" || hkState.stealExist {
			t.Fatalf("关闭偷自己应先推 config 再移除 satchel-steal.conf：order=%v exist=%v", hkState.callOrder, hkState.stealExist)
		}
		hkState.mu.Unlock()

		// 对 off 的 apply_id 执行 snapshot rollback -> 恢复 use_443=true 且重新下发 satchel-steal.conf！
		rbOut, stderr, code := h.cli("snapshot", "rollback", "--apply-id", offApplyID, "--json")
		if code != 0 {
			t.Fatalf("snapshot rollback 失败：%s %s", rbOut, stderr)
		}
		applyPlan(t, rbOut)
		hkState.mu.Lock()
		if !hkState.stealExist {
			t.Fatalf("snapshot rollback 后应重新恢复 satchel-steal.conf")
		}
		hkState.mu.Unlock()

		// 4. 任意门单跳 + 复用分流 + list + 按 rule-id / tag 清理。
		tPlan, stderr, code := h.cli("tunnel", "plan", "--server-name", "hk-1", "--listen-port", "23001", "--target-address", "203.0.113.10", "--target-port", "8080", "--json")
		if code != 0 {
			t.Fatalf("tunnel plan 失败：%s %s", tPlan, stderr)
		}
		applyPlan(t, tPlan)

		mPlan, stderr, code := h.cli("tunnel", "multiplex", "plan", "--server-name", "hk-1", "--inbound-tag", "tunnel-23001", "--domain", "svc-b.example.com", "--target-address", "203.0.113.20", "--target-port", "8443", "--json")
		if code != 0 {
			t.Fatalf("tunnel multiplex plan 失败：%s %s", mPlan, stderr)
		}
		applyPlan(t, mPlan)

		tlOut, stderr, code := h.cli("tunnel", "list", "--server-name", "hk-1", "--json")
		if code != 0 || !strings.Contains(tlOut, `"tunnel-23001"`) || !strings.Contains(tlOut, `"svc-b.example.com"`) || strings.Contains(tlOut, `"tunnel-in"`) {
			t.Fatalf("tunnel list 异常：%s %s", tlOut, stderr)
		}
		var tlResp struct {
			Items []struct {
				Kind   string `json:"kind"`
				RuleID int64  `json:"rule_id"`
			} `json:"items"`
		}
		_ = json.Unmarshal([]byte(tlOut), &tlResp)
		var muxRuleID int64
		for _, it := range tlResp.Items {
			if it.Kind == "multiplex" {
				muxRuleID = it.RuleID
			}
		}
		if muxRuleID <= 0 {
			t.Fatalf("未在 tunnel list 找到 multiplex rule_id：%s", tlOut)
		}
		delMuxPlan, stderr, code := h.cli("tunnel", "delete-plan", "--rule-id", strconv.FormatInt(muxRuleID, 10), "--json")
		if code != 0 {
			t.Fatalf("delete-plan --rule-id 失败：%s %s", delMuxPlan, stderr)
		}
		applyPlan(t, delMuxPlan)

		delTunPlan, stderr, code := h.cli("tunnel", "delete-plan", "--server-name", "hk-1", "--tag", "tunnel-23001", "--json")
		if code != 0 {
			t.Fatalf("delete-plan --tag 失败：%s %s", delTunPlan, stderr)
		}
		applyPlan(t, delTunPlan)

		// 5. 三机链式隧道（hk-1 -> jp-1 -> us-1 -> 203.0.113.99:443）：单 Plan 6 对象原子提交 + 整链清理。
		chainPlan, stderr, code := h.cli("tunnel", "chain", "plan", "--label", "hk-jp-us", "--servers", "hk-1", "--servers", "jp-1", "--servers", "us-1", "--entry-port", "25000", "--target-address", "203.0.113.99", "--target-port", "443", "--json")
		if code != 0 {
			t.Fatalf("tunnel chain plan 失败：%s %s", chainPlan, stderr)
		}
		var cp struct {
			AffectedCount int64 `json:"affected_count"`
		}
		_ = json.Unmarshal([]byte(chainPlan), &cp)
		if cp.AffectedCount != 6 {
			t.Fatalf("三机链 Plan 应包含 6 个对象，实际 %d：\n%s", cp.AffectedCount, chainPlan)
		}
		applyPlan(t, chainPlan)

		// 核对三台节点都收到对应链跳配置。
		for name, st := range map[string]*fakeAgentState{"hk-1": hkState, "jp-1": jpState, "us-1": usState} {
			st.mu.Lock()
			if !strings.Contains(st.configRaw, "tunnel-hk-jp-us-h") {
				t.Errorf("节点 %s 未收到链式隧道配置：\n%s", name, st.configRaw)
			}
			st.mu.Unlock()
		}

		// tunnel chain list 展示 1 条 3 跳完整链，且 tunnel list 不包含链跳。
		clOut, stderr, code := h.cli("tunnel", "chain", "list", "--json")
		if code != 0 {
			t.Fatalf("tunnel chain list 失败：%s %s", clOut, stderr)
		}
		var clResp struct {
			Total int `json:"total"`
			Items []struct {
				Label    string `json:"label"`
				HopCount int    `json:"hop_count"`
				Complete bool   `json:"complete"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(clOut), &clResp); err != nil || clResp.Total != 1 || len(clResp.Items) != 1 ||
			clResp.Items[0].Label != "hk-jp-us" || clResp.Items[0].HopCount != 3 || !clResp.Items[0].Complete {
			t.Fatalf("tunnel chain list 异常：%s", clOut)
		}
		if tlAfterChain, _, code := h.cli("tunnel", "list", "--json"); code != 0 || strings.Contains(tlAfterChain, "hk-jp-us") {
			t.Fatalf("tunnel list 不应重复列链式隧道：%s", tlAfterChain)
		}

		// 整链清理（6 对象）。
		delChainPlan, stderr, code := h.cli("tunnel", "chain", "delete-plan", "--label", "hk-jp-us", "--json")
		if code != 0 {
			t.Fatalf("tunnel chain delete-plan 失败：%s %s", delChainPlan, stderr)
		}
		applyPlan(t, delChainPlan)
		clEmpty, stderr, code := h.cli("tunnel", "chain", "list", "--json")
		if code != 0 {
			t.Fatalf("整链清理后查询失败：%s %s", clEmpty, stderr)
		}
		var clEmptyResp struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal([]byte(clEmpty), &clEmptyResp); err != nil || clEmptyResp.Total != 0 {
			t.Fatalf("整链清理后应为空：%s", clEmpty)
		}
	})
}
