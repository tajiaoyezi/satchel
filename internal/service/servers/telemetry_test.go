package servers

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
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type telemetryFakeConns struct {
	mu           sync.Mutex
	connected    []int64
	callFn       func(ctx context.Context, serverID int64, method string, params any, timeout time.Duration) (json.RawMessage, error)
	pullSpeedFn  func(ctx context.Context, serverID int64, timeout time.Duration) (xrpc.SpeedReport, string, bool, error)
	broadcastLog []xrpc.AgentConfig
}

func (f *telemetryFakeConns) Call(ctx context.Context, serverID int64, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if f.callFn != nil {
		return f.callFn(ctx, serverID, method, params, timeout)
	}
	return nil, agentconn.ErrOffline
}

func (f *telemetryFakeConns) ConnectedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.connected...)
}

func (f *telemetryFakeConns) PullSpeed(ctx context.Context, serverID int64, timeout time.Duration) (xrpc.SpeedReport, string, bool, error) {
	if f.pullSpeedFn != nil {
		return f.pullSpeedFn(ctx, serverID, timeout)
	}
	return xrpc.SpeedReport{}, "", false, agentconn.ErrOffline
}

func (f *telemetryFakeConns) BroadcastConfig(_ context.Context, _ []int64, cfg xrpc.AgentConfig) {
	f.mu.Lock()
	f.broadcastLog = append(f.broadcastLog, cfg)
	f.mu.Unlock()
}

func runSvc(t *testing.T, ctx context.Context, svc *Service, name string, args []string, flags map[string]any, page *command.Page) (any, error) {
	t.Helper()
	h, ok := svc.Bindings()[name]
	if !ok {
		t.Fatalf("未登记命令 %q", name)
	}
	return h(ctx, &command.Invocation{Path: strings.Split(name, " "), Args: args, Flags: flags, Page: page})
}

