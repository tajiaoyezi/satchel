package websites

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type stubScanner struct {
	res *xrpc.NginxWebsitesScanResult
	err error
}

func (s *stubScanner) CallNginxWebsitesScan(_ context.Context, _ int64) (*xrpc.NginxWebsitesScanResult, error) {
	return s.res, s.err
}

func TestWebsiteServiceCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		srvRepo := coreservers.New(bdb, st)
		webRepo := corewebsites.New(bdb, st)
		scanner := &stubScanner{}
		svc := New(Deps{Servers: srvRepo, Websites: webRepo, Scanner: scanner})
		b := svc.Bindings()

		adminCtx := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		userCtx := v1.WithIdentity(context.Background(), v1.Identity{Actor: "u1", ActorKind: v1.ActorUser, Role: v1.RoleUser})

		// 非管理员一律 forbidden
		if _, err := b["website templates"](userCtx, &command.Invocation{}); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("non-admin must be forbidden, got %v", err)
		}

		// 1. website templates
		tmplRaw, err := b["website templates"](adminCtx, &command.Invocation{})
		if err != nil {
			t.Fatalf("website templates failed: %v", err)
		}
		tv := tmplRaw.(*WebsiteTemplatesView)
		if len(tv.Templates) < 7 {
			t.Fatalf("expected >=7 templates, got %d", len(tv.Templates))
		}

		remote, err := srvRepo.Create(context.Background(), "hk-web", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		local, err := srvRepo.Create(context.Background(), "local-web", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		if err := srvRepo.SetSameHost(context.Background(), local.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}

		// 2. website build: WSS proxy, decoy, injection rejection, same-host 443 rejection
		wssRaw, err := b["website build"](adminCtx, &command.Invocation{
			Flags: map[string]any{
				"server-name": "hk-web",
				"domain":      "WSS.Example.COM",
				"type":        "proxy",
				"target":      "https://127.0.0.1:10000",
				"wss-path":    "/vless-ws",
			},
		})
		if err != nil {
			t.Fatalf("website build wss failed: %v", err)
		}
		bv := wssRaw.(*WebsiteBuildView)
		if bv.Spec.Domain != "wss.example.com" || bv.Spec.Target != "wss://127.0.0.1:10000/vless-ws" ||
			bv.PlanOp.Op != "create" || bv.PlanOp.Kind != "Website" ||
			!strings.Contains(bv.PreviewConf, "Upgrade $http_upgrade") {
			t.Fatalf("unexpected wss build view: %+v", bv)
		}

		// 注入字符拦截
		if _, err := b["website build"](adminCtx, &command.Invocation{
			Flags: map[string]any{
				"server-name": "hk-web",
				"domain":      "inj.example.com",
				"type":        "proxy",
				"target":      "http://127.0.0.1:8080; return 200;",
			},
		}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("injection target must return bad_request, got %v", err)
		}

		// 同机 443 拦截
		if _, err := b["website build"](adminCtx, &command.Invocation{
			Flags: map[string]any{
				"server-name": "local-web",
				"domain":      "tls.example.com",
				"type":        "decoy",
				"target":      "landing",
				"use-443":     true,
			},
		}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("same-host use-443 must return conflict, got %v", err)
		}

		// 同机非 443 返回 requires_human=true
		localRaw, err := b["website build"](adminCtx, &command.Invocation{
			Flags: map[string]any{
				"server-name": "local-web",
				"domain":      "decoy.example.com",
				"type":        "decoy",
				"target":      "blog",
			},
		})
		if err != nil || !localRaw.(*WebsiteBuildView).RequiresHuman || localRaw.(*WebsiteBuildView).HTMLPath == "" {
			t.Fatalf("unexpected local decoy build: res=%+v err=%v", localRaw, err)
		}

		// 3. 写入一条 Website，测 list / get / scan（已纳管不抬版本 + 未纳管绝不落库）
		var created *corewebsites.Website
		if err := bdb.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
			var cerr error
			created, cerr = webRepo.CreateTx(ctx, tx, bv.Spec)
			return cerr
		}); err != nil {
			t.Fatal(err)
		}

		listRaw, err := b["website list"](adminCtx, &command.Invocation{
			Flags: map[string]any{"server-name": "hk-web", "type": "proxy"},
		})
		if err != nil {
			t.Fatalf("website list failed: %v", err)
		}
		lr := listRaw.(*command.PageResult)
		if lr.Total != 1 || len(lr.Items) != 1 {
			t.Fatalf("expected 1 website in list, got %+v", lr)
		}

		getRaw, err := b["website get"](adminCtx, &command.Invocation{Args: []string{"1"}})
		if err != nil {
			t.Fatalf("website get failed: %v", err)
		}
		if getRaw.(WebsiteView).Spec.Domain != "wss.example.com" || getRaw.(WebsiteView).ServerName != "hk-web" {
			t.Fatalf("unexpected get view: %+v", getRaw)
		}

		scanner.res = &xrpc.NginxWebsitesScanResult{
			Sites: []xrpc.ScannedWebsite{
				{Domain: "wss.example.com", ConfPath: "/etc/nginx/servers/satchel-site-wss.example.com.conf", Managed: true, Type: "proxy", Target: "https://127.0.0.1:10000", ListenPorts: []string{"80"}},
				{Domain: "legacy.example.com", ConfPath: "/etc/nginx/servers/manual.conf", Managed: false, Type: "static", ListenPorts: []string{"80"}},
			},
		}
		scanRaw, err := b["website scan"](adminCtx, &command.Invocation{Args: []string{"hk-web"}})
		if err != nil {
			t.Fatalf("website scan failed: %v", err)
		}
		sv := scanRaw.(*WebsiteScanView)
		if len(sv.Managed) != 1 || len(sv.Unmanaged) != 1 || sv.Unmanaged[0].Domain != "legacy.example.com" {
			t.Fatalf("unexpected scan view: %+v", sv)
		}
		// 核验：resourceVersion 不抬升，且未纳管域名未落库
		reloaded, err := webRepo.ByID(context.Background(), created.Metadata.ID)
		if err != nil || reloaded.Metadata.ResourceVersion != 1 || !reloaded.Status.Managed || reloaded.Status.ConfPath != "/etc/nginx/servers/satchel-site-wss.example.com.conf" {
			t.Fatalf("scan must update status without bumping version: obj=%+v err=%v", reloaded, err)
		}
		allAfterScan, err := webRepo.ListByServer(context.Background(), remote.Metadata.ID)
		if err != nil || len(allAfterScan) != 1 {
			t.Fatalf("unmanaged site must never be inserted into websites table, got len=%d err=%v", len(allAfterScan), err)
		}
	})
}
