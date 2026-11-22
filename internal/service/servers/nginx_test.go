package servers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type stubHumanVerifier struct {
	expected string
}

func (v *stubHumanVerifier) Verify(_ context.Context, inv *command.Invocation) error {
	if inv == nil || inv.Verify == nil || inv.Verify.Password != v.expected {
		return v1.New(v1.CodeHumanRequired, "当场密码校验失败")
	}
	return nil
}

func TestServerServiceAndNginxCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		repo := core.New(bdb, st)
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(context.Background()); err != nil {
			t.Fatal(err)
		}
		svc := New(repo, setRepo, nil, nil)
		svc.SetVerifier(&stubHumanVerifier{expected: "ok-pass"})

		jobsStarted := 0
		svc.SetStartJob(func(ctx context.Context, serverID int64, inv *command.Invocation, run func(ctx context.Context) (any, error), done func()) (any, error) {
			jobsStarted++
			if done != nil {
				defer done()
			}
			return run(ctx)
		})

		remote, err := repo.Create(context.Background(), "hk-nx", core.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		local, err := repo.Create(context.Background(), "local-nx", core.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.SetSameHost(context.Background(), local.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}

		now := time.Now().UTC().Truncate(time.Second)
		var calledMethods []string
		svc.SetConns(&stubConfigConns{call: func(serverID int64, method string, params any) (json.RawMessage, error) {
			calledMethods = append(calledMethods, method)
			switch method {
			case xrpc.MethodServiceStatus:
				return json.Marshal(xrpc.ServiceStatusResult{
					Kernel:    xrpc.KernelServiceStatus{Running: true, Version: "1.14.4"},
					Nginx:     xrpc.NginxServiceStatus{Installed: true, Running: true, Version: "1.26.2", InDocker: true, InitSystem: "direct", ConfigOK: true},
					Agent:     xrpc.AgentServiceStatus{Running: true, Version: "dev", UptimeSeconds: 120, InDocker: true},
					CheckedAt: now,
				})
			case xrpc.MethodServiceControl:
				p := params.(xrpc.ServiceControlParams)
				return json.Marshal(xrpc.ServiceControlResult{
					Service: p.Service, Action: p.Action, Running: p.Action != "stop", Message: "ok",
				})
			case xrpc.MethodNginxInstall:
				return json.Marshal(xrpc.NginxInstallResult{
					Installed: true, AlreadyInstalled: true, InDocker: true, Version: "1.26.2",
				})
			case xrpc.MethodNginxUninstall:
				return nil, &xrpc.Error{Code: xrpc.CodeConflict, Message: "Docker 容器内置 nginx 不支持卸载"}
			case xrpc.MethodNginxServers:
				return json.Marshal(xrpc.NginxServersResult{
					Servers: []xrpc.NginxServerBlock{
						{Kind: "http", ConfPath: "/etc/nginx/servers/satchel-site-a.example.com.conf", Managed: true, ServerNames: []string{"a.example.com"}, ListenPorts: []string{"80"}},
						{Kind: "stream", ConfPath: "/etc/nginx/stream_servers/master_https_443.conf", Managed: false, ListenPorts: []string{"443"}},
					},
				})
			case xrpc.MethodNginxClearStream:
				p := params.(xrpc.NginxClearStreamParams)
				return json.Marshal(xrpc.NginxClearStreamResult{
					Port: p.Port, RemovedFiles: []string{"/etc/nginx/stream_servers/satchel-stream-8443.conf"}, Reloaded: true,
				})
			case xrpc.MethodNginxWebsitesScan:
				return json.Marshal(xrpc.NginxWebsitesScanResult{
					Sites: []xrpc.ScannedWebsite{{Domain: "a.example.com", Managed: true, ConfPath: "/etc/nginx/servers/satchel-site-a.example.com.conf"}},
				})
			case xrpc.MethodNginxWebsitesApply:
				return json.Marshal(xrpc.NginxWebsitesApplyResult{
					Applied: []xrpc.AppliedWebsiteFile{{Domain: "a.example.com", ConfPath: "/etc/nginx/servers/satchel-site-a.example.com.conf"}}, Reloaded: true,
				})
			default:
				return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
			}
		}})

		adminCtx := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		tokenCtx := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "bot", ActorKind: v1.ActorToken, Role: v1.RoleAdmin,
			Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate},
			Danger: []v1.Danger{v1.DangerRestart, v1.DangerDelete},
		})
		b := svc.Bindings()

		// 1. server service status
		rawStatus, err := b["server service status"](tokenCtx, &command.Invocation{Args: []string{"hk-nx"}})
		if err != nil {
			t.Fatalf("service status failed: %v", err)
		}
		sv := rawStatus.(*ServiceStatusView)
		if !sv.Nginx.Running || sv.Nginx.Version != "1.26.2" || sv.SameHostAsMaster {
			t.Fatalf("unexpected service status: %+v", sv)
		}

		// 2. server service control: remote token reload nginx OK; kernel reload rejected; agent rejected.
		if _, err := b["server service control"](tokenCtx, &command.Invocation{
			Args: []string{"hk-nx"}, Flags: map[string]any{"service": "nginx", "action": "reload"},
		}); err != nil {
			t.Fatalf("remote token nginx reload failed: %v", err)
		}
		if _, err := b["server service control"](tokenCtx, &command.Invocation{
			Args: []string{"hk-nx"}, Flags: map[string]any{"service": "kernel", "action": "reload"},
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("kernel reload must return bad_request, got %v", err)
		}
		if _, err := b["server service control"](tokenCtx, &command.Invocation{
			Args: []string{"hk-nx"}, Flags: map[string]any{"service": "agent", "action": "restart"},
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("agent control must return bad_request, got %v", err)
		}

		// 3. 同机节点 control nginx 触发 DynamicHuman（令牌被拒，管理员带当场密码通过）；同机 control kernel 不触发人验。
		if _, err := b["server service control"](tokenCtx, &command.Invocation{
			Args: []string{"local-nx"}, Flags: map[string]any{"service": "nginx", "action": "reload"},
		}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("same-host token nginx control must return human_required, got %v", err)
		}
		if _, err := b["server service control"](tokenCtx, &command.Invocation{
			Args: []string{"local-nx"}, Flags: map[string]any{"service": "kernel", "action": "restart"},
		}); err != nil {
			t.Fatalf("same-host token kernel restart should succeed without human verify, got %v", err)
		}
		if _, err := b["server service control"](adminCtx, &command.Invocation{
			Args:   []string{"local-nx"},
			Flags:  map[string]any{"service": "nginx", "action": "reload"},
			Verify: &command.Verification{Password: "ok-pass"},
		}); err != nil {
			t.Fatalf("same-host verified admin nginx control failed: %v", err)
		}

		// 4. server nginx install / uninstall：同机令牌在建立长任务前被拒；远程 Docker 节点直答已预装且拒绝卸载。
		beforeJobs := jobsStarted
		if _, err := b["server nginx install"](tokenCtx, &command.Invocation{Args: []string{"local-nx"}}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("same-host token nginx install must return human_required, got %v", err)
		}
		if _, err := b["server nginx uninstall"](tokenCtx, &command.Invocation{Args: []string{"local-nx"}}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("same-host token nginx uninstall must return human_required, got %v", err)
		}
		if jobsStarted != beforeJobs {
			t.Fatalf("rejected same-host nginx install/uninstall must not start jobs, delta=%d", jobsStarted-beforeJobs)
		}

		instRaw, err := b["server nginx install"](tokenCtx, &command.Invocation{Args: []string{"hk-nx"}})
		if err != nil {
			t.Fatalf("remote docker nginx install failed: %v", err)
		}
		iv := instRaw.(*NginxInstallView)
		if !iv.AlreadyInstalled || !iv.InDocker {
			t.Fatalf("unexpected docker install result: %+v", iv)
		}
		if _, err := b["server nginx uninstall"](tokenCtx, &command.Invocation{Args: []string{"hk-nx"}}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("docker nginx uninstall must return conflict, got %v", err)
		}

		// 5. server nginx servers 分页
		srvPageRaw, err := b["server nginx servers"](tokenCtx, &command.Invocation{Args: []string{"hk-nx"}, Page: &command.Page{Limit: 1}})
		if err != nil {
			t.Fatalf("nginx servers failed: %v", err)
		}
		srvPage := srvPageRaw.(*command.PageResult)
		if srvPage.Total != 2 || len(srvPage.Items) != 1 || srvPage.NextCursor == "" {
			t.Fatalf("unexpected pagination: %+v", srvPage)
		}

		// 6. server nginx clear-stream：端口边界、同机人验 + 443 防抢占、远程 8443 成功。
		if _, err := b["server nginx clear-stream"](tokenCtx, &command.Invocation{
			Args: []string{"hk-nx"}, Flags: map[string]any{"port": 0},
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("port 0 must return bad_request, got %v", err)
		}
		if _, err := b["server nginx clear-stream"](tokenCtx, &command.Invocation{
			Args: []string{"local-nx"}, Flags: map[string]any{"port": 8443},
		}); v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("same-host token clear-stream must return human_required, got %v", err)
		}
		if _, err := b["server nginx clear-stream"](adminCtx, &command.Invocation{
			Args:   []string{"local-nx"},
			Flags:  map[string]any{"port": 443},
			Verify: &command.Verification{Password: "ok-pass"},
		}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("same-host clear-stream 443 must return conflict, got %v", err)
		}
		csRaw, err := b["server nginx clear-stream"](tokenCtx, &command.Invocation{
			Args: []string{"hk-nx"}, Flags: map[string]any{"port": 8443},
		})
		if err != nil {
			t.Fatalf("remote clear-stream 8443 failed: %v", err)
		}
		cs := csRaw.(*NginxClearStreamView)
		if !cs.Reloaded || len(cs.RemovedFiles) != 1 || !strings.Contains(cs.RemovedFiles[0], "8443") {
			t.Fatalf("unexpected clear-stream result: %+v", cs)
		}

		// 7. CallNginxWebsitesScan & CallNginxWebsitesApply 封装方法
		scanRes, err := svc.CallNginxWebsitesScan(adminCtx, remote.Metadata.ID)
		if err != nil || len(scanRes.Sites) != 1 {
			t.Fatalf("CallNginxWebsitesScan failed: res=%+v err=%v", scanRes, err)
		}
		applyRes, err := svc.CallNginxWebsitesApply(adminCtx, remote.Metadata.ID, xrpc.NginxWebsitesApplyParams{})
		if err != nil || !applyRes.Reloaded {
			t.Fatalf("CallNginxWebsitesApply failed: res=%+v err=%v", applyRes, err)
		}
	})
}
