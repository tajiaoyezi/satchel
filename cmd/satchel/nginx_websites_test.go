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

func TestNginxWebsitesAndServices(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		pub := masterPub(t, h)
		tokRemote := h.addServer("hk-web1")
		tokLocal := h.addServer("local-web1")

		type nodeState struct {
			mu     sync.Mutex
			sites  map[string]xrpc.ManagedWebsiteFile
			manual []xrpc.ScannedWebsite
		}
		startFakeNginxNode := func(tok string) *nodeState {
			st := &nodeState{
				sites: map[string]xrpc.ManagedWebsiteFile{},
				manual: []xrpc.ScannedWebsite{
					{
						Domain:      "legacy.example.com",
						ConfPath:    "/etc/nginx/servers/manual.conf",
						Managed:     false,
						Type:        "static",
						ListenPorts: []string{"80"},
					},
				},
			}
			methods := xrpc.Methods{
				xrpc.MethodServiceStatus: func(_ context.Context, _ json.RawMessage) (any, error) {
					return xrpc.ServiceStatusResult{
						Kernel:    xrpc.KernelServiceStatus{Running: true, Version: "1.14.4"},
						Nginx:     xrpc.NginxServiceStatus{Installed: true, Running: true, Version: "1.26.2", InDocker: true, InitSystem: "direct", ConfigOK: true},
						Agent:     xrpc.AgentServiceStatus{Running: true, Version: "dev", UptimeSeconds: 60, InDocker: true},
						CheckedAt: time.Now().UTC(),
					}, nil
				},
				xrpc.MethodServiceControl: func(_ context.Context, raw json.RawMessage) (any, error) {
					var p xrpc.ServiceControlParams
					_ = json.Unmarshal(raw, &p)
					return xrpc.ServiceControlResult{
						Service: p.Service,
						Action:  p.Action,
						Running: p.Action != "stop",
						Message: "controlled " + p.Service + " " + p.Action,
						Status: xrpc.ServiceStatusResult{
							Nginx: xrpc.NginxServiceStatus{Installed: true, Running: p.Action != "stop", Version: "1.26.2", InDocker: true, ConfigOK: true},
						},
					}, nil
				},
				xrpc.MethodNginxInstall: func(_ context.Context, _ json.RawMessage) (any, error) {
					return xrpc.NginxInstallResult{
						Installed: true, AlreadyInstalled: true, InDocker: true, Version: "1.26.2",
						Message: "Docker 镜像已预装 nginx",
					}, nil
				},
				xrpc.MethodNginxUninstall: func(_ context.Context, _ json.RawMessage) (any, error) {
					return nil, &xrpc.Error{Code: xrpc.CodeConflict, Message: "Docker 镜像内置 nginx 不支持卸载"}
				},
				xrpc.MethodNginxServers: func(_ context.Context, _ json.RawMessage) (any, error) {
					st.mu.Lock()
					defer st.mu.Unlock()
					blocks := []xrpc.NginxServerBlock{
						{Kind: "http", ConfPath: "/etc/nginx/servers/manual.conf", Managed: false, ServerNames: []string{"legacy.example.com"}, ListenPorts: []string{"80"}},
						{Kind: "stream", ConfPath: "/etc/nginx/stream_servers/master_https_443.conf", Managed: false, ListenPorts: []string{"443"}},
					}
					for d := range st.sites {
						blocks = append(blocks, xrpc.NginxServerBlock{
							Kind: "http", ConfPath: "/etc/nginx/servers/satchel-site-" + d + ".conf", Managed: true, ServerNames: []string{d}, ListenPorts: []string{"80"},
						})
					}
					return xrpc.NginxServersResult{Servers: blocks}, nil
				},
				xrpc.MethodNginxClearStream: func(_ context.Context, raw json.RawMessage) (any, error) {
					var p xrpc.NginxClearStreamParams
					_ = json.Unmarshal(raw, &p)
					if p.Port == 443 && p.ProtectMaster443 {
						return nil, &xrpc.Error{Code: xrpc.CodeConflict, Message: "master 443 protected"}
					}
					return xrpc.NginxClearStreamResult{
						Port: p.Port, RemovedFiles: []string{"/etc/nginx/stream_servers/satchel-stream-" + strconv.Itoa(p.Port) + ".conf"}, Reloaded: true,
					}, nil
				},
				xrpc.MethodNginxWebsitesScan: func(_ context.Context, _ json.RawMessage) (any, error) {
					st.mu.Lock()
					defer st.mu.Unlock()
					out := append([]xrpc.ScannedWebsite{}, st.manual...)
					for d := range st.sites {
						out = append(out, xrpc.ScannedWebsite{
							Domain:      d,
							ConfPath:    "/etc/nginx/servers/satchel-site-" + d + ".conf",
							Managed:     true,
							Type:        "proxy",
							ListenPorts: []string{"80"},
						})
					}
					return xrpc.NginxWebsitesScanResult{Sites: out}, nil
				},
				xrpc.MethodNginxWebsitesApply: func(_ context.Context, raw json.RawMessage) (any, error) {
					var p xrpc.NginxWebsitesApplyParams
					_ = json.Unmarshal(raw, &p)
					st.mu.Lock()
					defer st.mu.Unlock()
					applied := make([]xrpc.AppliedWebsiteFile, 0, len(p.Sites))
					for _, s := range p.Sites {
						st.sites[s.Domain] = s
						applied = append(applied, xrpc.AppliedWebsiteFile{
							Domain:   s.Domain,
							ConfPath: "/etc/nginx/servers/satchel-site-" + s.Domain + ".conf",
						})
					}
					for _, d := range p.RemoveDomains {
						delete(st.sites, d)
					}
					return xrpc.NginxWebsitesApplyResult{
						Applied: applied, Removed: p.RemoveDomains, Reloaded: true,
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
			return st
		}

		startFakeNginxNode(tokRemote)
		startFakeNginxNode(tokLocal)
		h.waitStatus("hk-web1", "connected")
		h.waitStatus("local-web1", "connected")

		// 将 hk-web1 标为远程节点，local-web1 保持同机节点（same_host_as_master=true）。
		if _, err := h.db.NewUpdate().TableExpr("servers").Set("same_host_as_master = ?", false).Where("name = ?", "hk-web1").Exec(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.NewUpdate().TableExpr("servers").Set("same_host_as_master = ?", true).Where("name = ?", "local-web1").Exec(context.Background()); err != nil {
			t.Fatal(err)
		}

		// 初始化管理员并通过浏览器会话登录，同时签发一把全权 API 令牌用于验证远程放行 + 同机拦截。
		br := newBrowser(t)
		if st, f, _ := br.call("POST", h.tcpURL+"/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil); st != 200 {
			t.Fatalf("setup init failed: %d %v", st, f)
		}
		if st, f := br.login(h.tcpURL, "admin", "secret12", ""); st != 200 {
			t.Fatalf("login failed: %d %v", st, f)
		}
		stCode, tokBody, _ := br.call("POST", h.tcpURL+"/api/v1/token/create", `{"name":"full-bot","preset":"full","verify-password":"secret12"}`, nil)
		if stCode != 200 {
			t.Fatalf("token create failed: %d %v", stCode, tokBody)
		}
		apiToken := jsonStr(tokBody["token"])
		if apiToken == "" {
			t.Fatalf("missing token in response: %v", tokBody)
		}
		authHeader := map[string]string{"Authorization": "Bearer " + apiToken}

		// 1. 远程节点：API 令牌通过 REST 直接查服务状态、重载 nginx、安装（Docker 预装）、巡检 servers、清理 stream 8443。
		if code, body, _ := newBrowser(t).call("GET", h.tcpURL+"/api/v1/server/service/status/hk-web1", "", authHeader); code != 200 {
			t.Fatalf("remote service status failed: %d %v", code, body)
		}
		if code, body, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/server/service/control/hk-web1", `{"service":"nginx","action":"reload","confirm":"hk-web1"}`, authHeader); code != 200 {
			t.Fatalf("remote token nginx reload failed: %d %v", code, body)
		}
		if code, body, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/server/nginx/install/hk-web1", `{"no-wait":false}`, authHeader); code != 200 && code != 202 {
			t.Fatalf("remote token nginx install failed: %d %v", code, body)
		}
		if code, body, _ := newBrowser(t).call("GET", h.tcpURL+"/api/v1/server/nginx/servers/hk-web1", "", authHeader); code != 200 {
			t.Fatalf("remote nginx servers failed: %d %v", code, body)
		}
		if code, body, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/server/nginx/clear-stream/hk-web1", `{"port":8443,"confirm":"hk-web1"}`, authHeader); code != 200 {
			t.Fatalf("remote clear-stream failed: %d %v", code, body)
		}

		// 2. 远程节点：website templates / build (WSS) -> plan -> apply -> website scan (区分已纳管与未纳管，不抬版本) -> snapshot rollback。
		if out, stderr, code := h.cli("website", "templates", "--json"); code != 0 || !strings.Contains(out, `"wss-proxy"`) {
			t.Fatalf("website templates failed (%d): %s %s", code, out, stderr)
		}
		buildOut, stderr, code := h.cli("website", "build", "--server-name", "hk-web1", "--domain", "app.example.com", "--type", "proxy", "--target", "http://127.0.0.1:10000", "--wss-path", "/ws", "--json")
		if code != 0 {
			t.Fatalf("website build failed (%d): %s", code, stderr)
		}
		var bv struct {
			PlanOp json.RawMessage `json:"plan_op"`
		}
		_ = json.Unmarshal([]byte(buildOut), &bv)

		planCode, planBody, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/plan", jsonBody(map[string]any{"spec": string(bv.PlanOp)}), authHeader)
		if planCode != 200 {
			t.Fatalf("remote website plan failed: %d %v", planCode, planBody)
		}
		planID := jsonInt64(planBody["plan_id"])
		applyCode, applyBody, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/apply", jsonBody(map[string]any{"plan": planID}), authHeader)
		if applyCode != 200 {
			t.Fatalf("remote token website apply failed: %d %v", applyCode, applyBody)
		}

		// website scan: managed + unmanaged, resourceVersion 保持 1
		scanOut, stderr, code := h.cli("website", "scan", "hk-web1", "--json")
		if code != 0 || !strings.Contains(scanOut, `"app.example.com"`) || !strings.Contains(scanOut, `"legacy.example.com"`) {
			t.Fatalf("website scan failed (%d): %s %s", code, scanOut, stderr)
		}
		listOut, stderr, code := h.cli("website", "list", "--server-name", "hk-web1", "--json")
		if code != 0 || !strings.Contains(listOut, `"total":1`) || !strings.Contains(listOut, `"resourceVersion":1`) {
			t.Fatalf("website list mismatch (%d): %s %s", code, listOut, stderr)
		}

		// 3. 同机节点（same_host_as_master=true）：API 令牌在 control nginx / nginx install / apply Website 时全部返回 human_required！
		if code, body, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/server/service/control/local-web1", `{"service":"nginx","action":"reload","confirm":"local-web1"}`, authHeader); code != 403 || jsonStr(body["code"]) != "human_required" {
			t.Fatalf("same-host token nginx control expected 403 human_required, got %d %v", code, body)
		}
		beforeJobCnt, _ := h.db.NewSelect().TableExpr("jobs").Count(context.Background())
		if code, body, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/server/nginx/install/local-web1", `{}`, authHeader); code != 403 || jsonStr(body["code"]) != "human_required" {
			t.Fatalf("same-host token nginx install expected 403 human_required, got %d %v", code, body)
		}
		afterJobCnt, _ := h.db.NewSelect().TableExpr("jobs").Count(context.Background())
		if afterJobCnt != beforeJobCnt {
			t.Fatalf("rejected same-host nginx install must not create job row (%d -> %d)", beforeJobCnt, afterJobCnt)
		}

		// 同机 Website：build -> plan (requires_human=true) -> token apply (human_required) -> admin 无密 apply (human_required) -> admin 带密 apply (200) -> 下一笔不带密再次 human_required（一次性无票据）
		localBuildOut, stderr, code := h.cli("website", "build", "--server-name", "local-web1", "--domain", "decoy.example.com", "--type", "decoy", "--target", "landing", "--json")
		if code != 0 {
			t.Fatalf("local website build failed (%d): %s", code, stderr)
		}
		var lbv struct {
			PlanOp json.RawMessage `json:"plan_op"`
		}
		_ = json.Unmarshal([]byte(localBuildOut), &lbv)

		lpCode, lpBody, _ := br.call("POST", h.tcpURL+"/api/v1/plan", jsonBody(map[string]any{"spec": string(lbv.PlanOp)}), nil)
		if lpCode != 200 || !jsonBool(lpBody["requires_human"]) {
			t.Fatalf("same-host website plan should set requires_human=true: %d %v", lpCode, lpBody)
		}
		localPlanID := jsonInt64(lpBody["plan_id"])

		if code, body, _ := newBrowser(t).call("POST", h.tcpURL+"/api/v1/apply", jsonBody(map[string]any{"plan": localPlanID}), authHeader); code != 403 || jsonStr(body["code"]) != "human_required" {
			t.Fatalf("token apply on same-host website expected human_required, got %d %v", code, body)
		}
		if code, body, _ := br.call("POST", h.tcpURL+"/api/v1/apply", jsonBody(map[string]any{"plan": localPlanID}), nil); code != 403 || jsonStr(body["code"]) != "human_required" {
			t.Fatalf("session apply without verify-password expected human_required, got %d %v", code, body)
		}
		if code, body, _ := br.call("POST", h.tcpURL+"/api/v1/apply", jsonBody(map[string]any{"plan": localPlanID, "verify-password": "secret12"}), nil); code != 200 {
			t.Fatalf("verified session apply on same-host website failed: %d %v", code, body)
		}
		// 一次性消费：紧接着不带 verify-password 调同机 nginx control 仍被 human_required 拦截。
		if code, body, _ := br.call("POST", h.tcpURL+"/api/v1/server/service/control/local-web1", `{"service":"nginx","action":"reload","confirm":"local-web1"}`, nil); code != 403 || jsonStr(body["code"]) != "human_required" {
			t.Fatalf("next call without verify-password must still require human verification, got %d %v", code, body)
		}
		// 带 verify-password 成功。
		if code, body, _ := br.call("POST", h.tcpURL+"/api/v1/server/service/control/local-web1", `{"service":"nginx","action":"reload","confirm":"local-web1","verify-password":"secret12"}`, nil); code != 200 {
			t.Fatalf("verified session same-host nginx control failed: %d %v", code, body)
		}

		// 审计记录绝不泄漏 verify-password 原文。
		leakCnt, err := h.db.NewSelect().TableExpr("audit_logs").Where("args_digest LIKE ?", "%secret12%").Count(context.Background())
		if err != nil || leakCnt != 0 {
			t.Fatalf("audit_logs must never contain verify-password plaintext: leakCnt=%d err=%v", leakCnt, err)
		}
	})
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonStr(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func jsonInt64(raw json.RawMessage) int64 {
	var n int64
	_ = json.Unmarshal(raw, &n)
	return n
}

func jsonBool(raw json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}
