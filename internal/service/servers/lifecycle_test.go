package servers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	svcjobs "github.com/satchel/satchel/internal/service/jobs"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
	"github.com/satchel/satchel/pkg/xrpc"
)

type fakeLifecycleConns struct {
	mu           sync.Mutex
	calls        map[int64][]string
	disconnected map[int64]int
	blockedSync  map[int64]bool
	runtimes     map[int64]agentconn.NodeRuntime
	handlers     map[int64]func(method string, params any) (json.RawMessage, error)
}

func newFakeLifecycleConns() *fakeLifecycleConns {
	return &fakeLifecycleConns{
		calls:        map[int64][]string{},
		disconnected: map[int64]int{},
		blockedSync:  map[int64]bool{},
		runtimes:     map[int64]agentconn.NodeRuntime{},
		handlers:     map[int64]func(method string, params any) (json.RawMessage, error){},
	}
}

func (c *fakeLifecycleConns) ConnectedIDs() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int64, 0, len(c.handlers))
	for id := range c.handlers {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (c *fakeLifecycleConns) Call(_ context.Context, serverID int64, method string, params any, _ time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	c.calls[serverID] = append(c.calls[serverID], method)
	h := c.handlers[serverID]
	c.mu.Unlock()
	if h == nil {
		return nil, agentconn.ErrOffline
	}
	return h(method, params)
}

func (c *fakeLifecycleConns) Runtime(serverID int64) (agentconn.NodeRuntime, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rt, ok := c.runtimes[serverID]
	return rt, ok
}

func (c *fakeLifecycleConns) RecordRuntime(serverID int64, rt agentconn.NodeRuntime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtimes[serverID] = rt
}

func (c *fakeLifecycleConns) SetMasterSyncBlocked(serverID int64, blocked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockedSync[serverID] = blocked
}

func (c *fakeLifecycleConns) Disconnect(_ context.Context, serverID int64) error {
	c.mu.Lock()
	c.disconnected[serverID]++
	delete(c.handlers, serverID)
	delete(c.runtimes, serverID)
	c.mu.Unlock()
	return nil
}

type fakeReleaseFeed struct {
	version         string
	controlProtocol int
	minMaster       string
	cdnDown         bool
	allDown         bool
}

func newFakeReleaseServer(t *testing.T, feed *fakeReleaseFeed) release.Sources {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/cdn/satchel-agent/channels/stable/version.json", func(w http.ResponseWriter, _ *http.Request) {
		if feed.cdnDown || feed.allDown {
			http.Error(w, "cdn down", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":            feed.version,
			"control_protocol":   feed.controlProtocol,
			"min_master_version": feed.minMaster,
		})
	})
	mux.HandleFunc("/api/repos/tajiaoyezi/satchel-agent/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		if feed.allDown {
			http.Error(w, "github down", http.StatusBadGateway)
			return
		}
		notes := fmt.Sprintf("release <!-- satchel-meta: {\"control_protocol\":%d,\"min_master_version\":%q} -->",
			feed.controlProtocol, feed.minMaster)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v" + strings.TrimPrefix(feed.version, "v"),
			"body":     notes,
			"html_url": "https://example.invalid/releases/" + feed.version,
		})
	})
	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		if feed.allDown {
			http.Error(w, "proxy down", http.StatusBadGateway)
			return
		}
		http.Error(w, "unused unless api down", http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return release.Sources{
		CDN:     srv.URL + "/cdn",
		Project: "satchel-agent",
		Repo:    "tajiaoyezi/satchel-agent",
		API:     srv.URL + "/api",
		Web:     srv.URL + "/web",
		Proxy:   srv.URL + "/proxy",
		Client:  srv.Client(),
	}
}

func waitJobDone(t *testing.T, repo *corejobs.Repo, jobID string) *corejobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := repo.Get(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Finished() {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待长任务 %s 完成超时，当前状态 %s", jobID, j.Status)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func adminWithDangers(dangers ...v1.Danger) context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{
		Actor:     "admin-tok",
		ActorKind: v1.ActorToken,
		Role:      v1.RoleAdmin,
		Scopes:    append([]v1.Scope{}, v1.AllScopes...),
		Danger:    dangers,
	})
}

