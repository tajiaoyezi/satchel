package diag_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	coreevidence "github.com/satchel/satchel/internal/core/evidence"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/service/diag"
	servicejobs "github.com/satchel/satchel/internal/service/jobs"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type fakeNodes struct {
	mu              sync.Mutex
	masterMachineID string
	connectedWS     map[int64]bool
	runtimes        map[int64]agentconn.NodeRuntime

	diagCalls   int
	diagFn      func(ctx context.Context, serverID int64, p xrpc.DiagRunParams) (*xrpc.DiagRunResult, error)
	execStartFn func(ctx context.Context, serverID int64, p xrpc.ExecStartParams) (*xrpc.ExecStartResult, error)
	execStatFn  func(ctx context.Context, serverID int64, p xrpc.ExecStatusParams) (*xrpc.ExecStatusResult, error)
	execCanFn   func(ctx context.Context, serverID int64, p xrpc.ExecCancelParams) (*xrpc.ExecCancelResult, error)
	agentInfoFn func(ctx context.Context, serverID int64) (*xrpc.AgentInfo, error)
}

func newFakeNodes() *fakeNodes {
	return &fakeNodes{
		masterMachineID: "master-mid-001",
		connectedWS:     map[int64]bool{},
		runtimes:        map[int64]agentconn.NodeRuntime{},
	}
}

func (f *fakeNodes) CallDiagRun(ctx context.Context, serverID int64, p xrpc.DiagRunParams) (*xrpc.DiagRunResult, error) {
	f.mu.Lock()
	f.diagCalls++
	fn := f.diagFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, serverID, p)
	}
	return nil, v1.New(v1.CodeUnavailable, "node offline")
}

func (f *fakeNodes) CallExecStart(ctx context.Context, serverID int64, p xrpc.ExecStartParams) (*xrpc.ExecStartResult, error) {
	f.mu.Lock()
	fn := f.execStartFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, serverID, p)
	}
	return &xrpc.ExecStartResult{JobID: p.JobID, Accepted: true, PGID: 4321, StartedAt: time.Now().UTC()}, nil
}

func (f *fakeNodes) CallExecStatus(ctx context.Context, serverID int64, p xrpc.ExecStatusParams) (*xrpc.ExecStatusResult, error) {
	f.mu.Lock()
	fn := f.execStatFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, serverID, p)
	}
	return &xrpc.ExecStatusResult{JobID: p.JobID, Found: false}, nil
}

func (f *fakeNodes) CallExecCancel(ctx context.Context, serverID int64, p xrpc.ExecCancelParams) (*xrpc.ExecCancelResult, error) {
	f.mu.Lock()
	fn := f.execCanFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, serverID, p)
	}
	return &xrpc.ExecCancelResult{JobID: p.JobID, Canceled: true}, nil
}

func (f *fakeNodes) CallAgentInfo(ctx context.Context, serverID int64) (*xrpc.AgentInfo, error) {
	f.mu.Lock()
	fn := f.agentInfoFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, serverID)
	}
	return nil, v1.New(v1.CodeUnavailable, "node offline")
}

func (f *fakeNodes) ConnectedWS(serverID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectedWS[serverID]
}

func (f *fakeNodes) NodeRuntime(serverID int64) (agentconn.NodeRuntime, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.runtimes[serverID]
	return rt, ok
}

func (f *fakeNodes) MasterMachineID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.masterMachineID
}

type fixture struct {
	svc      *diag.Service
	jobsSvc  *servicejobs.Service
	servers  *coreservers.Repo
	settings *coresettings.Repo
	evidence *coreevidence.Repo
	audit    *coreaudit.Repo
	jobsRepo *corejobs.Repo
	nodes    *fakeNodes
	nowMu    sync.Mutex
	curTime  time.Time
}

