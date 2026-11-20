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
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type stubConfigConns struct {
	call func(serverID int64, method string, params any) (json.RawMessage, error)
}

func (s *stubConfigConns) Call(_ context.Context, serverID int64, method string, params any, _ time.Duration) (json.RawMessage, error) {
	return s.call(serverID, method, params)
}

func (s *stubConfigConns) ConnectedIDs() []int64 { return nil }

func TestServerConfigCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		repo := core.New(bdb, st)
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(context.Background()); err != nil {
			t.Fatal(err)
		}
		svc := New(repo, setRepo, nil, nil)
		srv, err := repo.Create(context.Background(), "hk-cfg", core.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := singbox.Render(singbox.ServerInput{Server: srv.Spec})
		if err != nil {
			t.Fatal(err)
		}
		svc.SetConfigRenderer(func(context.Context, int64) (*singbox.Rendered, error) { return rendered, nil })
		if err := repo.MarkAppliedConfig(context.Background(), srv.Metadata.ID, rendered.Hash, 3); err != nil {
			t.Fatal(err)
		}

		now := time.Now().UTC().Truncate(time.Second)
		svc.SetConns(&stubConfigConns{call: func(_ int64, method string, params any) (json.RawMessage, error) {
			switch method {
			case xrpc.MethodConfigStatus:
				return json.Marshal(xrpc.ConfigStatusResult{
					Exists: true, Path: "/etc/satchel-agent/kernel.json", Hash: rendered.Hash,
					Size: int64(len(rendered.Pretty)), Modified: &now, Inbounds: []string{"in-1"}, KernelRunning: true, Generation: 3,
				})
			case xrpc.MethodConfigFiles:
				return json.Marshal(xrpc.ConfigFilesResult{Files: []xrpc.ConfigFileInfo{
					{Scope: "kernel", Path: "/etc/satchel-agent/kernel.json", Name: "kernel.json", Size: 100, Modified: now},
					{Scope: "nginx", Path: "/etc/nginx/nginx.conf", Name: "nginx.conf", Size: 200, Modified: now},
				}})
			case xrpc.MethodConfigRead:
				p := params.(xrpc.ConfigReadParams)
				if strings.HasSuffix(p.Path, "/config.yaml") {
					return nil, &xrpc.Error{Code: xrpc.CodeNotFound, Message: "文件不在白名单内"}
				}
				return json.Marshal(xrpc.ConfigReadResult{
					Scope: "kernel", Path: p.Path, Content: `{"inbounds":[{"users":[{"uuid":"raw-uuid-secret"}]}]}`,
					Size: 54, Modified: now, Hash: "abcd",
				})
			case xrpc.MethodConfigTest:
				return json.Marshal(xrpc.ConfigTestResult{OK: true, Inbounds: []string{"in-1"}, Hash: rendered.Hash})
			}
			return nil, &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
		}})

		adminFull := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		adminNoSecrets := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, Scopes: []v1.Scope{v1.ScopeRead},
		})
		userCtx := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead},
		})

		// status
		stRes, err := svc.Bindings()["server config status"](adminFull, &command.Invocation{Path: []string{"server", "config", "status"}, Args: []string{"hk-cfg"}})
		if err != nil {
			t.Fatal(err)
		}
		if v := stRes.(*ConfigStatusView); !v.InSync || v.Hash != rendered.Hash || v.AppliedGeneration != 3 {
			t.Fatalf("status 不对：%+v", v)
		}

		// files 分页
		fRes, err := svc.Bindings()["server config files"](adminFull, &command.Invocation{
			Path: []string{"server", "config", "files"}, Args: []string{"hk-cfg"}, Flags: map[string]any{"scope": "all"}, Page: &command.Page{Limit: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		if page := fRes.(*command.PageResult); page.Total != 2 || len(page.Items) != 1 || page.NextCursor == "" {
			t.Fatalf("files 分页不对：%+v", page)
		}

		// read：无 secrets 自动打码，有 secrets 见原文，越权路径返回 not_found。
		rMasked, err := svc.Bindings()["server config read"](adminNoSecrets, &command.Invocation{
			Path: []string{"server", "config", "read"}, Args: []string{"hk-cfg"}, Flags: map[string]any{"path": "/etc/satchel-agent/kernel.json"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if v := rMasked.(*ConfigReadView); !v.Redacted || strings.Contains(v.Content, "raw-uuid-secret") {
			t.Fatalf("无 secrets 应打码：%+v", v)
		}
		rRaw, err := svc.Bindings()["server config read"](adminFull, &command.Invocation{
			Path: []string{"server", "config", "read"}, Args: []string{"hk-cfg"}, Flags: map[string]any{"path": "/etc/satchel-agent/kernel.json"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if v := rRaw.(*ConfigReadView); v.Redacted || !strings.Contains(v.Content, "raw-uuid-secret") {
			t.Fatalf("有 secrets 应返回原文：%+v", v)
		}
		if _, err := svc.Bindings()["server config read"](adminFull, &command.Invocation{
			Path: []string{"server", "config", "read"}, Args: []string{"hk-cfg"}, Flags: map[string]any{"path": "/etc/satchel-agent/config.yaml"},
		}); v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("config.yaml 应报 not_found，得到 %v", err)
		}

		// test
		tRes, err := svc.Bindings()["server config test"](adminFull, &command.Invocation{Path: []string{"server", "config", "test"}, Args: []string{"hk-cfg"}})
		if err != nil || !tRes.(*ConfigTestView).OK {
			t.Fatalf("config test 失败：%v %+v", err, tRes)
		}

		// 普通用户全部 forbidden。
		for _, cmd := range []string{"server config status", "server config files", "server config read", "server config test"} {
			if _, err := svc.Bindings()[cmd](userCtx, &command.Invocation{Path: strings.Split(cmd, " "), Args: []string{"hk-cfg"}, Flags: map[string]any{"path": "/etc/satchel-agent/kernel.json"}}); v1.AsError(err).Code != v1.CodeForbidden {
				t.Errorf("普通用户执行 %s 应报 forbidden，得到 %v", cmd, err)
			}
		}
	})
}