func TestLifecycleUpdateCheckApplyBatch(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		settingsRepo := coresettings.New(bdb, st, schema.Default())
		if err := settingsRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := settingsRepo.Write(ctx, coresettings.WriteRequest{
			Values:          map[string]any{"update_cdn_enabled": true},
			ExpectedVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		repo := core.New(bdb, st)
		jobsRepo := corejobs.New(bdb, st)
		jobsSvc := svcjobs.New(jobsRepo, command.Catalog(), func(_ *command.Command, _ *command.Invocation) string { return `{}` }, nil)
		t.Cleanup(func() { jobsSvc.Stop(context.Background()) })

		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		feed := &fakeReleaseFeed{version: "0.2.0", controlProtocol: xrpc.ControlProtocol, minMaster: "0.1.0"}
		src := newFakeReleaseServer(t, feed)
		conns := newFakeLifecycleConns()

		svc := New(repo, settingsRepo, pub, nil)
		svc.SetConns(conns)
		svc.SetMasterVersion("0.2.0")
		svc.SetReleaseSources(src)
		svc.SetUpdatePollTimings(15*time.Millisecond, 180*time.Millisecond)
		svc.SetStartJob(func(jctx context.Context, sid int64, inv *command.Invocation, run func(context.Context) (any, error), done func()) (any, error) {
			return jobsSvc.StartForServer(jctx, sid, inv, run, done)
		})

		hk1, err := repo.Create(ctx, "hk-1", ModeWebsocket)
		if err != nil {
			t.Fatal(err)
		}
		hk2, err := repo.Create(ctx, "hk-2", ModeHTTP)
		if err != nil {
			t.Fatal(err)
		}
		hkDocker, err := repo.Create(ctx, "hk-docker", ModeWebsocket)
		if err != nil {
			t.Fatal(err)
		}
		hkOffline, err := repo.Create(ctx, "hk-off", ModeWebsocket)
		if err != nil {
			t.Fatal(err)
		}

		// 默认模拟状态：hk-1 与 hk-2 为 0.1.0 非容器，hk-docker 在容器内，hk-off 离线。
		var mu sync.Mutex
		ver := map[int64]string{hk1.Metadata.ID: "0.1.0", hk2.Metadata.ID: "0.1.0", hkDocker.Metadata.ID: "0.1.0"}
		execBusy := map[int64]bool{}
		updHandler := map[int64]func(xrpc.AgentUpdateParams) (any, error){}
		register := func(id int64, inDocker bool) {
			conns.mu.Lock()
			conns.handlers[id] = func(method string, params any) (json.RawMessage, error) {
				switch method {
				case xrpc.MethodAgentInfo:
					mu.Lock()
					v := ver[id]
					busy := execBusy[id]
					mu.Unlock()
					raw, _ := json.Marshal(xrpc.AgentInfo{AgentVersion: v, ControlProtocol: 1, InDocker: inDocker, ExecRunning: busy})
					return raw, nil
				case xrpc.MethodAgentUpdate:
					p := params.(xrpc.AgentUpdateParams)
					mu.Lock()
					fn := updHandler[id]
					mu.Unlock()
					if fn == nil {
						res, err := json.Marshal(xrpc.AgentUpdateAck{Accepted: true, PreviousVersion: ver[id], TargetVersion: p.Version, Source: "cdn"})
						return res, err
					}
					out, err := fn(p)
					if err != nil {
						return nil, err
					}
					res, _ := json.Marshal(out)
					return res, nil
				default:
					return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
				}
			}
			conns.mu.Unlock()
		}
		register(hk1.Metadata.ID, false)
		register(hk2.Metadata.ID, false)
		register(hkDocker.Metadata.ID, true)
		_ = hkOffline

		adminCtx := v1.WithIdentity(ctx, v1.LocalAdmin("root"))
		b := svc.Bindings()

		// 1. server update check 单台 + 全量汇总 + Docker 禁用 + 控制协议不兼容 + 全源不可用。
		out, err := b["server update check"](adminCtx, &command.Invocation{Path: []string{"server", "update", "check"}, Args: []string{"hk-1"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		item := out.(*AgentUpdateCheckItem)
		if item.CurrentVersion != "0.1.0" || item.LatestVersion != "0.2.0" || !item.HasUpdate || !item.Compatible || !item.CanApply {
			t.Fatalf("hk-1 检查结果不对：%+v", item)
		}

		outAll, err := b["server update check"](adminCtx, &command.Invocation{Path: []string{"server", "update", "check"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		list := outAll.(*AgentUpdateCheckList)
		if list.Total != 4 || len(list.Items) != 4 {
			t.Fatalf("全量汇总数量应当为 4：%+v", list)
		}
		for _, it := range list.Items {
			if it.Server == "hk-docker" && (!it.InDocker || it.CanApply) {
				t.Fatalf("Docker 节点应当 in_docker=true 且 can_apply=false：%+v", it)
			}
			if it.Server == "hk-off" && (it.CanApply || it.Reachable) {
				t.Fatalf("离线节点应当 can_apply=false：%+v", it)
			}
		}

		feed.minMaster = "0.3.0"
		feed.version = "0.3.0"
		incompOut, err := b["server update check"](adminCtx, &command.Invocation{Path: []string{"server", "update", "check"}, Args: []string{"hk-1"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if incomp := incompOut.(*AgentUpdateCheckItem); incomp.Compatible || incomp.CanApply {
			t.Fatalf("要求主控 >=0.3.0 时 0.2.0 主控应当返回 compatible=false：%+v", incomp)
		}
		// 恢复 0.2.0 兼容发行版。
		feed.minMaster = "0.1.0"
		feed.version = "0.2.0"

		// 2. 前置拒绝对照（不建 job）：Docker、高版本、exec 互斥、离线。
		if _, err := b["server update apply"](adminCtx, &command.Invocation{Path: []string{"server", "update", "apply"}, Args: []string{"hk-docker", "0.2.0"}, Flags: map[string]any{}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("Docker 节点 apply 应当返回 conflict：%v", err)
		}
		if _, err := b["server update apply"](adminCtx, &command.Invocation{Path: []string{"server", "update", "apply"}, Args: []string{"hk-off", "0.2.0"}, Flags: map[string]any{}}); v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("离线节点 apply 应当返回 unavailable：%v", err)
		}
		mu.Lock()
		execBusy[hk1.Metadata.ID] = true
		mu.Unlock()
		if _, err := b["server update apply"](adminCtx, &command.Invocation{Path: []string{"server", "update", "apply"}, Args: []string{"hk-1", "0.2.0"}, Flags: map[string]any{}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("exec 运行中应当返回 conflict：%v", err)
		}
		mu.Lock()
		execBusy[hk1.Metadata.ID] = false
		mu.Unlock()
		if n, _ := jobsRepo.Count(ctx, ""); n != 0 {
			t.Fatalf("前置检查拒绝时不应当创建 job，当前数量 %d", n)
		}

		// 3. 单台成功升级 + server_id 写入验证。
		mu.Lock()
		updHandler[hk1.Metadata.ID] = func(p xrpc.AgentUpdateParams) (any, error) {
			go func() {
				time.Sleep(35 * time.Millisecond)
				mu.Lock()
				ver[hk1.Metadata.ID] = p.Version
				mu.Unlock()
			}()
			return xrpc.AgentUpdateAck{Accepted: true, PreviousVersion: "0.1.0", TargetVersion: p.Version, Source: "cdn"}, nil
		}
		mu.Unlock()
		jRaw, err := b["server update apply"](adminCtx, &command.Invocation{Path: []string{"server", "update", "apply"}, Args: []string{"hk-1", "0.2.0"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		job := jRaw.(*corejobs.Job)
		if job.ServerID == nil || *job.ServerID != hk1.Metadata.ID {
			t.Fatalf("server update apply job 应当关联 server_id=%d：%v", hk1.Metadata.ID, job.ServerID)
		}
		finished := waitJobDone(t, jobsRepo, job.JobID)
		if finished.Status != corejobs.StatusDone || finished.Output == nil || !strings.Contains(*finished.Output, `"0.2.0"`) {
			t.Fatalf("单台升级应当 done 且包含新版本：%#v", finished)
		}

		// 4. 三态失败分类验证：重置 hk-1 回 0.1.0。
		mu.Lock()
		ver[hk1.Metadata.ID] = "0.1.0"
		updHandler[hk1.Metadata.ID] = func(xrpc.AgentUpdateParams) (any, error) {
			return nil, &xrpc.Error{Code: xrpc.CodeFailed, Message: "三源均失败"}
		}
		mu.Unlock()
		jOldRaw, err := b["server update apply"](adminCtx, &command.Invocation{Path: []string{"server", "update", "apply"}, Args: []string{"hk-1", "0.2.0"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		jOld := waitJobDone(t, jobsRepo, jOldRaw.(*corejobs.Job).JobID)
		if jOld.Status != corejobs.StatusFailed || !strings.Contains(*jOld.Output, `"disk_state":"old"`) || !strings.Contains(*jOld.Output, `"retryable":true`) || !strings.Contains(*jOld.Output, `"batch_continue":true`) {
			t.Fatalf("阶段错误应当归类 disk_state=old, retryable=true, batch_continue=true：%s", *jOld.Output)
		}

		// 态 3：accept 后重连仍报告旧版本 -> old_after_swap。
		mu.Lock()
		updHandler[hk1.Metadata.ID] = func(p xrpc.AgentUpdateParams) (any, error) {
			return xrpc.AgentUpdateAck{Accepted: true, PreviousVersion: "0.1.0", TargetVersion: p.Version, Source: "cdn"}, nil
		}
		mu.Unlock()
		jSwapOldRaw, err := b["server update apply"](adminCtx, &command.Invocation{Path: []string{"server", "update", "apply"}, Args: []string{"hk-1", "0.2.0"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		jSwapOld := waitJobDone(t, jobsRepo, jSwapOldRaw.(*corejobs.Job).JobID)
		if jSwapOld.Status != corejobs.StatusFailed || !strings.Contains(*jSwapOld.Output, `"disk_state":"old_after_swap"`) {
			t.Fatalf("重连仍报旧版应当归类 old_after_swap：%s", *jSwapOld.Output)
		}

		// 5. batch 权限、数组校验与数量确认，以及遇 new_unconfirmed (batch_continue=false) 立即止损不发给 hk-2。
		onlyBatchCtx := adminWithDangers(v1.DangerBatch)
		if _, err := b["server update batch"](onlyBatchCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{"hk-1", "hk-2"}}, Confirm: "2",
		}); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("仅有 DangerBatch 无 DangerRestart 应当返回 forbidden：%v", err)
		}

		if _, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{"hk-1", "hk-2"}}, Confirm: "1",
		}); v1.AsError(err).Code != v1.CodeConfirmRequired {
			t.Fatalf("confirm 不等于目标台数 2 应当返回 confirm_required：%v", err)
		}

		if _, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{"hk-1", "hk-2"}, "all": true}, Confirm: "2",
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("--servers 与 --all 同时传应当 bad_request：%v", err)
		}

		if _, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{}, "all": true}, Confirm: "2",
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("空 --servers 与 --all 同时传应当 bad_request：%v", err)
		}

		if _, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{}}, Confirm: "0",
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("空 --servers 应当 bad_request：%v", err)
		}

		if _, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{"hk-1", "   "}}, Confirm: "2",
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("--servers 含空白项应当 bad_request：%v", err)
		}

		// hk-1 在 accept 后断开失联 -> new_unconfirmed, batch_continue=false -> 不向 hk-2 下发 agent.update。
		conns.mu.Lock()
		conns.calls[hk2.Metadata.ID] = nil
		conns.mu.Unlock()
		mu.Lock()
		updHandler[hk1.Metadata.ID] = func(p xrpc.AgentUpdateParams) (any, error) {
			conns.mu.Lock()
			delete(conns.handlers, hk1.Metadata.ID) // 模拟替换后不再恢复心跳
			conns.mu.Unlock()
			return xrpc.AgentUpdateAck{Accepted: true, PreviousVersion: "0.1.0", TargetVersion: p.Version, Source: "cdn"}, nil
		}
		mu.Unlock()
		batchRaw, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{"hk-1", "hk-2"}}, Confirm: "2",
		})
		if err != nil {
			t.Fatal(err)
		}
		batchJob := waitJobDone(t, jobsRepo, batchRaw.(*corejobs.Job).JobID)
		if batchJob.Status != corejobs.StatusFailed ||
			!strings.Contains(*batchJob.Output, `"disk_state":"new_unconfirmed"`) ||
			!strings.Contains(*batchJob.Output, `"batch_continue":false`) ||
			!strings.Contains(*batchJob.Output, `"status":"aborted"`) {
			t.Fatalf("batch 遇 new_unconfirmed 应当立即中止并记录完整三态清单：%s", *batchJob.Output)
		}
		conns.mu.Lock()
		hk2Calls := append([]string(nil), conns.calls[hk2.Metadata.ID]...)
		conns.mu.Unlock()
		if slices.Contains(hk2Calls, xrpc.MethodAgentUpdate) {
			t.Fatalf("hk-1 触发止损后绝不能再向 hk-2 发 agent.update：%v", hk2Calls)
		}

		// 全源不可达。
		feed.allDown = true
		if _, err := b["server update check"](adminCtx, &command.Invocation{Path: []string{"server", "update", "check"}, Args: []string{"hk-2"}, Flags: map[string]any{}}); v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("全源不可达应当返回 unavailable：%v", err)
		}
	})
}

func TestLifecycleUninstallSwitchPortsAndProbeMasterURL(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		settingsRepo := coresettings.New(bdb, st, schema.Default())
		if err := settingsRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := settingsRepo.Write(ctx, coresettings.WriteRequest{
			Values:          map[string]any{"master_url": "https://panel.example.com"},
			ExpectedVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		repo := core.New(bdb, st)
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		conns := newFakeLifecycleConns()
		svc := New(repo, settingsRepo, pub, nil)
		svc.SetConns(conns)

		hk1, err := repo.CreateWith(ctx, core.CreateParams{Name: "hk-1", Mode: ModeHTTP, ListenPort: 23889, PullPort: 23890})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.MarkOnline(ctx, hk1.Metadata.ID, "198.51.100.1", false, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
		hkDocker, err := repo.Create(ctx, "hk-docker", ModeWebsocket)
		if err != nil {
			t.Fatal(err)
		}

		var switchedListen, switchedPull int
		var probedURLs []string
		var execBusy bool
		conns.mu.Lock()
		conns.handlers[hk1.Metadata.ID] = func(method string, params any) (json.RawMessage, error) {
			switch method {
			case xrpc.MethodAgentInfo:
				raw, _ := json.Marshal(xrpc.AgentInfo{AgentVersion: "0.2.0", ControlProtocol: 1, InDocker: false, ExecRunning: execBusy})
				return raw, nil
			case xrpc.MethodAgentSwitchPorts:
				p := params.(xrpc.SwitchPortsParams)
				switchedListen, switchedPull = p.ListenPort, p.PullPort
				raw, _ := json.Marshal(xrpc.SwitchPortsAck{ListenPort: switchedListen, PullPort: switchedPull, UpdatedAt: time.Now().UTC()})
				return raw, nil
			case xrpc.MethodAgentProbeMaster:
				p := params.(xrpc.ProbeMasterParams)
				probedURLs = append(probedURLs, p.URL)
				raw, _ := json.Marshal(xrpc.ProbeMasterResult{URL: p.URL, Reachable: true, LatencyMS: 18})
				return raw, nil
			case xrpc.MethodAgentUninstall:
				raw, _ := json.Marshal(xrpc.AgentUninstallAck{Accepted: true, ScheduledAt: time.Now().UTC(), Spawner: "systemd-run"})
				return raw, nil
			default:
				return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
			}
		}
		conns.handlers[hkDocker.Metadata.ID] = func(method string, _ any) (json.RawMessage, error) {
			if method == xrpc.MethodAgentInfo {
				raw, _ := json.Marshal(xrpc.AgentInfo{AgentVersion: "0.2.0", ControlProtocol: 1, InDocker: true})
				return raw, nil
			}
			return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
		}
		conns.mu.Unlock()

		adminCtx := v1.WithIdentity(ctx, v1.LocalAdmin("root"))
		b := svc.Bindings()

		// 1. server probe-master-url：允许 master，禁止未配置 recovery 与任意 URL（防 SSRF）。
		probeOut, err := b["server probe-master-url"](adminCtx, &command.Invocation{Path: []string{"server", "probe-master-url"}, Args: []string{"hk-1"}, Flags: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		pr := probeOut.(*MasterProbeOutput)
		if !pr.Reachable || pr.URL != "https://panel.example.com" || pr.LatencyMS != 18 {
			t.Fatalf("probe master 结果不对：%+v", pr)
		}
		if _, err := b["server probe-master-url"](adminCtx, &command.Invocation{Path: []string{"server", "probe-master-url"}, Args: []string{"hk-1"}, Flags: map[string]any{"target": "recovery"}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("空的 recovery 地址应当返回 conflict：%v", err)
		}
		if _, err := b["server probe-master-url"](adminCtx, &command.Invocation{Path: []string{"server", "probe-master-url"}, Args: []string{"hk-1"}, Flags: map[string]any{"target": "https://169.254.169.254"}}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("任意 URL 应当被 target 枚举拒绝返回 bad_request：%v", err)
		}
		if len(probedURLs) != 1 {
			t.Fatalf("被拦截的请求绝不应触发节点探测：%v", probedURLs)
		}

		// 2. server switch-listen-port 校验与成功后抬升 resourceVersion。
		verBefore := hk1.Metadata.ResourceVersion
		if _, err := b["server switch-listen-port"](adminCtx, &command.Invocation{
			Path: []string{"server", "switch-listen-port"}, Args: []string{"hk-1"}, Flags: map[string]any{"listen-port": 23890, "pull-port": 23890},
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("两端口相同应当返回 bad_request：%v", err)
		}
		swOut, err := b["server switch-listen-port"](adminCtx, &command.Invocation{
			Path: []string{"server", "switch-listen-port"}, Args: []string{"hk-1"}, Flags: map[string]any{"listen-port": 24889, "pull-port": 24890},
		})
		if err != nil {
			t.Fatal(err)
		}
		updated := swOut.(*core.Server)
		if switchedListen != 24889 || switchedPull != 24890 || updated.Spec.ListenPort != 24889 || updated.Spec.PullPort != 24890 || updated.Metadata.ResourceVersion != verBefore+1 {
			t.Fatalf("switch-listen-port 更新或版本不对：%+v", updated)
		}

		// 3. server uninstall：拒绝 Docker 与 exec，在线非容器成功并断连且保留 DB 记录。
		if _, err := b["server uninstall"](adminCtx, &command.Invocation{Path: []string{"server", "uninstall"}, Args: []string{"hk-docker"}, Flags: map[string]any{}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("Docker 节点卸载应当返回 conflict：%v", err)
		}
		execBusy = true
		if _, err := b["server uninstall"](adminCtx, &command.Invocation{Path: []string{"server", "uninstall"}, Args: []string{"hk-1"}, Flags: map[string]any{}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("exec 运行中卸载应当返回 conflict：%v", err)
		}
		execBusy = false
		if _, err := b["server uninstall"](adminCtx, &command.Invocation{Path: []string{"server", "uninstall"}, Args: []string{"hk-1"}, Flags: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		conns.mu.Lock()
		discCount := conns.disconnected[hk1.Metadata.ID]
		conns.mu.Unlock()
		if discCount != 1 {
			t.Fatalf("uninstall 成功后应当调用 Disconnect，实际 %d", discCount)
		}
		// 若库仍标 online 则由 MarkOffline 或 Disconnect 真实 AgentConn 标为 offline，此时记录一定保留在库中。
		if kept, err := repo.ByName(ctx, "hk-1"); err != nil || kept == nil {
			t.Fatalf("uninstall 应保留服务器配置记录：%v", err)
		}

		// 4. 迁移前置探测与批量下发钩子。
		if _, err := b["server probe-master-url"](adminCtx, &command.Invocation{Path: []string{"server", "probe-master-url"}, Args: []string{"hk-1"}, Flags: map[string]any{}}); v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("卸载断连后节点不可达应当 unavailable：%v", err)
		}
		// 恢复 hk-1 连接用于验证 ProbeAllNodesMasterURL / PushNodesMasterURL。
		var updatedMasterURL string
		conns.mu.Lock()
		conns.handlers[hk1.Metadata.ID] = func(method string, params any) (json.RawMessage, error) {
			switch method {
			case xrpc.MethodAgentProbeMaster:
				p := params.(xrpc.ProbeMasterParams)
				raw, _ := json.Marshal(xrpc.ProbeMasterResult{URL: p.URL, Reachable: true, LatencyMS: 9})
				return raw, nil
			case xrpc.MethodAgentUpdateMaster:
				p := params.(xrpc.UpdateMasterParams)
				updatedMasterURL = p.MasterURL
				raw, _ := json.Marshal(xrpc.UpdateMasterAck{MasterURL: p.MasterURL, UpdatedAt: time.Now().UTC()})
				return raw, nil
			default:
				return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
			}
		}
		conns.mu.Unlock()
		probes, err := svc.ProbeAllNodesMasterURL(ctx, "https://next.example.com")
		if err != nil || len(probes) != 2 {
			t.Fatalf("ProbeAllNodesMasterURL 结果不对：%+v %v", probes, err)
		}
		if probes[0].Server != "hk-1" || probes[0].State != "ready" {
			t.Fatalf("hk-1 应当 ready：%+v", probes)
		}
		if err := svc.PushNodesMasterURL(ctx, "https://next.example.com", []string{"hk-1"}); err != nil {
			t.Fatal(err)
		}
		if updatedMasterURL != "https://next.example.com" {
			t.Fatalf("PushNodesMasterURL 未下发新地址：%q", updatedMasterURL)
		}
	})
}

func TestBatchUpdateCommaServerNames(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		settingsRepo := coresettings.New(bdb, st, schema.Default())
		if err := settingsRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		repo := core.New(bdb, st)
		jobsRepo := corejobs.New(bdb, st)
		jobsSvc := svcjobs.New(jobsRepo, command.Catalog(), func(_ *command.Command, _ *command.Invocation) string { return `{}` }, nil)
		t.Cleanup(func() { jobsSvc.Stop(context.Background()) })
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		svc := New(repo, settingsRepo, pub, nil)
		svc.SetMasterVersion("0.2.0")
		svc.SetUpdatePollTimings(10*time.Millisecond, 2*time.Second)
		svc.SetReleaseSources(newFakeReleaseServer(t, &fakeReleaseFeed{version: "0.2.0", controlProtocol: xrpc.ControlProtocol, minMaster: "0.1.0"}))
		svc.SetStartJob(func(jctx context.Context, sid int64, inv *command.Invocation, run func(context.Context) (any, error), done func()) (any, error) {
			return jobsSvc.StartForServer(jctx, sid, inv, run, done)
		})

		conns := newFakeLifecycleConns()
		svc.SetConns(conns)

		srvAB, err := repo.Create(ctx, "a,b", ModeWebsocket)
		if err != nil {
			t.Fatal(err)
		}
		srvA, err := repo.Create(ctx, "a", ModeWebsocket)
		if err != nil {
			t.Fatal(err)
		}
		srvB, err := repo.Create(ctx, "b", ModeWebsocket)
		if err != nil {
			t.Fatal(err)
		}

		var mu sync.Mutex
		versions := map[int64]string{
			srvAB.Metadata.ID: "0.1.0",
			srvA.Metadata.ID:  "0.1.0",
			srvB.Metadata.ID:  "0.1.0",
		}
		updated := map[int64]int{}
		registerNode := func(id int64) {
			conns.handlers[id] = func(method string, params any) (json.RawMessage, error) {
				switch method {
				case xrpc.MethodAgentInfo:
					mu.Lock()
					ver := versions[id]
					mu.Unlock()
					raw, _ := json.Marshal(xrpc.AgentInfo{AgentVersion: ver, ControlProtocol: xrpc.ControlProtocol})
					return raw, nil
				case xrpc.MethodAgentUpdate:
					p := params.(xrpc.AgentUpdateParams)
					mu.Lock()
					prev := versions[id]
					versions[id] = p.Version
					updated[id]++
					mu.Unlock()
					raw, _ := json.Marshal(xrpc.AgentUpdateAck{Accepted: true, PreviousVersion: prev, TargetVersion: p.Version, Source: "cdn"})
					return raw, nil
				default:
					return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
				}
			}
		}
		registerNode(srvAB.Metadata.ID)
		registerNode(srvA.Metadata.ID)
		registerNode(srvB.Metadata.ID)

		b := svc.Bindings()
		adminCtx := adminWithDangers(v1.DangerBatch, v1.DangerRestart)

		// 1. 仅选带逗号的 "a,b"：confirm=1 不应拆成 a 与 b。
		jobRaw, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{"a,b"}}, Confirm: "1",
		})
		if err != nil {
			t.Fatalf("仅选 a,b 应当成功：%v", err)
		}
		job1 := waitJobDone(t, jobsRepo, jobRaw.(*corejobs.Job).JobID)
		if job1.Status != corejobs.StatusDone || !strings.Contains(*job1.Output, `"server":"a,b"`) {
			t.Fatalf("仅选 a,b 任务应当 done 且目标为 a,b：%s %s", job1.Status, *job1.Output)
		}
		mu.Lock()
		if updated[srvAB.Metadata.ID] != 1 || updated[srvA.Metadata.ID] != 0 || updated[srvB.Metadata.ID] != 0 {
			t.Fatalf("应当只升级 a,b，实际调用次数 ab=%d a=%d b=%d", updated[srvAB.Metadata.ID], updated[srvA.Metadata.ID], updated[srvB.Metadata.ID])
		}
		// 复原 a,b 为 0.1.0，继续测试同时选 ["a,b", "a"] 不得误升 b。
		versions[srvAB.Metadata.ID] = "0.1.0"
		mu.Unlock()

		// 2. 同时选 "a,b" 与 "a"（confirm=2）：必须只升 a,b 与 a，绝不碰 b。
		jobRaw2, err := b["server update batch"](adminCtx, &command.Invocation{
			Path: []string{"server", "update", "batch"}, Args: []string{"0.2.0"}, Flags: map[string]any{"servers": []string{"a,b", "a"}}, Confirm: "2",
		})
		if err != nil {
			t.Fatalf("同时选 a,b 与 a 应当成功：%v", err)
		}
		job2 := waitJobDone(t, jobsRepo, jobRaw2.(*corejobs.Job).JobID)
		if job2.Status != corejobs.StatusDone || !strings.Contains(*job2.Output, `"succeeded":2`) {
			t.Fatalf("同时选 a,b 与 a 应当成功 2 台：%s %s", job2.Status, *job2.Output)
		}
		mu.Lock()
		if updated[srvAB.Metadata.ID] != 2 || updated[srvA.Metadata.ID] != 1 || updated[srvB.Metadata.ID] != 0 {
			t.Fatalf("应当只升级 a,b 与 a、绝不升级 b，实际 ab=%d a=%d b=%d", updated[srvAB.Metadata.ID], updated[srvA.Metadata.ID], updated[srvB.Metadata.ID])
		}
		mu.Unlock()
	})
}