func newFixture(t *testing.T, bdb *bun.DB) *fixture {
	t.Helper()
	st := store.New(bdb, schema.Default())
	srvRepo := coreservers.New(bdb, st)
	setRepo := coresettings.New(bdb, st, schema.Default())
	if err := setRepo.EnsureSingleton(context.Background()); err != nil {
		t.Fatal(err)
	}
	evRepo := coreevidence.New(bdb, st)
	audRepo := coreaudit.New(bdb, st)
	jobRepo := corejobs.New(bdb, st)
	tbl := command.Catalog()
	jobsSvc := servicejobs.New(jobRepo, tbl, func(*command.Command, *command.Invocation) string { return "{}" }, nil)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		jobsSvc.Stop(cctx)
	})

	nodes := newFakeNodes()
	f := &fixture{
		jobsSvc:  jobsSvc,
		servers:  srvRepo,
		settings: setRepo,
		evidence: evRepo,
		audit:    audRepo,
		jobsRepo: jobRepo,
		nodes:    nodes,
		curTime:  time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC),
	}
	f.svc = diag.New(diag.Deps{
		Servers:          srvRepo,
		Settings:         setRepo,
		Evidence:         evRepo,
		JobsRepo:         jobRepo,
		AuditRepo:        audRepo,
		Nodes:            nodes,
		StartJob:         jobsSvc.StartForServer,
		ReconcileUnknown: jobsSvc.ReconcileUnknown,
		ListUnknownJobs:  jobsSvc.ListUnknownByServer,
		Now: func() time.Time {
			f.nowMu.Lock()
			defer f.nowMu.Unlock()
			return f.curTime
		},
		PollInterval:   30 * time.Millisecond,
		WSFallbackPoll: 200 * time.Millisecond,
	})
	return f
}

func adminCtx(scopes ...v1.Scope) context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{
		Actor:     "admin",
		ActorKind: v1.ActorUser,
		Role:      v1.RoleAdmin,
		Scopes:    scopes,
	})
}

func userCtx() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{
		Actor:     "bob",
		ActorKind: v1.ActorUser,
		Role:      v1.RoleUser,
		Scopes:    []v1.Scope{v1.ScopeRead},
	})
}

func (f *fixture) waitJobStatus(t *testing.T, jobID string, wantStatuses ...string) *corejobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := f.jobsRepo.Get(context.Background(), jobID)
		if err != nil {
			t.Fatalf("读 job %s 失败：%v", jobID, err)
		}
		for _, w := range wantStatuses {
			if j.Status == w {
				return j
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 job %s 进入 %v 超时，当前 status=%q", jobID, wantStatuses, j.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServerDiagPermissionsAndRedaction(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		srv, err := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "hk-1", Mode: coreservers.ModeAuto})
		if err != nil {
			t.Fatal(err)
		}

		cfgData := map[string]any{
			"private_key": "super-secret-priv-key",
			"uuid":        "11111111-2222-3333-4444-555555555555",
			"token":       "sat_abcdef1234567890",
			"listen_port": 443,
		}
		f.nodes.diagFn = func(_ context.Context, _ int64, p xrpc.DiagRunParams) (*xrpc.DiagRunResult, error) {
			return &xrpc.DiagRunResult{
				CollectedAt: time.Now().UTC(),
				NodeTimeUTC: time.Now().UTC(),
				Items: []xrpc.DiagItemResult{
					{Name: xrpc.DiagItemConfig, OK: true, Status: "ok", Data: cfgData},
					{Name: xrpc.DiagItemSpeedtest, OK: true, Status: "not_available"},
				},
			}, nil
		}

		// 普通用户被拒
		if _, err := f.svc.Bindings()["server diag"](userCtx(), &command.Invocation{Path: []string{"server", "diag"}, Args: []string{"hk-1"}}); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("普通用户应得 forbidden，得到 %v", err)
		}
		// 未知诊断项
		if _, err := f.svc.Bindings()["server diag"](adminCtx(v1.ScopeRead), &command.Invocation{Path: []string{"server", "diag"}, Args: []string{"hk-1", "bogus"}}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("未知诊断项应得 bad_request，得到 %v", err)
		}

		// 无 secrets 范围时自动打码 config 且显式标注 content_type=data / untrusted_data=true
		out, err := f.svc.Bindings()["server diag"](adminCtx(v1.ScopeRead), &command.Invocation{Path: []string{"server", "diag"}, Args: []string{"hk-1"}})
		if err != nil {
			t.Fatalf("server diag 失败：%v", err)
		}
		res := out.(*diag.DiagResponse)
		if res.Server != srv.Spec.Name || len(res.Items) != 2 {
			t.Fatalf("返回结构不对：%+v", res)
		}
		cfgItem := res.Items[0]
		if cfgItem.ContentType != "data" || !cfgItem.UntrustedData {
			t.Fatalf("config 项应标注 data / untrusted_data：%+v", cfgItem)
		}
		cfgBytes, _ := json.Marshal(cfgItem.Data)
		if strings.Contains(string(cfgBytes), "super-secret-priv-key") || strings.Contains(string(cfgBytes), "sat_abcdef1234567890") {
			t.Fatalf("无 secrets 权限时敏感值未打码：%s", cfgBytes)
		}

		// 有 secrets 范围时允许透传原值
		outSec, err := f.svc.Bindings()["server diag"](adminCtx(v1.ScopeRead, v1.ScopeSecrets), &command.Invocation{Path: []string{"server", "diag"}, Args: []string{"hk-1"}})
		if err != nil {
			t.Fatalf("带 secrets 的 server diag 失败：%v", err)
		}
		secBytes, _ := json.Marshal(outSec.(*diag.DiagResponse).Items[0].Data)
		if !strings.Contains(string(secBytes), "super-secret-priv-key") {
			t.Fatalf("带 secrets 权限时应保留原始 config：%s", secBytes)
		}
	})
}