func TestTelemetryCommandsAndRBAC(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		sRepo := core.New(bdb, st)
		cfgRepo := coresettings.New(bdb, st, schema.Default())
		ctx := context.Background()
		if err := cfgRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		svc := New(sRepo, cfgRepo, nil, nil)
		fc := &telemetryFakeConns{}
		svc.SetConns(fc)

		srv, err := sRepo.Create(ctx, "hk-1", ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
		if err := sRepo.MarkOnline(ctx, srv.Metadata.ID, "203.0.113.10", false, now, now); err != nil {
			t.Fatal(err)
		}

		adminCtx := v1.WithIdentity(ctx, v1.LocalAdmin("admin"))
		userCtx := v1.WithIdentity(ctx, v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})

		// 1. 非管理员对 6 条新命令全部被拒 forbidden。
		for _, cmd := range []string{"server system-info", "server nics", "server speed", "server traffic-stats set", "server logs list", "server logs files"} {
			if _, err := runSvc(t, userCtx, svc, cmd, []string{"hk-1"}, map[string]any{}, nil); v1.AsError(err).Code != v1.CodeForbidden {
				t.Fatalf("普通用户调 %s 应当 forbidden，得到 %v", cmd, err)
			}
		}

		// 2. system-info / nics / logs files / logs list 正常返回与错误映射。
		fc.callFn = func(_ context.Context, _ int64, method string, params any, _ time.Duration) (json.RawMessage, error) {
			switch method {
			case xrpc.MethodSystemInfo:
				return json.Marshal(xrpc.SystemInfo{Hostname: "hk-node", Arch: "amd64", CPUCores: 4, CPUThreads: 8, CPUPct: 23.5, MemTotalBytes: 4 << 30, DiskTotalBytes: 40 << 30, CollectedAt: now})
			case xrpc.MethodSystemNICs:
				return json.Marshal(xrpc.SystemNICs{NICs: []xrpc.NICInfo{{Name: "eth0", Addrs: []xrpc.NICAddr{{IP: "203.0.113.10", Family: "v4", CIDR: "203.0.113.10/24", Scope: "global"}}}}})
			case xrpc.MethodAgentLogFiles:
				return json.Marshal(xrpc.AgentLogFilesResult{Dir: "/var/lib/satchel-agent/logs", TotalSize: 2048, Files: []xrpc.AgentLogFileInfo{
					{Name: "satchel-agent.log", Size: 1024, Modified: now, Active: true},
					{Name: "satchel-agent-old.log", Size: 1024, Modified: now.Add(-time.Hour), Active: false},
				}})
			case xrpc.MethodAgentLogs:
				p := params.(xrpc.AgentLogsParams)
				if p.File == "missing.log" {
					return nil, &xrpc.Error{Code: xrpc.CodeNotFound, Message: "没有日志文件 \"missing.log\""}
				}
				return json.Marshal(xrpc.AgentLogsResult{
					Service: p.Service,
					File:    "satchel-agent.log",
					Items: []xrpc.AgentLogEntry{
						{Time: now.Format(time.RFC3339), Level: "ERROR", Msg: "boom " + p.Grep},
					},
					Total: 3,
				})
			default:
				return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
			}
		}

		sysOut, err := runSvc(t, adminCtx, svc, "server system-info", []string{"hk-1"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sysRes := sysOut.(*SystemInfoResult)
		if sysRes.Server != "hk-1" || sysRes.Hostname != "hk-node" || sysRes.CPUCores != 4 {
			t.Fatalf("system-info 返回不对：%+v", sysRes)
		}

		nicsOut, err := runSvc(t, adminCtx, svc, "server nics", []string{"hk-1"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(nicsOut.(*NICsResult).NICs) != 1 || nicsOut.(*NICsResult).NICs[0].Addrs[0].Scope != "global" {
			t.Fatalf("nics 返回不对：%+v", nicsOut)
		}

		filesOut, err := runSvc(t, adminCtx, svc, "server logs files", []string{"hk-1"}, nil, &command.Page{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		fp := filesOut.(*command.PageResult)
		if fp.Total != 2 || len(fp.Items) != 1 || fp.NextCursor == "" {
			t.Fatalf("logs files 分页不对：%+v", fp)
		}

		logsOut, err := runSvc(t, adminCtx, svc, "server logs list", []string{"hk-1"}, map[string]any{"service": "agent", "level": "warn", "grep": "secret"}, &command.Page{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		lp := logsOut.(*command.PageResult)
		if lp.Total != 3 || len(lp.Items) != 1 || lp.NextCursor == "" {
			t.Fatalf("logs list 分页不对：%+v", lp)
		}

		// 路径穿越与非法参数在主控端即拦截。
		if _, err := runSvc(t, adminCtx, svc, "server logs list", []string{"hk-1"}, map[string]any{"file": "../../etc/shadow"}, nil); v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("路径穿越应当返回 not_found：%v", err)
		}
		if _, err := runSvc(t, adminCtx, svc, "server logs list", []string{"hk-1"}, map[string]any{"file": "missing.log"}, nil); v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("节点回 not_found 应当翻译为 not_found：%v", err)
		}
		if _, err := runSvc(t, adminCtx, svc, "server logs list", []string{"hk-1"}, map[string]any{"service": "systemd"}, nil); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("非法 service 应当返回 bad_request：%v", err)
		}
		if _, err := runSvc(t, adminCtx, svc, "server logs list", []string{"hk-1"}, map[string]any{"service": "nginx", "file": "a.log"}, nil); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("nginx + --file 应当返回 bad_request：%v", err)
		}

		// 旧节点未知方法映射为 conflict；离线节点映射为 unavailable。
		fc.callFn = func(context.Context, int64, string, any, time.Duration) (json.RawMessage, error) {
			return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
		}
		if _, err := runSvc(t, adminCtx, svc, "server system-info", []string{"hk-1"}, nil, nil); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("旧节点未知方法应为 conflict：%v", err)
		}
		fc.callFn = nil
		if _, err := runSvc(t, adminCtx, svc, "server system-info", []string{"hk-1"}, nil, nil); v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("离线节点应为 unavailable：%v", err)
		}

		// 3. traffic-stats set 乐观锁与合法性校验。
		if _, err := runSvc(t, adminCtx, svc, "server traffic-stats set", []string{"hk-1"}, map[string]any{"mode": "max"}, nil); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("缺少 resource-version 应当 bad_request：%v", err)
		}
		if _, err := runSvc(t, adminCtx, svc, "server traffic-stats set", []string{"hk-1"}, map[string]any{"resource-version": 1, "mode": "bad"}, nil); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("非法 mode 应当 bad_request：%v", err)
		}
		if _, err := runSvc(t, adminCtx, svc, "server traffic-stats set", []string{"hk-1"}, map[string]any{"resource-version": 1, "include": "yes"}, nil); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("非法 include 应当 bad_request：%v", err)
		}
		tsOut, err := runSvc(t, adminCtx, svc, "server traffic-stats set", []string{"hk-1"}, map[string]any{"resource-version": 1, "mode": "max", "source": "core", "include": "false"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		updated := tsOut.(*core.Server)
		if updated.Metadata.ResourceVersion != 2 || updated.Spec.TrafficStatsMode != "max" || updated.Spec.TrafficSource != "core" || updated.Spec.IncludeInTrafficStats {
			t.Fatalf("traffic-stats set 结果不对：%+v", updated)
		}
		if _, err := runSvc(t, adminCtx, svc, "server traffic-stats set", []string{"hk-1"}, map[string]any{"resource-version": 1, "mode": "upload"}, nil); v1.AsError(err).Code != v1.CodeVersionConflict {
			t.Fatalf("旧版本号应当 version_conflict：%v", err)
		}
	})
}

