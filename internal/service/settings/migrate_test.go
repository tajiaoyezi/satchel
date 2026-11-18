package settings

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type fakeMigrationHooks struct {
	mu          sync.Mutex
	nodes       []MigrationNodeProbe
	probedURL   string
	pushedURL   string
	pushedNames []string
	pushErr     error
}

func (h *fakeMigrationHooks) ProbeNodes(_ context.Context, candidateURL string) ([]MigrationNodeProbe, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probedURL = candidateURL
	out := make([]MigrationNodeProbe, len(h.nodes))
	copy(out, h.nodes)
	return out, nil
}

func (h *fakeMigrationHooks) PushNodes(_ context.Context, targetURL string, servers []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pushErr != nil {
		return h.pushErr
	}
	h.pushedURL = targetURL
	h.pushedNames = append([]string(nil), servers...)
	return nil
}

type fakeReconnectDir struct {
	tokens map[string]agentconn.Node
}

func (d *fakeReconnectDir) Lookup(_ context.Context, tok string) (agentconn.Node, bool, error) {
	n, ok := d.tokens[tok]
	return n, ok, nil
}
func (d *fakeReconnectDir) Online(context.Context, int64, string, bool, time.Time, time.Time) error {
	return nil
}
func (d *fakeReconnectDir) Offline(context.Context, int64, time.Time) error { return nil }

