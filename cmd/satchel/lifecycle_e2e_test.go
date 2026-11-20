package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	svcservers "github.com/satchel/satchel/internal/service/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
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

// TestServerUpdateBatchArrayEndToEnd 端到端验证 REST JSON 字符串数组与 CLI 可重复 --servers：
// 与 satchel-web v0.2.10 发送的 {"servers":["hk-1","a,b"],"channel":"stable","confirm":"2"} 契约严格对齐，
// 确保带逗号服务器名不被拆分，不会错升未选节点，且拒绝旧版逗号拼接字符串。
func TestServerUpdateBatchArrayEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base, pub := h.tcpURL, masterPub(t, h)
		admin := setupAdmin(t, base)
		fullTok := issueToken(t, admin, base, "full")

		feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/cdn/satchel-agent/channels/stable/version.json":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"version":            "0.2.0",
					"control_protocol":   xrpc.ControlProtocol,
					"min_master_version": "0.1.0",
				})
			case "/api/repos/tajiaoyezi/satchel-agent/releases/latest":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v0.2.0",
					"body":     `release <!-- satchel-meta: {"control_protocol":1,"min_master_version":"0.1.0"} -->`,
					"html_url": "https://example.invalid/releases/0.2.0",
				})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(feedSrv.Close)
		h.app.servers.SetReleaseSources(release.Sources{
			CDN:     feedSrv.URL + "/cdn",
			Project: "satchel-agent",
			Repo:    "tajiaoyezi/satchel-agent",
			API:     feedSrv.URL + "/api",
			Web:     feedSrv.URL + "/web",
			Proxy:   feedSrv.URL + "/proxy",
			Client:  feedSrv.Client(),
		})
		h.app.servers.SetUpdatePollTimings(10*time.Millisecond, 2*time.Second)

		var mu sync.Mutex
		versions := map[string]string{
			"hk-1": "0.1.0",
			"a,b":  "0.1.0",
			"a":    "0.1.0",
			"b":    "0.1.0",
		}
		updated := map[string]int{}
		for _, sname := range []string{"hk-1", "a,b", "a", "b"} {
			name := sname
			ag := mustDial(t, h)
			if !ag.authenticate(pub, h.addServer(name), xrpc.ControlProtocol).OK {
				t.Fatalf("%s 应当认证成功", name)
			}
			ag.serveMethods(xrpc.Methods{
				xrpc.MethodAgentInfo: func(context.Context, json.RawMessage) (any, error) {
					mu.Lock()
					ver := versions[name]
					mu.Unlock()
					return xrpc.AgentInfo{AgentVersion: ver, ControlProtocol: xrpc.ControlProtocol}, nil
				},
				xrpc.MethodAgentUpdate: func(_ context.Context, p json.RawMessage) (any, error) {
					var in xrpc.AgentUpdateParams
					_ = json.Unmarshal(p, &in)
					mu.Lock()
					prev := versions[name]
					versions[name] = in.Version
					updated[name]++
					mu.Unlock()
					return xrpc.AgentUpdateAck{Accepted: true, PreviousVersion: prev, TargetVersion: in.Version, Source: "cdn"}, nil
				},
			})
		}

		waitJob := func(jobID string) string {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				r := send(t, nil, "GET", base+"/api/v1/job/get/"+jobID, map[string]string{"Authorization": "Bearer " + fullTok}, "")
				if r.status != 200 {
					t.Fatalf("查 job %s 失败：%d %s", jobID, r.status, r.body)
				}
				if st := str(r.fields["status"]); st == "done" || st == "failed" {
					if st != "done" {
						t.Fatalf("job %s 应当 done，实际 %s：%s", jobID, st, r.body)
					}
					return str(r.fields["output"])
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatalf("等待 job %s 完成超时", jobID)
			return ""
		}

		// 1. 非法请求体均返回 400 bad_request：旧版逗号字符串、空数组、含空白项、--servers 与 --all 共存、两者皆缺。
		authHdr := map[string]string{"Authorization": "Bearer " + fullTok}
		for _, tc := range []struct {
			desc, body string
		}{
			{"旧版字符串", `{"servers":"hk-1,a,b","channel":"stable","confirm":"2"}`},
			{"空数组", `{"servers":[],"channel":"stable","confirm":"0"}`},
			{"含空白项", `{"servers":["hk-1","  "],"channel":"stable","confirm":"2"}`},
			{"servers与all同传", `{"servers":["hk-1"],"all":true,"channel":"stable","confirm":"1"}`},
			{"空servers与all同传", `{"servers":[],"all":true,"channel":"stable","confirm":"1"}`},
			{"两者皆缺", `{"channel":"stable","confirm":"1"}`},
		} {
			r := send(t, nil, "POST", base+"/api/v1/server/update/batch/0.2.0", authHdr, tc.body)
			if r.status != 400 || r.code() != string(v1.CodeBadRequest) {
				t.Fatalf("%s 应当 400 bad_request，得到 %d %s", tc.desc, r.status, r.body)
			}
		}

		// 2. 与 satchel-web v0.2.10 完全一致的 REST 请求体：{"servers":["hk-1","a,b"],"channel":"stable","confirm":"2"}。
		r := send(t, nil, "POST", base+"/api/v1/server/update/batch/0.2.0", authHdr, `{"servers":["hk-1","a,b"],"channel":"stable","confirm":"2"}`)
		if r.status != 200 || str(r.fields["job_id"]) == "" {
			t.Fatalf("REST 批量升级应当 200 并返回 job_id：%d %s", r.status, r.body)
		}
		out := waitJob(str(r.fields["job_id"]))
		if !strings.Contains(out, `"succeeded":2`) || !strings.Contains(out, `"server":"hk-1"`) || !strings.Contains(out, `"server":"a,b"`) {
			t.Fatalf("REST 批量升级结果不对：%s", out)
		}
		mu.Lock()
		if updated["hk-1"] != 1 || updated["a,b"] != 1 || updated["a"] != 0 || updated["b"] != 0 {
			t.Fatalf("REST 批量升级应当只升级 hk-1 与 a,b，实际：%v", updated)
		}
		// 复原 a,b 版本为 0.1.0，测试 CLI 传 --servers a,b --servers a（同时存在 b）。
		versions["a,b"] = "0.1.0"
		mu.Unlock()

		// 3. CLI 可重复 --servers 不拆逗号：升级 a,b 与 a，绝不碰 b。
		stdout, stderr, code := h.cli("server", "update", "batch", "0.2.0", "--servers", "a,b", "--servers", "a", "--confirm", "2", "--json")
		if code != 0 {
			t.Fatalf("CLI 批量升级失败：code=%d stderr=%s stdout=%s", code, stderr, stdout)
		}
		var cliJob struct {
			Status string `json:"status"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal([]byte(stdout), &cliJob); err != nil || cliJob.Status != "done" {
			t.Fatalf("CLI 批量升级返回任务应当 done：%v %s", err, stdout)
		}
		var batchRes svcservers.BatchUpdateResult
		if err := json.Unmarshal([]byte(cliJob.Output), &batchRes); err != nil || batchRes.Succeeded != 2 || len(batchRes.Results) != 2 ||
			batchRes.Results[0].Server != "a,b" || batchRes.Results[1].Server != "a" {
			t.Fatalf("CLI 批量升级结果应当精确包含 a,b 与 a：err=%v res=%+v raw=%s", err, batchRes, cliJob.Output)
		}
		mu.Lock()
		if updated["a,b"] != 2 || updated["a"] != 1 || updated["b"] != 0 {
			t.Fatalf("CLI 批量升级应当只升 a,b 与 a、绝不误升 b，实际：%v", updated)
		}
		mu.Unlock()
	})
}