func TestSpeedThrottlingSystemBaselineAndPullCollect(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		sRepo := core.New(bdb, st)
		cfgRepo := coresettings.New(bdb, st, schema.Default())
		ctx := context.Background()
		_ = cfgRepo.EnsureSingleton(ctx)
		svc := New(sRepo, cfgRepo, nil, nil)
		fc := &telemetryFakeConns{}
		svc.SetConns(fc)

		wsSrv, _ := sRepo.Create(ctx, "hk-ws", ModeWebsocket)
		pullSrv, _ := sRepo.CreateWith(ctx, core.CreateParams{Name: "hk-pull", Mode: ModePull, PullAddress: "127.0.0.1", PullPort: 23890})
		t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
		_ = sRepo.MarkOnline(ctx, wsSrv.Metadata.ID, "198.51.100.1", false, t0, t0)
		_ = sRepo.MarkOnline(ctx, pullSrv.Metadata.ID, "127.0.0.1", false, t0, t0)

		// 1. 速度 15s 节流落盘 + 内存实时可读 + 系统网卡重启基线（首见不突刺 / 正常增量 / 同开机倒退记0 / 重启续加）。
		svc.IngestSpeed(ctx, wsSrv.Metadata.ID, xrpc.SpeedReport{UploadSpeed: 1000, DownloadSpeed: 2000, SystemRxBytes: 5000, SystemTxBytes: 2000, SystemBootTimeUnix: 100, At: t0}, t0)
		// 3 秒后：内存立即更新为 3000/4000，但库里被 15s 节流保持 1000/2000。
		svc.IngestSpeed(ctx, wsSrv.Metadata.ID, xrpc.SpeedReport{UploadSpeed: 3000, DownloadSpeed: 4000, SystemRxBytes: 8000, SystemTxBytes: 3500, SystemBootTimeUnix: 100, At: t0.Add(3 * time.Second)}, t0.Add(3*time.Second))

		dbMid, _ := sRepo.ByID(ctx, wsSrv.Metadata.ID)
		if dbMid.Status.CurrentUploadSpeed != 1000 || dbMid.Status.CurrentDownloadSpeed != 2000 {
			t.Fatalf("15s 节流期内不应当频繁写库覆盖首帧：%+v", dbMid.Status)
		}

		adminCtx := v1.WithIdentity(ctx, v1.LocalAdmin("admin"))
		spOne, err := runSvc(t, adminCtx, svc, "server speed", []string{"hk-ws"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sv := spOne.(*SpeedView); sv.UploadSpeed != 3000 || sv.DownloadSpeed != 4000 {
			t.Fatalf("server speed 应实时读内存 3000/4000，得到 %+v", sv)
		}

		// 同次开机计数器回退（rx 100 < 8000，tx 50 < 3500）：cycle 不变 (3000, 1500)。
		svc.IngestSpeed(ctx, wsSrv.Metadata.ID, xrpc.SpeedReport{UploadSpeed: 500, DownloadSpeed: 600, SystemRxBytes: 100, SystemTxBytes: 50, SystemBootTimeUnix: 100, At: t0.Add(16 * time.Second)}, t0.Add(16*time.Second))
		// 主机重启（boot=200，rx=400，tx=300）：cycle 增至 (3400, 1800) 且立即落盘。
		svc.IngestSpeed(ctx, wsSrv.Metadata.ID, xrpc.SpeedReport{UploadSpeed: 700, DownloadSpeed: 800, SystemRxBytes: 400, SystemTxBytes: 300, SystemBootTimeUnix: 200, At: t0.Add(17 * time.Second)}, t0.Add(17*time.Second))

		dbAfter, _ := sRepo.ByID(ctx, wsSrv.Metadata.ID)
		if dbAfter.Status.SystemRxCycle != 3400 || dbAfter.Status.SystemTxCycle != 1800 || dbAfter.Status.SystemBootTimeUnix != 200 || dbAfter.Metadata.ResourceVersion != 1 {
			t.Fatalf("系统网卡重启基线不对：rx=%d tx=%d boot=%d rv=%d", dbAfter.Status.SystemRxCycle, dbAfter.Status.SystemTxCycle, dbAfter.Status.SystemBootTimeUnix, dbAfter.Metadata.ResourceVersion)
		}

		// 2. CollectSpeedOnce 仅拉取 pull 节点，跳过 WS 节点。
		fc.connected = []int64{wsSrv.Metadata.ID}
		fc.pullSpeedFn = func(_ context.Context, serverID int64, _ time.Duration) (xrpc.SpeedReport, string, bool, error) {
			if serverID != pullSrv.Metadata.ID {
				t.Errorf("不应当对 WS 节点 %d 发 PullSpeed", serverID)
			}
			return xrpc.SpeedReport{UploadSpeed: 51200, DownloadSpeed: 102400, At: t0.Add(20 * time.Second)}, "127.0.0.1", false, nil
		}
		svc.CollectSpeedOnce(ctx, t0.Add(20*time.Second))

		allOut, err := runSvc(t, adminCtx, svc, "server speed", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		allRes := allOut.(*SpeedListResult)
		if allRes.Total != 2 || allRes.Items[1].UploadSpeed != 51200 || allRes.Items[1].DownloadSpeed != 102400 {
			t.Fatalf("全量 server speed 不对：%+v", allRes)
		}

		// 3. server traffic、server list、DashboardSnapshot 均含最新速率且无凭据泄露。
		tvOut, err := runSvc(t, adminCtx, svc, "server traffic", []string{"hk-ws"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		tv := tvOut.(*TrafficView)
		if tv.SystemRxCycle != 3400 || tv.SystemTxCycle != 1800 || tv.CurrentUploadSpeed != 700 || tv.TrafficSource != core.TrafficSourceSystem {
			t.Fatalf("server traffic 扩展字段不对：%+v", tv)
		}
		dashItems, dashSum, err := svc.DashboardSnapshot(ctx)
		if err != nil || dashSum.TotalServers != 2 || dashSum.ConnectedServers != 2 || dashSum.TotalUploadSpeed != 700+51200 || len(dashItems) != 2 {
			t.Fatalf("DashboardSnapshot 不对：sum=%+v items=%+v err=%v", dashSum, dashItems, err)
		}

		// 4. SweepOffline 离线后内存速率归零。
		if _, err := svc.SweepOffline(30*time.Second, func() time.Time { return t0.Add(60 * time.Second) })(ctx); err != nil {
			t.Fatal(err)
		}
		spAfter, _ := runSvc(t, adminCtx, svc, "server speed", []string{"hk-ws"}, nil, nil)
		if sv := spAfter.(*SpeedView); sv.UploadSpeed != 0 || sv.DownloadSpeed != 0 {
			t.Fatalf("SweepOffline 离线后速度应归零：%+v", sv)
		}
	})
}