func TestServerExecGatesFailClosedAndLocalSwitch(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := adminCtx(v1.ScopeRead, v1.ScopeOperate)
		handler := f.svc.Bindings()["server exec"]

		// 1. 离线节点拒绝
		offSrv, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "hk-off", Mode: coreservers.ModeAuto})
		_ = offSrv
		_, err := handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"hk-off"}, Flags: map[string]any{"command": "echo hi"}})
		if v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("离线节点应报 unavailable，得到 %v", err)
		}

		// 2. 同机标记节点拒绝
		sameSrv, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "local-1", Mode: coreservers.ModeAuto})
		_ = f.servers.SetSameHost(context.Background(), sameSrv.Metadata.ID, true)
		f.nodes.connectedWS[sameSrv.Metadata.ID] = true
		f.nodes.runtimes[sameSrv.Metadata.ID] = agentconn.NodeRuntime{PeerIP: "203.0.113.10", MachineID: "node-other", ExecEnabled: true}
		_, err = handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"local-1"}, Flags: map[string]any{"command": "echo hi"}})
		if v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("同机节点应报 forbidden，得到 %v", err)
		}

		// 3. 运行时证据不足（缺 machine_id）按 fail-closed 拒绝
		noMid, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "no-mid", Mode: coreservers.ModeAuto})
		f.nodes.connectedWS[noMid.Metadata.ID] = true
		f.nodes.runtimes[noMid.Metadata.ID] = agentconn.NodeRuntime{PeerIP: "203.0.113.11", MachineID: "", ExecEnabled: true}
		_, err = handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"no-mid"}, Flags: map[string]any{"command": "echo hi"}})
		if v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("缺失 machine_id 应按 fail-closed 报 forbidden，得到 %v", err)
		}

		// 4. 对端回环 IP 拒绝
		loopSrv, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "loop-1", Mode: coreservers.ModeAuto})
		f.nodes.connectedWS[loopSrv.Metadata.ID] = true
		f.nodes.runtimes[loopSrv.Metadata.ID] = agentconn.NodeRuntime{PeerIP: "127.0.0.1", MachineID: "node-loop", ExecEnabled: true}
		_, err = handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"loop-1"}, Flags: map[string]any{"command": "echo hi"}})
		if v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("回环来源应报 forbidden，得到 %v", err)
		}

		// 5. machine_id 与主控相同拒绝
		dupMid, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "dup-mid", Mode: coreservers.ModeAuto})
		f.nodes.connectedWS[dupMid.Metadata.ID] = true
		f.nodes.runtimes[dupMid.Metadata.ID] = agentconn.NodeRuntime{PeerIP: "203.0.113.12", MachineID: "master-mid-001", ExecEnabled: true}
		_, err = handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"dup-mid"}, Flags: map[string]any{"command": "echo hi"}})
		if v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("machine_id 同主控应报 forbidden，得到 %v", err)
		}

		// 6. 本地 allow_exec 关闭拒绝
		noExec, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "hk-noexec", Mode: coreservers.ModeAuto})
		f.nodes.connectedWS[noExec.Metadata.ID] = true
		f.nodes.runtimes[noExec.Metadata.ID] = agentconn.NodeRuntime{PeerIP: "203.0.113.13", MachineID: "node-remote-1", ExecEnabled: false}
		_, err = handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"hk-noexec"}, Flags: map[string]any{"command": "echo hi"}})
		if v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("未开启 allow_exec 应报 forbidden，得到 %v", err)
		}

		// 以上被拒均不得创建 job 行
		cnt, _ := f.jobsRepo.CountFiltered(context.Background(), corejobs.ListFilter{})
		if cnt != 0 {
			t.Fatalf("闸门拒绝不应写入任何 job 行，却有 %d 行", cnt)
		}
	})
}

