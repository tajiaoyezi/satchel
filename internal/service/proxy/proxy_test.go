package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	corebatchin "github.com/satchel/satchel/internal/core/batchinbounds"
	corebatchout "github.com/satchel/satchel/internal/core/batchoutbounds"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	corepresets "github.com/satchel/satchel/internal/core/routingpresets"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	coreusers "github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type stubPlanner struct {
	lastSpec      string
	lastBatchKind string
	lastBatchID   string
}

func (p *stubPlanner) CreatePlanFromJSON(_ context.Context, specJSON, batchKind, batchID string) (any, error) {
	p.lastSpec = specJSON
	p.lastBatchKind = batchKind
	p.lastBatchID = batchID
	return map[string]any{"plan_id": int64(99), "batch_id": batchID}, nil
}

type stubProber struct {
	lastServerID int64
	lastParams   xrpc.OutboundProbeParams
	res          *xrpc.OutboundProbeResult
	err          error
}

func (p *stubProber) CallOutboundProbe(_ context.Context, serverID int64, params xrpc.OutboundProbeParams) (*xrpc.OutboundProbeResult, error) {
	p.lastServerID = serverID
	p.lastParams = params
	return p.res, p.err
}

func TestProxyServiceAllCommandsAndPermissions(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		srvRepo := coreservers.New(bdb, st)
		inRepo := coreinbounds.New(bdb, st)
		outRepo := coreoutbounds.New(bdb, st)
		rrRepo := corerouting.New(bdb, st)
		batchInRepo := corebatchin.New(bdb, st)
		batchOutRepo := corebatchout.New(bdb, st)
		presetRepo := corepresets.New(bdb, st)
		userRepo := coreusers.New(bdb, st)
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(context.Background()); err != nil {
			t.Fatal(err)
		}

		// 准备一个管理员账号用于 routing preset 外键归属。
		adminModel := &model.User{Username: "admin", PasswordHash: "h", Role: "admin", IsActive: true}
		if err := st.Insert(context.Background(), adminModel); err != nil {
			t.Fatal(err)
		}

		planner := &stubPlanner{}
		prober := &stubProber{res: &xrpc.OutboundProbeResult{
			Tag: "out-1", URL: "http://127.0.0.1/204", OK: true, LatencyMS: 28, StatusCode: 204, ProbedAt: time.Now().UTC(),
		}}
		svc := New(Deps{
			Servers:        srvRepo,
			Inbounds:       inRepo,
			Outbounds:      outRepo,
			RoutingRules:   rrRepo,
			BatchInbounds:  batchInRepo,
			BatchOutbounds: batchOutRepo,
			RoutingPresets: presetRepo,
			Users:          userRepo,
			Settings:       setRepo,
			Planner:        planner,
			Nodes:          prober,
		})
		b := svc.Bindings()

		adminFull := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		adminNoSec := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate},
		})
		userCtx := v1.WithIdentity(context.Background(), v1.Identity{
			Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead},
		})

		s1, err := srvRepo.CreateWith(adminFull, coreservers.CreateParams{Name: "hk-1", Mode: coreservers.ModeAuto, Domain: "hk1.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srvRepo.CreateWith(adminFull, coreservers.CreateParams{Name: "jp-land", Mode: coreservers.ModeAuto, Domain: "jp.example.com"}); err != nil {
			t.Fatal(err)
		}

		// 1. inbound protocols / examples / keygen / build
		if _, err := b["inbound protocols"](adminFull, &command.Invocation{}); err != nil {
			t.Fatal(err)
		}
		if _, err := b["inbound examples"](adminFull, &command.Invocation{Flags: map[string]any{"protocol": "vless"}}); err != nil {
			t.Fatal(err)
		}
		kgRaw, err := b["inbound keygen"](adminFull, &command.Invocation{Flags: map[string]any{"type": "x25519"}})
		if err != nil {
			t.Fatal(err)
		}
		kg := kgRaw.(*singbox.KeygenResult)
		if kg.RealityPrivateKey == "" || kg.RealityPublicKey == "" {
			t.Fatalf("keygen missing reality keypair: %+v", kg)
		}
		buildRaw, err := b["inbound build"](adminFull, &command.Invocation{Flags: map[string]any{
			"server-name": "jp-land", "protocol": "vless", "port": 18443, "security": "reality", "sni": "www.microsoft.com",
		}})
		if err != nil {
			t.Fatal(err)
		}
		bm := buildRaw.(map[string]any)
		builtCreds := bm["credentials"].(singbox.BuildCredentials)

		// 写入一个真实 VLESS Reality 入站到 jp-land，供 inbound get 派生公钥与 outbound landing plan 跨机派生测试。
		var jpIn *coreinbounds.Inbound
		if err := bdb.RunInTx(adminFull, nil, func(ctx context.Context, tx bun.Tx) error {
			specJSON, _ := json.Marshal(bm["spec"])
			var spec v1.InboundSpec
			_ = json.Unmarshal(specJSON, &spec)
			// SecretJSON Unmarshal preserves raw bytes via Reveal
			var rawSpecMap map[string]json.RawMessage
			_ = json.Unmarshal(specJSON, &rawSpecMap)
			spec.TLS = v1.SecretJSON(rawSpecMap["tls"])
			spec.Settings = v1.SecretJSON(rawSpecMap["settings"])
			var cerr error
			jpIn, cerr = inRepo.CreateTx(ctx, tx, spec)
			return cerr
		}); err != nil {
			t.Fatal(err)
		}

		// inbound get: 派生公钥存在，且无 secrets 令牌看到打码。
		igFull := mustCall(t, b["inbound get"], adminFull, &command.Invocation{Args: []string{itoa(jpIn.Metadata.ID)}}).(InboundView)
		if igFull.Derived == nil || igFull.Derived.RealityPublicKey != builtCreds.RealityPublicKey {
			t.Fatalf("expected derived reality_public_key=%q, got %+v", builtCreds.RealityPublicKey, igFull.Derived)
		}
		igMask := mustCall(t, b["inbound get"], adminNoSec, &command.Invocation{Args: []string{itoa(jpIn.Metadata.ID)}}).(InboundView)
		if string(igMask.Spec.TLS) != `"***"` || string(igMask.Spec.Settings) != `"***"` {
			t.Fatalf("expected masked tls/settings for non-secrets token: tls=%s settings=%s", igMask.Spec.TLS, igMask.Spec.Settings)
		}

		// 2. inbound batch plan
		if _, err := b["inbound batch plan"](adminFull, &command.Invocation{Flags: map[string]any{
			"servers": []string{"hk-1", "jp-land"}, "protocol": "trojan", "port": 28443, "security": "tls",
		}}); err != nil {
			t.Fatal(err)
		}
		if planner.lastBatchKind != "Inbound" || !strings.HasPrefix(planner.lastBatchID, "batch-") {
			t.Fatalf("unexpected batch meta: kind=%q id=%q", planner.lastBatchKind, planner.lastBatchID)
		}

		// 3. outbound probe 更新状态且不抬升 resourceVersion
		var ob *coreoutbounds.Outbound
		if err := bdb.RunInTx(adminFull, nil, func(ctx context.Context, tx bun.Tx) error {
			var cerr error
			ob, cerr = outRepo.CreateTx(ctx, tx, v1.OutboundSpec{
				ServerID: s1.Metadata.ID, Tag: "out-1", Protocol: "direct",
			})
			return cerr
		}); err != nil {
			t.Fatal(err)
		}
		pr := mustCall(t, b["outbound probe"], adminFull, &command.Invocation{
			Args:  []string{itoa(ob.Metadata.ID)},
			Flags: map[string]any{"url": "http://127.0.0.1/204"},
		}).(*OutboundProbeView)
		if !pr.OK || pr.LatencyMS != 28 {
			t.Fatalf("probe result mismatch: %+v", pr)
		}
		obAfter := mustCall(t, b["outbound get"], adminFull, &command.Invocation{Args: []string{itoa(ob.Metadata.ID)}}).(OutboundView)
		if obAfter.Metadata.ResourceVersion != 1 || !obAfter.Status.LastProbeOk {
			t.Fatalf("outbound probe should update status without bumping resourceVersion: ver=%d status=%+v", obAfter.Metadata.ResourceVersion, obAfter.Status)
		}

		// 4. outbound landing plan 跨机自动派生 VLESS Reality 客户端出站 + 路由规则
		if _, err := b["outbound landing plan"](adminFull, &command.Invocation{Flags: map[string]any{
			"server-name":    "hk-1",
			"outbound-tag":   "land-jp",
			"inbound-tag":    "in-src",
			"target-server":  "jp-land",
			"target-inbound": jpIn.Spec.Tag,
			"detour":         "out-1",
		}}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(planner.lastSpec, builtCreds.RealityPublicKey) || !strings.Contains(planner.lastSpec, `"detour":"out-1"`) || !strings.Contains(planner.lastSpec, `"jp.example.com"`) {
			t.Fatalf("landing plan missing derived fields: %s", planner.lastSpec)
		}

		// 5. routing list 遮蔽检测 + routing preset builtin/save/list/delete
		if err := bdb.RunInTx(adminFull, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := rrRepo.CreateTx(ctx, tx, v1.RoutingRuleSpec{
				ServerID: s1.Metadata.ID, SortOrder: 10, Match: json.RawMessage(`{"inbound":["in-1"]}`), OutboundTag: "out-1", Enabled: true,
			}); err != nil {
				return err
			}
			_, err := rrRepo.CreateTx(ctx, tx, v1.RoutingRuleSpec{
				ServerID: s1.Metadata.ID, SortOrder: 20, Match: json.RawMessage(`{"inbound":["in-1"],"domain":["example.com"]}`), OutboundTag: "direct", Enabled: true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		rlPage := mustCall(t, b["routing list"], adminFull, &command.Invocation{Flags: map[string]any{"server-name": "hk-1"}}).(*command.PageResult)
		if rlPage.Total != 2 {
			t.Fatalf("expected 2 routing rules, got %d", rlPage.Total)
		}
		r1 := rlPage.Items[0].(RoutingRuleView)
		r2 := rlPage.Items[1].(RoutingRuleView)
		if !r1.IsInboundCatchAll || len(r2.ShadowedBy) != 1 || r2.ShadowedBy[0] != r1.Metadata.ID {
			t.Fatalf("shadow analysis wrong: r1=%+v r2=%+v", r1, r2)
		}

		if _, err := b["routing preset builtin"](adminFull, &command.Invocation{}); err != nil {
			t.Fatal(err)
		}
		savedPreset := mustCall(t, b["routing preset save"], adminFull, &command.Invocation{Flags: map[string]any{
			"name": "my-rule", "rule": `{"domain_suffix":["example.org"]}`,
		}}).(*corepresets.Preset)
		if savedPreset.Username != "admin" {
			t.Fatalf("local admin preset should fall back to admin user, got %q", savedPreset.Username)
		}
		if _, err := b["routing preset delete"](adminFull, &command.Invocation{Args: []string{itoa(savedPreset.ID)}}); err != nil {
			t.Fatal(err)
		}

		// 6. 普通用户全部 forbidden
		for name, fn := range b {
			if _, err := fn(userCtx, &command.Invocation{Args: []string{"1"}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeForbidden {
				t.Errorf("普通用户执行 %s 应报 forbidden，得到 %v", name, err)
			}
		}
	})
}

func mustCall(t *testing.T, fn command.Handler, ctx context.Context, inv *command.Invocation) any {
	t.Helper()
	res, err := fn(ctx, inv)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	return res
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestBatchPlanServersArraySemantics(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		srvRepo := coreservers.New(bdb, st)
		inRepo := coreinbounds.New(bdb, st)
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		planner := &stubPlanner{}
		svc := New(Deps{
			Servers:  srvRepo,
			Inbounds: inRepo,
			Settings: setRepo,
			Planner:  planner,
		})
		b := svc.Bindings()
		admin := v1.WithIdentity(ctx, v1.LocalAdmin("root"))

		sComma, err := srvRepo.CreateWith(admin, coreservers.CreateParams{Name: "hk,1", Mode: coreservers.ModeAuto})
		if err != nil {
			t.Fatal(err)
		}
		sPlain, err := srvRepo.CreateWith(admin, coreservers.CreateParams{Name: "jp-land", Mode: coreservers.ModeAuto})
		if err != nil {
			t.Fatal(err)
		}

		parseServerIDs := func(specJSON string) []int64 {
			t.Helper()
			var ops []struct {
				Spec struct {
					ServerID int64 `json:"server_id"`
				} `json:"spec"`
			}
			if err := json.Unmarshal([]byte(specJSON), &ops); err != nil {
				t.Fatalf("unmarshal ops failed: %v (%s)", err, specJSON)
			}
			ids := make([]int64, 0, len(ops))
			for _, op := range ops {
				ids = append(ids, op.Spec.ServerID)
			}
			return ids
		}

		// 1. 名字带逗号的服务器在 inbound batch plan 与 outbound batch plan 中都能按原样匹配，并按 ID 去重。
		if _, err := b["inbound batch plan"](admin, &command.Invocation{Flags: map[string]any{
			"servers": []string{"hk,1", itoa(sComma.Metadata.ID), "jp-land"}, "protocol": "trojan", "port": 28443, "security": "tls",
		}}); err != nil {
			t.Fatalf("inbound batch plan should match comma server name verbatim: %v", err)
		}
		gotIn := parseServerIDs(planner.lastSpec)
		if len(gotIn) != 2 || gotIn[0] != sComma.Metadata.ID || gotIn[1] != sPlain.Metadata.ID {
			t.Fatalf("inbound batch plan expected server IDs [%d %d], got %v", sComma.Metadata.ID, sPlain.Metadata.ID, gotIn)
		}

		if _, err := b["outbound batch plan"](admin, &command.Invocation{Flags: map[string]any{
			"servers": []string{"hk,1"}, "tag": "direct-out", "protocol": "direct",
		}}); err != nil {
			t.Fatalf("outbound batch plan should match comma server name verbatim: %v", err)
		}
		gotOut := parseServerIDs(planner.lastSpec)
		if len(gotOut) != 1 || gotOut[0] != sComma.Metadata.ID {
			t.Fatalf("outbound batch plan expected server IDs [%d], got %v", sComma.Metadata.ID, gotOut)
		}

		// 2. 不再按逗号拆元素：把两台服务器拼成单个逗号串应报 not_found。
		if _, err := b["inbound batch plan"](admin, &command.Invocation{Flags: map[string]any{
			"servers": []string{"hk,1,jp-land"}, "protocol": "trojan", "port": 28443, "security": "tls",
		}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("comma-joined --servers should not be split, expected not_found, got %v", err)
		}
		if _, err := b["outbound batch plan"](admin, &command.Invocation{Flags: map[string]any{
			"servers": []string{"hk,1,jp-land"}, "tag": "direct-out", "protocol": "direct",
		}}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("comma-joined --servers should not be split for outbound, expected not_found, got %v", err)
		}

		// 3. 数组与 --all 互斥、空数组、空白项均报 bad_request（与 m2-10 server update batch 一致）。
		badCases := []struct {
			name  string
			flags map[string]any
		}{
			{"neither servers nor all", map[string]any{"tag": "direct-out", "protocol": "direct"}},
			{"servers and all both set", map[string]any{"servers": []string{"hk,1"}, "all": true, "tag": "direct-out", "protocol": "direct"}},
			{"empty servers with all", map[string]any{"servers": []string{}, "all": true, "tag": "direct-out", "protocol": "direct"}},
			{"empty servers", map[string]any{"servers": []string{}, "tag": "direct-out", "protocol": "direct"}},
			{"blank item in servers", map[string]any{"servers": []string{"hk,1", "   "}, "tag": "direct-out", "protocol": "direct"}},
		}
		for _, tc := range badCases {
			if _, err := b["outbound batch plan"](admin, &command.Invocation{Flags: tc.flags}); v1.AsError(err) == nil || v1.AsError(err).Code != v1.CodeBadRequest {
				t.Fatalf("%s: expected bad_request, got %v", tc.name, err)
			}
		}
	})
}
