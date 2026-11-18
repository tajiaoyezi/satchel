package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	svcservers "github.com/satchel/satchel/internal/service/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

func TestNodeLifecycleAndMasterMigrateEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base, pub := h.tcpURL, masterPub(t, h)
		admin := setupAdmin(t, base)
		fullTok := issueToken(t, admin, base, "full")

		// 设初始 master_url 与 Turnstile 密钥（人类管理员会话当场验证）。
		ver := 1
		if status, fields, _ := admin.call("POST", base+"/api/v1/settings/master-url/set", `{"url":"https://old.example.com","resource-version":1,"verify-password":"secret12"}`, nil); status != 200 {
			t.Fatalf("设置初始 master_url 失败：%d %v", status, fields)
		}
		ver++
		if status, fields, _ := admin.call("POST", base+"/api/v1/settings/gates/set", `{"set":{"turnstile_site_key":"0x4AAAAAAATestSiteKey1234","turnstile_secret_key":"0x4AAAAAAATestSecretKey123"},"resource-version":2,"verify-password":"secret12"}`, nil); status != 200 {
			t.Fatalf("设置初始 Turnstile 失败：%d %v", status, fields)
		}
		ver++
		_ = ver

		var mu sync.Mutex
		var switchedPorts []int
		var probed []string
		var updatedMasters []string
		a := mustDial(t, h)
		if !a.authenticate(pub, h.addServer("hk-e2e"), xrpc.ControlProtocol).OK {
			t.Fatal("hk-e2e 应当认证成功")
		}
		a.serveMethods(xrpc.Methods{
			xrpc.MethodAgentInfo: func(context.Context, json.RawMessage) (any, error) {
				return xrpc.AgentInfo{
					AgentVersion:    "0.2.0",
					ControlProtocol: xrpc.ControlProtocol,
					InDocker:        false,
					MasterURL:       "https://old.example.com",
				}, nil
			},
			xrpc.MethodAgentSwitchPorts: func(_ context.Context, p json.RawMessage) (any, error) {
				var in xrpc.SwitchPortsParams
				_ = json.Unmarshal(p, &in)
				mu.Lock()
				switchedPorts = []int{in.ListenPort, in.PullPort}
				mu.Unlock()
				return xrpc.SwitchPortsAck{ListenPort: in.ListenPort, PullPort: in.PullPort, UpdatedAt: time.Now().UTC()}, nil
			},
			xrpc.MethodAgentProbeMaster: func(_ context.Context, p json.RawMessage) (any, error) {
				var in xrpc.ProbeMasterParams
				_ = json.Unmarshal(p, &in)
				mu.Lock()
				probed = append(probed, in.URL)
				mu.Unlock()
				return xrpc.ProbeMasterResult{URL: in.URL, Reachable: true, LatencyMS: 7}, nil
			},
			xrpc.MethodAgentUpdateMaster: func(_ context.Context, p json.RawMessage) (any, error) {
				var in xrpc.UpdateMasterParams
				_ = json.Unmarshal(p, &in)
				mu.Lock()
				updatedMasters = append(updatedMasters, in.MasterURL)
				mu.Unlock()
				return xrpc.UpdateMasterAck{MasterURL: in.MasterURL, UpdatedAt: time.Now().UTC()}, nil
			},
			xrpc.MethodAgentUninstall: func(context.Context, json.RawMessage) (any, error) {
				return xrpc.AgentUninstallAck{Accepted: true, ScheduledAt: time.Now().UTC(), Spawner: "setsid"}, nil
			},
		})

		// 1. CLI: server probe-master-url（合法与 SSRF 阻断）与 server switch-listen-port。
		stdout, stderr, code := h.cli("server", "probe-master-url", "hk-e2e", "--json")
		if code != 0 {
			t.Fatalf("server probe-master-url 失败：%s", stderr)
		}
		var pr svcservers.MasterProbeOutput
		if err := json.Unmarshal([]byte(stdout), &pr); err != nil || !pr.Reachable || pr.URL != "https://old.example.com" {
			t.Fatalf("probe 输出不对：%s %v", stdout, err)
		}
		if e := h.cliErr("server", "probe-master-url", "hk-e2e", "--target", "evil"); e.Code != v1.CodeBadRequest {
			t.Fatalf("非法 target 应当返回 bad_request：%+v", e)
		}

		stdout, stderr, code = h.cli("server", "switch-listen-port", "hk-e2e", "--listen-port", "24889", "--pull-port", "24890", "--confirm", "hk-e2e", "--json")
		if code != 0 {
			t.Fatalf("switch-listen-port 失败：%s", stderr)
		}
		if !strings.Contains(stdout, `"listen_port":24889`) || !strings.Contains(stdout, `"pull_port":24890`) {
			t.Fatalf("switch-listen-port 输出缺少新端口：%s", stdout)
		}
		mu.Lock()
		gotSwitched := append([]int(nil), switchedPorts...)
		mu.Unlock()
		if len(gotSwitched) != 2 || gotSwitched[0] != 24889 || gotSwitched[1] != 24890 {
			t.Fatalf("节点未收到预期端口：%v", gotSwitched)
		}

		// 2. REST Token 调人类专属 settings master-url migrate 被 human_required 拒绝；管理员会话 + verify-password 成功并清 Turnstile。
		if r := send(t, nil, "POST", base+"/api/v1/settings/master-url/migrate", map[string]string{"Authorization": "Bearer " + fullTok}, `{"url":"https://new.example.com","resource-version":3}`); r.status != 403 || r.code() != string(v1.CodeHumanRequired) {
			t.Fatalf("令牌调 master-url migrate 应当 human_required：%d %s", r.status, r.body)
		}
		if status, fields, _ := admin.call("POST", base+"/api/v1/settings/master-url/migrate", `{"url":"https://new.example.com","preview":true,"verify-password":"secret12"}`, nil); status != 200 || string(fields["can_migrate"]) != "true" {
			t.Fatalf("--preview 应当成功且 can_migrate=true：%d %v", status, fields)
		}
		if status, fields, _ := admin.call("POST", base+"/api/v1/settings/master-url/migrate", `{"url":"https://new.example.com","change-domain":true,"resource-version":3,"verify-password":"secret12"}`, nil); status != 200 || string(fields["turnstile_cleared"]) != "true" {
			t.Fatalf("正式迁移应当成功且清空 Turnstile：%d %v", status, fields)
		}
		mu.Lock()
		gotUpdated := append([]string(nil), updatedMasters...)
		mu.Unlock()
		if len(gotUpdated) == 0 || gotUpdated[0] != "https://new.example.com" {
			t.Fatalf("节点应当收到 agent.update_master_url：%v", gotUpdated)
		}

		// 3. server uninstall 成功断连并将节点置 offline。
		stdout, stderr, code = h.cli("server", "uninstall", "hk-e2e", "--confirm", "hk-e2e", "--json")
		if code != 0 || !strings.Contains(stdout, `"uninstalled":true`) {
			t.Fatalf("server uninstall 失败：%d %s %s", code, stderr, stdout)
		}
		h.waitStatus("hk-e2e", "offline")
	})
}