func TestServerExecWSStreamAuditQueueAndReconnectReconcile(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := adminCtx(v1.ScopeRead, v1.ScopeOperate)
		handler := f.svc.Bindings()["server exec"]

		srv, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "hk-exec", Mode: coreservers.ModeAuto})
		f.nodes.connectedWS[srv.Metadata.ID] = true
		f.nodes.runtimes[srv.Metadata.ID] = agentconn.NodeRuntime{PeerIP: "203.0.113.20", MachineID: "node-remote-20", ExecEnabled: true}

		// 1. WS 流式执行成功并写入打码后的审计记录
		f.nodes.execStartFn = func(_ context.Context, serverID int64, p xrpc.ExecStartParams) (*xrpc.ExecStartResult, error) {
			go func() {
				time.Sleep(20 * time.Millisecond)
				chunk := "hello token=sat_liveSecret9999\n"
				f.svc.HandleExecEvent(serverID, xrpc.ExecEvent{JobID: p.JobID, Seq: 1, Chunk: chunk, Offset: len(chunk)})
				time.Sleep(20 * time.Millisecond)
				f.svc.HandleExecEvent(serverID, xrpc.ExecEvent{JobID: p.JobID, Seq: 2, Offset: len(chunk), Done: true, State: xrpc.ExecStateDone, ExitCode: 0, DurationMS: 40})
			}()
			return &xrpc.ExecStartResult{JobID: p.JobID, Accepted: true, PGID: 1001, StartedAt: time.Now().UTC()}, nil
		}

		out, err := handler(ctx, &command.Invocation{
			Path:  []string{"server", "exec"},
			Args:  []string{"hk-exec"},
			Flags: map[string]any{"command": "echo token=sat_liveSecret9999", "timeout": "10s"},
		})
		if err != nil {
			t.Fatalf("server exec 失败：%v", err)
		}
		job := out.(*corejobs.Job)
		fin := f.waitJobStatus(t, job.JobID, corejobs.StatusDone)
		if fin.ExitCode == nil || *fin.ExitCode != 0 {
			t.Fatalf("期望 exit_code=0，得到 %v", fin.ExitCode)
		}

		recs, err := f.audit.List(context.Background(), coreaudit.Filter{CommandPrefix: "server exec"}, 10, 0)
		if err != nil || len(recs) == 0 {
			t.Fatalf("期望写入 server exec 审计记录：err=%v len=%d", err, len(recs))
		}
		if recs[0].FullCommand == nil || *recs[0].FullCommand != "echo token=sat_liveSecret9999" {
			t.Fatalf("full_command 不对：%v", recs[0].FullCommand)
		}
		if recs[0].Output == nil || strings.Contains(*recs[0].Output, "sat_liveSecret9999") || !strings.Contains(*recs[0].Output, v1.Redacted) {
			t.Fatalf("audit output 应对 sat_* 打码：%v", recs[0].Output)
		}

		// 2. 并发上限 1、排队上限 1：第 1 个占槽、第 2 个排队并带 queue_position=1、第 3 个直接 conflict
		st, err := f.settings.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.settings.Write(context.Background(), coresettings.WriteRequest{
			ExpectedVersion: st.Version,
			Values: map[string]any{
				"exec_max_concurrency": int64(1),
				"exec_queue_depth":     int64(1),
			},
		}); err != nil {
			t.Fatal(err)
		}

		started1 := make(chan string, 1)
		release1 := make(chan struct{})
		f.nodes.execStartFn = func(_ context.Context, serverID int64, p xrpc.ExecStartParams) (*xrpc.ExecStartResult, error) {
			select {
			case started1 <- p.JobID:
				go func() {
					<-release1
					f.svc.HandleExecEvent(serverID, xrpc.ExecEvent{JobID: p.JobID, Seq: 1, Chunk: "first", Offset: 5})
					f.svc.HandleExecEvent(serverID, xrpc.ExecEvent{JobID: p.JobID, Seq: 2, Offset: 5, Done: true, State: xrpc.ExecStateDone, ExitCode: 0})
				}()
			default:
				go func() {
					time.Sleep(15 * time.Millisecond)
					f.svc.HandleExecEvent(serverID, xrpc.ExecEvent{JobID: p.JobID, Seq: 1, Chunk: "second", Offset: 6})
					f.svc.HandleExecEvent(serverID, xrpc.ExecEvent{JobID: p.JobID, Seq: 2, Offset: 6, Done: true, State: xrpc.ExecStateDone, ExitCode: 0})
				}()
			}
			return &xrpc.ExecStartResult{JobID: p.JobID, Accepted: true, PGID: 2001}, nil
		}

		j1Out, err := handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"hk-exec"}, Flags: map[string]any{"command": "cmd1"}})
		if err != nil {
			t.Fatal(err)
		}
		j1 := j1Out.(*corejobs.Job)
		<-started1

		j2Out, err := handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"hk-exec"}, Flags: map[string]any{"command": "cmd2"}})
		if err != nil {
			t.Fatalf("第 2 个应进入排队，却失败：%v", err)
		}
		j2 := j2Out.(*corejobs.Job)
		progMap, _ := j2.Progress.(map[string]any)
		if progMap["queue_position"] != 1 {
			t.Fatalf("排队任务应带 queue_position=1，得到 %#v", j2.Progress)
		}

		// 第 3 个直接 conflict
		if _, err := handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"hk-exec"}, Flags: map[string]any{"command": "cmd3"}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("队列已满应报 conflict，得到 %v", err)
		}
		close(release1)
		f.waitJobStatus(t, j1.JobID, corejobs.StatusDone)
		f.waitJobStatus(t, j2.JobID, corejobs.StatusDone)

		// 3. 执行期间断连标 unknown（保留首段输出），重连后按 job_id 补报收敛到 done
		started3 := make(chan string, 1)
		f.nodes.execStartFn = func(_ context.Context, serverID int64, p xrpc.ExecStartParams) (*xrpc.ExecStartResult, error) {
			go func() {
				time.Sleep(15 * time.Millisecond)
				f.svc.HandleExecEvent(serverID, xrpc.ExecEvent{JobID: p.JobID, Seq: 1, Chunk: "part1-", Offset: 6})
				started3 <- p.JobID
			}()
			return &xrpc.ExecStartResult{JobID: p.JobID, Accepted: true, PGID: 3001}, nil
		}
		j3Out, err := handler(ctx, &command.Invocation{Path: []string{"server", "exec"}, Args: []string{"hk-exec"}, Flags: map[string]any{"command": "long-cmd"}})
		if err != nil {
			t.Fatal(err)
		}
		j3 := j3Out.(*corejobs.Job)
		<-started3

		// 模拟 WS 断开
		f.svc.HandleNodeOffline(srv.Metadata.ID)
		unk := f.waitJobStatus(t, j3.JobID, corejobs.StatusUnknown)
		if unk.Output == nil || !strings.Contains(*unk.Output, "part1-") {
			t.Fatalf("unknown 状态应保留已收输出，得到 %v", unk.Output)
		}
		if unk.ExitCode != nil {
			t.Fatalf("unknown 状态 exit_code 应为空，得到 %v", *unk.ExitCode)
		}

		// 模拟节点重连并通过 exec.status 回报终态
		f.nodes.execStatFn = func(_ context.Context, _ int64, p xrpc.ExecStatusParams) (*xrpc.ExecStatusResult, error) {
			if p.JobID == j3.JobID {
				return &xrpc.ExecStatusResult{
					JobID:    p.JobID,
					Found:    true,
					State:    xrpc.ExecStateDone,
					Done:     true,
					ExitCode: 0,
					Output:   "part1-part2-complete",
				}, nil
			}
			return &xrpc.ExecStatusResult{JobID: p.JobID, Found: false}, nil
		}
		f.svc.HandleNodeConnected(srv.Metadata.ID)
		reconciled := f.waitJobStatus(t, j3.JobID, corejobs.StatusDone)
		if reconciled.ExitCode == nil || *reconciled.ExitCode != 0 || reconciled.Output == nil || !strings.Contains(*reconciled.Output, "part1-part2-complete") {
			t.Fatalf("重连补报终态不对：status=%s exit=%v out=%v", reconciled.Status, reconciled.ExitCode, reconciled.Output)
		}
	})
}

