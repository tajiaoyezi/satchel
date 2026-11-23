package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/service/diag"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

func dialAgentWithIP(t *testing.T, h *harness, realIP string) *fakeAgent {
	t.Helper()
	hdr := http.Header{"User-Agent": {"satchel-agent/0.2.0-test"}}
	if realIP != "" {
		hdr.Set("X-Real-IP", realIP)
	}
	ws, _, err := websocket.Dial(tctx(t), wsURL(h), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &fakeAgent{t: t, ws: ws}
}

func authAgentFull(a *fakeAgent, pub ed25519.PublicKey, token, machineID string, execEnabled bool) xrpc.AuthResult {
	a.t.Helper()
	a.handshake(pub)
	a.send(xrpc.TypeAuth, xrpc.Auth{
		Token:           token,
		AgentVersion:    "0.2.0-test",
		ControlProtocol: xrpc.ControlProtocol,
		MachineID:       machineID,
		ExecEnabled:     execEnabled,
	})
	m, err := a.recv(5 * time.Second)
	if err != nil {
		a.t.Fatalf("读认证结果失败：%v", err)
	}
	var res xrpc.AuthResult
	if m.Type != xrpc.TypeAuthResult || xrpc.Decode(m, &res) != nil {
		a.t.Fatalf("认证结果不对：%s", m.Type)
	}
	return res
}

func TestDiagExecAndEvidenceEndToEnd(t *testing.T) {
	origMID := hostMachineID
	hostMachineID = func(_ ...string) string { return "master-mid-e2e" }
	t.Cleanup(func() { hostMachineID = origMID })

	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		pub := masterPub(t, h)

		// 先初始化管理员账号，再配置信任回环代理与签发令牌。
		alice := newBrowser(t)
		if status, fields, _ := alice.call("POST", h.tcpURL+"/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil); status != 200 {
			t.Fatalf("setup init 失败：%d %v", status, fields)
		}
		h.gates(`trusted_proxies=[{"cidr":"127.0.0.1/32","header":"X-Real-IP"}]`)

		// 核对内置定时任务总数保持 SQLite 12 / PostgreSQL 11 不变。
		schedOut, stderr, code := h.cli("schedule", "list", "--json")
		if code != 0 {
			t.Fatalf("schedule list 失败 (%d): %s %s", code, schedOut, stderr)
		}
		var schedPage struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(schedOut), &schedPage); err != nil {
			t.Fatal(err)
		}
		wantTasks := 11
		if db.DialectOf(bdb) == schema.SQLite {
			wantTasks = 12
		}
		if len(schedPage.Items) != wantTasks {
			t.Fatalf("内置定时任务数量应保持 %d，实际 %d", wantTasks, len(schedPage.Items))
		}

		// 准备 5 台测试节点：远端开启 exec、远端未开 exec、同机回环、缺 machine_id、离线节点。
		tokRemote := h.addServer("hk-diag")
		tokNoExec := h.addServer("hk-noexec")
		tokLoop := h.addServer("local-same")
		tokNoMid := h.addServer("hk-nomid")
		_ = h.addServer("hk-offline")

		// 签发两把 API 令牌：只读管理员令牌（无 secrets、无 exec）与开启 exec 危险类的令牌。
		issueTok := func(name string, extra ...string) string {
			args := append([]string{"token", "create", "--name", name}, extra...)
			args = append(args, "--verify-user", "admin", "--json")
			out, stderr, code := h.cliPrompt([]string{"secret12", ""}, args...)
			if code != 0 {
				t.Fatalf("签发令牌 %s 失败 (%d): %s %s", name, code, out, stderr)
			}
			var tok createdToken
			decodeInto(t, out, &tok)
			return tok.Token
		}
		roTok := issueTok("ro-diag", "--preset", "readonly")
		execTok := issueTok("op-exec", "--preset", "ops", "--danger", "exec")

		var (
			mu          sync.Mutex
			execResults = map[string]xrpc.ExecStatusResult{}
			agRemote    *fakeAgent
			agRemoteCtx context.Context
			agRemoteCan context.CancelFunc
			holdStart   = make(chan string, 4)
			holdRelease chan struct{}
			dropMidExec bool
		)

		serveFakeRemote := func(ag *fakeAgent) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				for {
					m, err := ag.recv(30 * time.Second)
					if err != nil || ctx.Err() != nil {
						return
					}
					if m.Type != xrpc.TypeCall {
						continue
					}
					var call xrpc.Call
					if xrpc.Decode(m, &call) != nil {
						continue
					}
					switch call.Method {
					case xrpc.MethodDiagRun:
						var p xrpc.DiagRunParams
						_ = json.Unmarshal(call.Params, &p)
						want := p.Items
						if len(want) == 0 {
							want = xrpc.DiagItems
						}
						items := make([]xrpc.DiagItemResult, 0, len(want))
						for _, name := range want {
							switch name {
							case xrpc.DiagItemConfig:
								items = append(items, xrpc.DiagItemResult{
									Name: xrpc.DiagItemConfig, OK: true, Status: xrpc.DiagStatusOK,
									ContentType: "data", UntrustedData: true,
									Data: map[string]any{"private_key": "raw-node-privkey-123", "token": "sat_nodecfg12345678", "listen_port": 443},
								})
							case xrpc.DiagItemLogs:
								items = append(items, xrpc.DiagItemResult{
									Name: xrpc.DiagItemLogs, OK: true, Status: xrpc.DiagStatusOK,
									ContentType: "data", UntrustedData: true,
									Data: map[string]any{"lines": strings.Repeat("log line token=sat_logsecret9999\n", 6000)},
								})
							case xrpc.DiagItemSpeedtest:
								items = append(items, xrpc.DiagItemResult{
									Name: xrpc.DiagItemSpeedtest, OK: true, Status: xrpc.DiagStatusNotAvailable,
									Data: map[string]any{"message": "测速诊断项将在 M7 接入"},
								})
							default:
								items = append(items, xrpc.DiagItemResult{
									Name: name, OK: true, Status: xrpc.DiagStatusOK,
									Data: map[string]any{"item": name, "ok": true},
								})
							}
						}
						rep := xrpc.Methods{
							xrpc.MethodDiagRun: func(context.Context, json.RawMessage) (any, error) {
								return xrpc.DiagRunResult{
									CollectedAt: time.Now().UTC(),
									NodeTimeUTC: time.Now().UTC(),
									Items:       items,
								}, nil
							},
						}.Serve(ctx, call)
						ag.send(xrpc.TypeReply, rep)

					case xrpc.MethodExecStart:
						var p xrpc.ExecStartParams
						_ = json.Unmarshal(call.Params, &p)
						rep := xrpc.Methods{
							xrpc.MethodExecStart: func(context.Context, json.RawMessage) (any, error) {
								return xrpc.ExecStartResult{JobID: p.JobID, Accepted: true, PGID: 9999, StartedAt: time.Now().UTC()}, nil
							},
						}.Serve(ctx, call)
						ag.send(xrpc.TypeReply, rep)

						mu.Lock()
						rel := holdRelease
						shouldDrop := dropMidExec
						mu.Unlock()

						go func(jobID, cmdStr string, timeoutMS int) {
							if rel != nil {
								holdStart <- jobID
								<-rel
							}
							if shouldDrop {
								part := "before-drop\n"
								ag.send(xrpc.TypeExecEvent, xrpc.ExecEvent{JobID: jobID, Seq: 1, Chunk: part, Offset: len(part)})
								time.Sleep(40 * time.Millisecond)
								mu.Lock()
								execResults[jobID] = xrpc.ExecStatusResult{
									JobID: jobID, Found: true, State: xrpc.ExecStateDone, Done: true,
									ExitCode: 0, Output: "before-drop\nafter-reconnect\n",
								}
								mu.Unlock()
								_ = ag.ws.CloseNow()
								return
							}
							// 真实执行 /bin/sh -c 含 Setpgid 与超时查杀，验证无孤儿进程。
							cmd := exec.Command("/bin/sh", "-c", cmdStr)
							cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
							var buf bytes.Buffer
							cmd.Stdout = &buf
							cmd.Stderr = &buf
							_ = cmd.Start()
							doneCh := make(chan error, 1)
							go func() { doneCh <- cmd.Wait() }()
							state := xrpc.ExecStateDone
							exitCode := 0
							select {
							case werr := <-doneCh:
								if werr != nil {
									if ee, ok := werr.(*exec.ExitError); ok {
										exitCode = ee.ExitCode()
									} else {
										exitCode = 1
									}
									state = xrpc.ExecStateFailed
								}
							case <-time.After(time.Duration(timeoutMS) * time.Millisecond):
								if cmd.Process != nil {
									_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
								}
								<-doneCh
								state = xrpc.ExecStateTimedOut
								exitCode = -1
							}
							outStr := buf.String()
							mu.Lock()
							execResults[jobID] = xrpc.ExecStatusResult{
								JobID: jobID, Found: true, State: state, Done: true,
								ExitCode: exitCode, Output: outStr,
							}
							mu.Unlock()
							if outStr != "" {
								ag.send(xrpc.TypeExecEvent, xrpc.ExecEvent{JobID: jobID, Seq: 1, Chunk: outStr, Offset: len(outStr)})
							}
							ag.send(xrpc.TypeExecEvent, xrpc.ExecEvent{
								JobID: jobID, Seq: 2, Offset: len(outStr), Done: true, State: state, ExitCode: exitCode,
							})
						}(p.JobID, p.Command, p.TimeoutMS)

					case xrpc.MethodExecStatus:
						var p xrpc.ExecStatusParams
						_ = json.Unmarshal(call.Params, &p)
						mu.Lock()
						st, ok := execResults[p.JobID]
						mu.Unlock()
						if !ok {
							st = xrpc.ExecStatusResult{JobID: p.JobID, Found: false}
						}
						rep := xrpc.Methods{
							xrpc.MethodExecStatus: func(context.Context, json.RawMessage) (any, error) {
								return st, nil
							},
						}.Serve(ctx, call)
						ag.send(xrpc.TypeReply, rep)
					}
				}
			}()
			return ctx, cancel
		}

		// 连接全部在线模拟节点
		agRemote = dialAgentWithIP(t, h, "203.0.113.50")
		if res := authAgentFull(agRemote, pub, tokRemote, "node-mid-remote-50", true); !res.OK {
			t.Fatalf("remote auth 失败：%+v", res)
		}
		agRemoteCtx, agRemoteCan = serveFakeRemote(agRemote)
		t.Cleanup(func() {
			if agRemoteCan != nil {
				agRemoteCan()
			}
		})
		_ = agRemoteCtx

		agNoExec := dialAgentWithIP(t, h, "203.0.113.51")
		if res := authAgentFull(agNoExec, pub, tokNoExec, "node-mid-noexec-51", false); !res.OK {
			t.Fatalf("noexec auth 失败：%+v", res)
		}
		agLoop := dialAgentWithIP(t, h, "") // 回环 127.0.0.1
		if res := authAgentFull(agLoop, pub, tokLoop, "node-mid-loop", true); !res.OK {
			t.Fatalf("loop auth 失败：%+v", res)
		}
		agNoMid := dialAgentWithIP(t, h, "203.0.113.53")
		if res := authAgentFull(agNoMid, pub, tokNoMid, "", true); !res.OK {
			t.Fatalf("nomid auth 失败：%+v", res)
		}

		// === 1. server diag：全量、单项与只读令牌打码 ===
		diagOut, stderr, code := h.cli("--server", h.tcpURL, "--token", roTok, "server", "diag", "hk-diag", "--json")
		if code != 0 {
			t.Fatalf("只读管理员令牌 server diag 失败 (%d): %s %s", code, diagOut, stderr)
		}
		if strings.Contains(diagOut, "raw-node-privkey-123") || strings.Contains(diagOut, "sat_nodecfg12345678") {
			t.Fatalf("只读令牌（无 secrets）的 server diag 未对 config 打码：%s", diagOut)
		}
		if !strings.Contains(diagOut, `"status":"not_available"`) {
			t.Fatalf("全量 server diag 应含 speedtest not_available：%s", diagOut)
		}
		singleOut, stderr, code := h.cli("server", "diag", "hk-diag", "ports", "--json")
		if code != 0 || !strings.Contains(singleOut, `"name":"ports"`) {
			t.Fatalf("单项 server diag ports 失败 (%d): %s %s", code, singleOut, stderr)
		}
		if _, _, code := h.cli("server", "diag", "hk-diag", "invalid-item", "--json"); code == 0 {
			t.Fatal("未知诊断项应失败")
		}

		// === 2. server exec 四重闸门（confirm / exec 危险类 / 同机 fail-closed / 本地开关 / 离线） ===
		// 缺 --confirm 返回退出码 7（confirm_required）
		if _, errOut, code := h.cli("server", "exec", "hk-diag", "--json", "--", "echo", "hi"); code != v1.ExitConfirmRequired || !strings.Contains(errOut, "confirm_required") {
			t.Fatalf("缺 --confirm 应返回 7/confirm_required，得到 code=%d err=%s", code, errOut)
		}
		// 无 exec 危险类的令牌返回 forbidden
		if _, errOut, code := h.cli("--server", h.tcpURL, "--token", roTok, "server", "exec", "hk-diag", "--confirm", "hk-diag", "--json", "--", "echo", "hi"); code == 0 || !strings.Contains(errOut, "forbidden") {
			t.Fatalf("无 exec 危险类令牌应报 forbidden：code=%d err=%s", code, errOut)
		}
		// 同机回环 / 缺 machine_id / 未开本地开关 / 离线节点均拒绝
		for _, tc := range []struct {
			srv  string
			want string
		}{
			{"local-same", "forbidden"},
			{"hk-nomid", "forbidden"},
			{"hk-noexec", "forbidden"},
			{"hk-offline", "unavailable"},
		} {
			if _, errOut, code := h.cli("--server", h.tcpURL, "--token", execTok, "server", "exec", tc.srv, "--confirm", tc.srv, "--json", "--", "echo", "hi"); code == 0 || !strings.Contains(errOut, tc.want) {
				t.Errorf("对 %s 执行 server exec 期望 %s，得到 code=%d err=%s", tc.srv, tc.want, code, errOut)
			}
		}

		// === 3. 远程 server exec：CLI -- 透传 + REST JSON + 审计打码 + 超时杀进程组 ===
		execOut, stderr, code := h.cli("--server", h.tcpURL, "--token", execTok, "server", "exec", "hk-diag", "--confirm", "hk-diag", "--json", "--", "echo hello-m221 token=sat_execsecret888")
		if code != 0 || !strings.Contains(execOut, `"status":"done"`) || !strings.Contains(execOut, "hello-m221") {
			t.Fatalf("CLI -- 透传执行失败 (%d): %s %s", code, execOut, stderr)
		}
		// 核对 audit list 中 full_command 与打码后的 output
		audOut, stderr, code := h.cli("audit", "list", "--command", "server exec", "--json")
		if code != 0 {
			t.Fatalf("audit list 失败 (%d): %s %s", code, audOut, stderr)
		}
		var audPage struct {
			Items []struct {
				FullCommand *string `json:"full_command"`
				Output      *string `json:"output"`
				Result      string  `json:"result"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(audOut), &audPage); err != nil || len(audPage.Items) == 0 {
			t.Fatalf("audit list 解析失败：%v %s", err, audOut)
		}
		var foundExecAud bool
		for _, item := range audPage.Items {
			if item.FullCommand != nil && *item.FullCommand == "echo hello-m221 token=sat_execsecret888" {
				foundExecAud = true
				if item.Output == nil || strings.Contains(*item.Output, "sat_execsecret888") || !strings.Contains(*item.Output, "***") {
					t.Fatalf("audit output 应打码 sat_*，得到 %v", item.Output)
				}
				break
			}
		}
		if !foundExecAud {
			t.Fatalf("audit list 中未找到含 full_command 的 server exec 执行审计记录：%s", audOut)
		}

		// MCP satchel_run 透传 -- <命令>
		mcpOut, mcpErr := h.mcpRun("server", "exec", "hk-diag", "--confirm", "hk-diag", "--", "echo", "mcp-exec-ok")
		if mcpErr || !strings.Contains(mcpOut, `"status":"done"`) || !strings.Contains(mcpOut, "mcp-exec-ok") {
			t.Fatalf("MCP server exec -- 透传失败：isErr=%v out=%s", mcpErr, mcpOut)
		}

		// REST POST /api/v1/server/exec/hk-diag
		reqBody := `{"command":"echo rest-exec-ok","timeout":"10s","confirm":"hk-diag"}`
		req, _ := http.NewRequest(http.MethodPost, h.tcpURL+"/api/v1/server/exec/hk-diag", strings.NewReader(reqBody))
		req.Header.Set("Authorization", "Bearer "+execTok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(rBytes), `"job_id":"job-`) {
			t.Fatalf("REST server exec 受理失败：status=%d body=%s", resp.StatusCode, rBytes)
		}
		var restJob struct {
			JobID string `json:"job_id"`
		}
		_ = json.Unmarshal(rBytes, &restJob)
		waitJobTerminal := func(jobID string, wantStatus string) string {
			deadline := time.Now().Add(8 * time.Second)
			for {
				jOut, _, jCode := h.cli("job", "get", jobID, "--json")
				if jCode == 0 && strings.Contains(jOut, `"status":"`+wantStatus+`"`) {
					return jOut
				}
				if time.Now().After(deadline) {
					t.Fatalf("等待 job %s 进入 %s 超时，最后响应：%s", jobID, wantStatus, jOut)
				}
				time.Sleep(40 * time.Millisecond)
			}
		}
		restDone := waitJobTerminal(restJob.JobID, "done")
		if !strings.Contains(restDone, "rest-exec-ok") {
			t.Fatalf("REST server exec 结果不对：%s", restDone)
		}

		// 超时命令（1s 超时）返回非零且 state=timed_out
		toOut, _, toCode := h.cli("--server", h.tcpURL, "--token", execTok, "server", "exec", "hk-diag", "--confirm", "hk-diag", "--timeout", "1s", "--json", "--", "sleep 5")
		if toCode == 0 || !strings.Contains(toOut, `"status":"failed"`) || !strings.Contains(toOut, "timed_out") {
			t.Fatalf("超时命令应以非零退出并标 failed/timed_out：code=%d out=%s", toCode, toOut)
		}

		// === 4. 并发上限与排队深度 ===
		stOut, _, _ := h.cli("settings", "show", "--json")
		var stObj struct {
			Metadata struct {
				ResourceVersion int64 `json:"resourceVersion"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal([]byte(stOut), &stObj)
		if _, sErr, sCode := h.cli("settings", "set",
			"--set", "exec_max_concurrency=1",
			"--set", "exec_queue_depth=1",
			"--set", "evidence_max_bytes=65536",
			"--set", "evidence_retention_days=1",
			"--resource-version", strconv.FormatInt(stObj.Metadata.ResourceVersion, 10),
			"--json"); sCode != 0 {
			t.Fatalf("更新系统设置失败 (%d): %s", sCode, sErr)
		}

		mu.Lock()
		holdRelease = make(chan struct{})
		mu.Unlock()
		j1Out, sErr, j1Code := h.cli("--server", h.tcpURL, "--token", execTok, "server", "exec", "hk-diag", "--confirm", "hk-diag", "--no-wait", "--json", "--", "echo q1")
		if j1Code != 0 {
			t.Fatalf("q1 受理失败 (%d): %s %s", j1Code, j1Out, sErr)
		}
		<-holdStart
		j2Out, sErr, j2Code := h.cli("--server", h.tcpURL, "--token", execTok, "server", "exec", "hk-diag", "--confirm", "hk-diag", "--no-wait", "--json", "--", "echo q2")
		if j2Code != 0 || !strings.Contains(j2Out, `"queue_position":1`) {
			t.Fatalf("q2 应进入排队 queue_position=1 (%d): %s %s", j2Code, j2Out, sErr)
		}
		if _, errOut, j3Code := h.cli("--server", h.tcpURL, "--token", execTok, "server", "exec", "hk-diag", "--confirm", "hk-diag", "--no-wait", "--json", "--", "echo q3"); j3Code == 0 || !strings.Contains(errOut, "conflict") {
			t.Fatalf("q3 排队满应报 conflict：code=%d err=%s", j3Code, errOut)
		}
		mu.Lock()
		close(holdRelease)
		holdRelease = nil
		mu.Unlock()
		var j1Obj, j2Obj struct {
			JobID string `json:"job_id"`
		}
		_ = json.Unmarshal([]byte(j1Out), &j1Obj)
		_ = json.Unmarshal([]byte(j2Out), &j2Obj)
		waitJobTerminal(j1Obj.JobID, "done")
		waitJobTerminal(j2Obj.JobID, "done")

		// === 5. 执行期间断连标 unknown（保留已收输出），重连后自动补报终态 done ===
		mu.Lock()
		dropMidExec = true
		mu.Unlock()
		unkOut, _, unkCode := h.cli("--server", h.tcpURL, "--token", execTok, "server", "exec", "hk-diag", "--confirm", "hk-diag", "--json", "--", "echo drop-test")
		if unkCode == 0 || !strings.Contains(unkOut, `"status":"unknown"`) || !strings.Contains(unkOut, "before-drop") {
			t.Fatalf("断连时 CLI 应输出 unknown job 并以非零退出：code=%d out=%s", unkCode, unkOut)
		}
		var unkJob struct {
			JobID string `json:"job_id"`
		}
		_ = json.Unmarshal([]byte(unkOut), &unkJob)

		// 重连节点，验证自动按 job_id 补报终态到 done
		mu.Lock()
		dropMidExec = false
		mu.Unlock()
		agRemoteCan()
		agRe := dialAgentWithIP(t, h, "203.0.113.50")
		if res := authAgentFull(agRe, pub, tokRemote, "node-mid-remote-50", true); !res.OK {
			t.Fatalf("重连 auth 失败：%+v", res)
		}
		_, agReCan := serveFakeRemote(agRe)
		t.Cleanup(agReCan)
		reconciledOut := waitJobTerminal(unkJob.JobID, "done")
		if !strings.Contains(reconciledOut, "after-reconnect") {
			t.Fatalf("重连补报应带完整输出：%s", reconciledOut)
		}

		// === 6. 一键取证（在线截断打码 + 离线容错落库）与 evidence list/get + job_cleanup 过期清理 ===
		evOn, stderr, code := h.cli("server", "evidence", "hk-diag", "--category", "general", "--json")
		if code != 0 {
			t.Fatalf("在线取证失败 (%d): %s %s", code, evOn, stderr)
		}
		var pkgOn struct {
			Metadata struct {
				ID int64 `json:"id"`
			} `json:"metadata"`
			Status struct {
				SizeBytes int64           `json:"size_bytes"`
				Items     json.RawMessage `json:"items"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(evOn), &pkgOn); err != nil {
			t.Fatal(err)
		}
		if pkgOn.Status.SizeBytes > 65536 {
			t.Fatalf("在线证据包应截断至 <= 65536，得到 %d", pkgOn.Status.SizeBytes)
		}
		if strings.Contains(string(pkgOn.Status.Items), "sat_logsecret9999") || strings.Contains(string(pkgOn.Status.Items), "raw-node-privkey-123") {
			t.Fatalf("证据包未完成敏感字段脱敏：%s", pkgOn.Status.Items)
		}
		var env diag.EvidenceEnvelope
		if err := json.Unmarshal(pkgOn.Status.Items, &env); err != nil || !env.DataOnly || !env.Truncated {
			t.Fatalf("证据包封装字段不对：err=%v data_only=%v truncated=%v", err, env.DataOnly, env.Truncated)
		}

		// 离线节点 offline 与 network 类别取证
		evOff1, stderr, code := h.cli("server", "evidence", "hk-offline", "--category", "offline", "--json")
		if code != 0 || !strings.Contains(evOff1, `"category":"offline"`) {
			t.Fatalf("离线节点 offline 取证失败 (%d): %s %s", code, evOff1, stderr)
		}
		evOff2, stderr, code := h.cli("server", "evidence", "hk-offline", "--category", "network", "--json")
		if code != 0 || !strings.Contains(evOff2, `"collect_error":`) || strings.Contains(evOff2, `"collect_error":null`) {
			t.Fatalf("离线节点 network 取证应容错落库并写 collect_error (%d): %s %s", code, evOff2, stderr)
		}

		// evidence list & evidence get
		evList, stderr, code := h.cli("evidence", "list", "--server-name", "hk-diag", "--json")
		if code != 0 || !strings.Contains(evList, `"total":1`) {
			t.Fatalf("evidence list 按 server-name 过滤失败 (%d): %s %s", code, evList, stderr)
		}
		evGot, stderr, code := h.cli("evidence", "get", strconv.FormatInt(pkgOn.Metadata.ID, 10), "--json")
		if code != 0 || !strings.Contains(evGot, `"kind":"EvidencePackage"`) {
			t.Fatalf("evidence get 失败 (%d): %s %s", code, evGot, stderr)
		}

		// 将全部证据包的 expires_at 回拨到过去，并触发 job_cleanup 内置任务验证顺带清理过期证据包
		past := time.Now().UTC().Add(-2 * time.Hour)
		if _, err := bdb.NewUpdate().Model((*model.EvidencePackage)(nil)).Set("expires_at = ?", past).Where("id > 0").Exec(context.Background()); err != nil {
			t.Fatal(err)
		}
		// 触发内置任务 job_cleanup（通过 app.sched.RunNow）
		if err := h.app.sched.RunNow(context.Background(), "job_cleanup"); err != nil {
			t.Fatalf("触发 job_cleanup 失败：%v", err)
		}
		evAfter, _, code := h.cli("evidence", "list", "--json")
		if code != 0 || !strings.Contains(evAfter, `"total":0`) {
			t.Fatalf("job_cleanup 应清理全部过期证据包，得到：%s", evAfter)
		}
	})
}