func TestMasterURLMigrateAndCatchUp(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		repo := coresettings.New(bdb, st, schema.Default())
		if err := repo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		// 设置初始域名与 Turnstile key。
		cur, err := repo.Write(ctx, coresettings.WriteRequest{
			Values: map[string]any{
				"master_url":           "https://old.example.com",
				"turnstile_site_key":   "site-123",
				"turnstile_secret_key": "sec-456",
			},
			ExpectedVersion: 1,
		})
		if err != nil {
			t.Fatal(err)
		}

		svc := New(repo)
		hooks := &fakeMigrationHooks{
			nodes: []MigrationNodeProbe{
				{Server: "hk-1", DBStatus: "connected", State: "ready", LatencyMS: 12},
				{Server: "hk-2", DBStatus: "offline", State: "offline", Pending: false, Error: "offline"},
				{Server: "hk-pend", DBStatus: "pending", State: "offline", Pending: true},
			},
		}
		svc.SetMigration(hooks)

		var currentMaster atomic.Value
		currentMaster.Store(cur.Gates().MasterURL)
		svc.OnWrite(func(st *coresettings.State) {
			currentMaster.Store(st.Gates().MasterURL)
		})

		adminCtx := v1.WithIdentity(ctx, v1.LocalAdmin("root"))
		userCtx := v1.WithIdentity(ctx, v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})
		b := svc.Bindings()

		// 1. 非管理员与非 https URL 被拒。
		if _, err := b["settings master-url migrate"](userCtx, &command.Invocation{Flags: map[string]any{"url": "https://new.example.com"}}); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("普通用户应当返回 forbidden：%v", err)
		}
		if _, err := b["settings master-url migrate"](adminCtx, &command.Invocation{Flags: map[string]any{"url": "http://new.example.com"}}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("非 https URL 应当返回 bad_request：%v", err)
		}

		// 2. --preview 只读返回探测矩阵且不改库/不清空 Turnstile。
		prevOut, err := b["settings master-url migrate"](adminCtx, &command.Invocation{
			Path:  []string{"settings", "master-url", "migrate"},
			Flags: map[string]any{"url": "https://new.example.com", "preview": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		prev := prevOut.(*MasterURLMigrateOutput)
		if !prev.Preview || prev.Ready != 1 || prev.Offline != 2 || prev.CanMigrate {
			t.Fatalf("--preview 结果不对：%+v", prev)
		}
		afterPrev, _ := repo.Load(ctx)
		if afterPrev.Gates().MasterURL != "https://old.example.com" || afterPrev.Gates().TurnstileSiteKey != "site-123" {
			t.Fatalf("--preview 绝不能改数据库：%s %s", afterPrev.Gates().MasterURL, afterPrev.Gates().TurnstileSiteKey)
		}

		// 3. 未传 --skip-unreachable 时被已配对离线节点 hk-2 阻塞返回 conflict，不推送且不改库。
		if _, err := b["settings master-url migrate"](adminCtx, &command.Invocation{
			Path:  []string{"settings", "master-url", "migrate"},
			Flags: map[string]any{"url": "https://new.example.com", "resource-version": int(cur.Version)},
		}); v1.AsError(err).Code != v1.CodeConflict || !strings.Contains(err.Error(), "hk-2") {
			t.Fatalf("有已配对离线节点应当返回 conflict 并点名 hk-2：%v", err)
		}
		if hooks.pushedURL != "" {
			t.Fatalf("阻塞时不应向任何节点推送新地址：%q", hooks.pushedURL)
		}

		// 4. 在线节点握手候选地址不可达时即使加 --skip-unreachable 也阻塞。
		hooks.nodes[0].State = "unreachable"
		if _, err := b["settings master-url migrate"](adminCtx, &command.Invocation{
			Path:  []string{"settings", "master-url", "migrate"},
			Flags: map[string]any{"url": "https://new.example.com", "skip-unreachable": true, "resource-version": int(cur.Version)},
		}); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("在线节点探测不可达即使加 --skip-unreachable 也应当返回 conflict：%v", err)
		}
		hooks.nodes[0].State = "ready"

		// 5. 正式迁移（--skip-unreachable --change-domain）：先推 hk-1 -> 写新 master_url 并清 Turnstile。
		migOut, err := b["settings master-url migrate"](adminCtx, &command.Invocation{
			Path:  []string{"settings", "master-url", "migrate"},
			Flags: map[string]any{"url": "https://new.example.com", "subscription-url": "https://sub.example.com", "skip-unreachable": true, "change-domain": true, "resource-version": int(cur.Version)},
		})
		if err != nil {
			t.Fatal(err)
		}
		mig := migOut.(*MasterURLMigrateOutput)
		if !mig.TurnstileCleared || len(mig.Pushed) != 1 || mig.Pushed[0] != "hk-1" {
			t.Fatalf("正式迁移输出不对：%+v", mig)
		}
		if hooks.pushedURL != "https://new.example.com" || len(hooks.pushedNames) != 1 || hooks.pushedNames[0] != "hk-1" {
			t.Fatalf("PushNodes 未推向 hk-1：%q %v", hooks.pushedURL, hooks.pushedNames)
		}
		finalSt, _ := repo.Load(ctx)
		if finalSt.Gates().MasterURL != "https://new.example.com" || finalSt.Gates().TurnstileSiteKey != "" || finalSt.Gates().TurnstileSecretKey != "" {
			t.Fatalf("迁移后应当更新 master_url 并清空 Turnstile：%+v", finalSt.Gates())
		}

		// 6. 被跳过的离线节点 hk-2 后续持旧地址 https://old.example.com 重连时，agentconn 自动补推 agent.update_master_url。
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		mgr := agentconn.New(agentconn.Config{
			Identity:         priv,
			Directory:        &fakeReconnectDir{tokens: map[string]agentconn.Node{"tok-hk2": {ID: 2, Name: "hk-2"}}},
			CurrentMasterURL: func(context.Context) string { return currentMaster.Load().(string) },
		})
		t.Cleanup(func() { mgr.Shutdown(context.Background()) })
		wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			mgr.Serve(r.Context(), ws, "203.0.113.20")
		}))
		t.Cleanup(wsSrv.Close)

		wsURL := "ws" + wsSrv.URL[len("http"):]
		ws, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ws.CloseNow() })
		xc := xrpc.NewConn(ws)
		if err := xrpc.ClientHandshake(ctx, xc, pub); err != nil {
			t.Fatal(err)
		}
		if err := xc.Write(ctx, xrpc.TypeAuth, xrpc.Auth{
			Token:           "tok-hk2",
			AgentVersion:    "0.2.0",
			ControlProtocol: xrpc.ControlProtocol,
			MasterURL:       "https://old.example.com",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := xc.Read(ctx); err != nil {
			t.Fatal(err)
		}
		pushedCh := make(chan string, 1)
		go func() {
			msg, err := xc.Read(ctx)
			if err != nil {
				return
			}
			if msg.Type == xrpc.TypeCall {
				var call xrpc.Call
				_ = xrpc.Decode(msg, &call)
				if call.Method == xrpc.MethodAgentUpdateMaster {
					var p xrpc.UpdateMasterParams
					_ = json.Unmarshal(call.Params, &p)
					pushedCh <- p.MasterURL
					res, _ := json.Marshal(xrpc.UpdateMasterAck{MasterURL: p.MasterURL, UpdatedAt: time.Now().UTC()})
					_ = xc.Write(ctx, xrpc.TypeReply, xrpc.Reply{ID: call.ID, Result: res})
				}
			}
		}()
		select {
		case got := <-pushedCh:
			if got != "https://new.example.com" {
				t.Fatalf("重连自动补推的 master_url 不对：%q", got)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("hk-2 持旧地址重连后未收到自动补推的 agent.update_master_url")
		}
	})
}