func TestEvidenceCollectTruncationOfflineAndCleanup(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := adminCtx(v1.ScopeRead, v1.ScopeOperate)
		onlineSrv, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "hk-ev-on", Mode: coreservers.ModeAuto})
		offSrv, _ := f.servers.CreateWith(context.Background(), coreservers.CreateParams{Name: "hk-ev-off", Mode: coreservers.ModeAuto})

		// 调整 evidence_max_bytes 为最小允许值 65536，测试超限截断
		st, _ := f.settings.Load(context.Background())
		if _, err := f.settings.Write(context.Background(), coresettings.WriteRequest{
			ExpectedVersion: st.Version,
			Values: map[string]any{
				"evidence_max_bytes":      int64(65536),
				"evidence_retention_days": int64(7),
			},
		}); err != nil {
			t.Fatal(err)
		}

		bigLog := strings.Repeat("INFO line token=sat_leak_99999999\n", 4000) // > 120 KiB
		f.nodes.diagFn = func(_ context.Context, sid int64, p xrpc.DiagRunParams) (*xrpc.DiagRunResult, error) {
			if sid == offSrv.Metadata.ID {
				return nil, v1.New(v1.CodeUnavailable, "节点离线")
			}
			for _, it := range p.Items {
				if it == xrpc.DiagItemSpeedtest {
					t.Errorf("server evidence 不应请求 speedtest 项")
				}
			}
			return &xrpc.DiagRunResult{
				CollectedAt: time.Now().UTC(),
				NodeTimeUTC: time.Now().UTC(),
				Items: []xrpc.DiagItemResult{
					{Name: xrpc.DiagItemLogs, OK: true, Status: "ok", ContentType: "data", UntrustedData: true, Data: map[string]any{"lines": bigLog}},
				},
			}, nil
		}

		// 1. 在线 general 取证：超限截断 + 敏感打码 + 数据标记 + expires_at
		evOut, err := f.svc.Bindings()["server evidence"](ctx, &command.Invocation{
			Path:  []string{"server", "evidence"},
			Args:  []string{"hk-ev-on"},
			Flags: map[string]any{"category": "general"},
		})
		if err != nil {
			t.Fatalf("在线取证失败：%v", err)
		}
		pkg1 := evOut.(*coreevidence.EvidencePackage)
		if pkg1.Status.SizeBytes > 65536 {
			t.Fatalf("证据包应截断至 <= 65536，得到 %d", pkg1.Status.SizeBytes)
		}
		if pkg1.Status.ExpiresAt == nil {
			t.Fatal("应写入 expires_at")
		}
		if strings.Contains(string(pkg1.Status.Items), "sat_leak_99999999") {
			t.Fatal("证据包日志文本中的 sat_* 应已打码")
		}
		var env diag.EvidenceEnvelope
		if err := json.Unmarshal(pkg1.Status.Items, &env); err != nil {
			t.Fatalf("证据包 JSON 解析失败：%v", err)
		}
		if !env.DataOnly || !env.Truncated || env.DataNotice == "" {
			t.Fatalf("证据包顶层标记不对：data_only=%v truncated=%v notice=%q", env.DataOnly, env.Truncated, env.DataNotice)
		}

		// 2. offline 类别不调用节点在线 RPC
		beforeCalls := f.nodes.diagCalls
		evOff, err := f.svc.Bindings()["server evidence"](ctx, &command.Invocation{
			Path:  []string{"server", "evidence"},
			Args:  []string{"hk-ev-off"},
			Flags: map[string]any{"category": "offline"},
		})
		if err != nil {
			t.Fatalf("offline 取证失败：%v", err)
		}
		if f.nodes.diagCalls != beforeCalls {
			t.Fatalf("offline 类别不应调用节点 diag.run，调用次数从 %d 变 %d", beforeCalls, f.nodes.diagCalls)
		}
		if evOff.(*coreevidence.EvidencePackage).Status.CollectError != nil {
			t.Fatalf("offline 类别无 RPC 失败不应设 collect_error")
		}

		// 3. 离线节点执行 network 取证：仍落库并记 collect_error
		evNet, err := f.svc.Bindings()["server evidence"](ctx, &command.Invocation{
			Path:  []string{"server", "evidence"},
			Args:  []string{"hk-ev-off"},
			Flags: map[string]any{"category": "network"},
		})
		if err != nil {
			t.Fatalf("离线节点 network 取证应容错落库，却返回错误：%v", err)
		}
		pkgNet := evNet.(*coreevidence.EvidencePackage)
		if pkgNet.Status.CollectError == nil || *pkgNet.Status.CollectError == "" {
			t.Fatalf("离线节点 network 取证应写入 collect_error")
		}

		// 4. evidence list 与 evidence get
		listOut, err := f.svc.Bindings()["evidence list"](adminCtx(v1.ScopeRead), &command.Invocation{
			Path:  []string{"evidence", "list"},
			Flags: map[string]any{"server-name": onlineSrv.Spec.Name},
		})
		if err != nil {
			t.Fatalf("evidence list 失败：%v", err)
		}
		pr := listOut.(*command.PageResult)
		if pr.Total != 1 || len(pr.Items) != 1 {
			t.Fatalf("按 server-name 过滤应返回 1 条，得到 total=%d len=%d", pr.Total, len(pr.Items))
		}

		gotOut, err := f.svc.Bindings()["evidence get"](adminCtx(v1.ScopeRead), &command.Invocation{
			Path: []string{"evidence", "get"},
			Args: []string{"1"},
		})
		if err != nil || gotOut.(*coreevidence.EvidencePackage).Metadata.ID != pkg1.Metadata.ID {
			t.Fatalf("evidence get 失败：err=%v out=%+v", err, gotOut)
		}

		// 5. 推进时间超过 7 天，PruneExpiredEvidence 清理全部 3 条过期证据包
		f.nowMu.Lock()
		f.curTime = f.curTime.Add(8 * 24 * time.Hour)
		f.nowMu.Unlock()
		pruned, err := f.svc.PruneExpiredEvidence(context.Background())
		if err != nil || pruned != 3 {
			t.Fatalf("期望清理 3 条过期证据包，得到 pruned=%d err=%v", pruned, err)
		}
	})
}
