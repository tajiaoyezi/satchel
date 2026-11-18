package servers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
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

type fakeTokenConns struct {
	mu       sync.Mutex
	callErr  error
	begin    []string
	confirm  int
	invDrops int
	invRoles [][]string
	pullSnap xrpc.TrafficSnapshot
	pullErr  error
	wsIDs    []int64
}

func (f *fakeTokenConns) Call(context.Context, int64, string, any, time.Duration) (json.RawMessage, error) {
	f.mu.Lock()
	err := f.callErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return json.Marshal(xrpc.TokenRotateAck{UpdatedAt: time.Now().UTC()})
}

func (f *fakeTokenConns) ConnectedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.wsIDs...)
}

func (f *fakeTokenConns) BeginTokenWindow(_ int64, _ agentconn.Node, oldToken string) {
	f.mu.Lock()
	f.begin = append(f.begin, oldToken)
	f.mu.Unlock()
}

func (f *fakeTokenConns) ConfirmTokenWindow(int64) {
	f.mu.Lock()
	f.confirm++
	f.mu.Unlock()
}

func (f *fakeTokenConns) InvalidateServer(_ int64, dropUplink bool, roles ...string) {
	f.mu.Lock()
	if dropUplink {
		f.invDrops++
	}
	if len(roles) > 0 {
		f.invRoles = append(f.invRoles, roles)
	}
	f.mu.Unlock()
}

func (f *fakeTokenConns) PullTraffic(context.Context, int64, time.Duration) (xrpc.TrafficSnapshot, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pullSnap, "198.51.100.7", false, f.pullErr
}

// 凭据轮换超时收敛、局部轮换/吊销、pending 吊销规则，以及拉取与 HTTP 上报汇聚同一累计器。
func TestTokensRotateTimeoutRevokeAndUnifiedIngest(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		st := store.New(bdb, schema.Default())
		settings := coresettings.New(bdb, st, schema.Default())
		if err := settings.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		repo := core.New(bdb, st)
		svc := New(repo, settings, pub, nil)

		srv, err := repo.CreateWith(ctx, core.CreateParams{Name: "hk-pending", Mode: ModeAuto, PullAddress: "198.51.100.7"})
		if err != nil {
			t.Fatal(err)
		}
		fc := &fakeTokenConns{}
		svc.SetConns(fc)

		// 1. 非法 --which 报错。
		if _, err := svc.tokenRotate(ctx, &command.Invocation{Args: []string{"hk-pending"}, Flags: map[string]any{"which": "bogus"}}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("非法 --which 应当返 bad_request：%v", err)
		}

		// 2. pending 服务器吊销不置 revoke_pending。
		res, err := svc.tokenRevoke(ctx, &command.Invocation{Args: []string{"hk-pending"}, Flags: map[string]any{"which": "token"}})
		if err != nil {
			t.Fatal(err)
		}
		if res.(*core.Server).Status.RevokePending {
			t.Fatal("从未上线过的 pending 服务器吊销时不应当置 revoke_pending")
		}

		// 3. 节点 ACK 超时：保留新 token 与 rotation_pending=true，保留双凭据过渡窗不关闭。
		_ = repo.MarkOnline(ctx, srv.Metadata.ID, "198.51.100.7", false, time.Time{}, time.Now().UTC())
		fc.callErr = xrpc.ErrTimeout
		oldTok := res.(*core.Server).Status.Token.Reveal()
		rotRes, err := svc.tokenRotate(ctx, &command.Invocation{Args: []string{"hk-pending"}, Flags: map[string]any{"which": "token"}})
		if err != nil {
			t.Fatalf("确认超时应当返回带 rotation_pending 的状态而非回滚报错：%v", err)
		}
		rotSrv := rotRes.(*core.Server)
		if !rotSrv.Status.RotationPending || rotSrv.Status.Token.Reveal() == oldTok {
			t.Fatalf("确认超时应当保持 rotation_pending=true 且写入新 token：%+v", rotSrv.Status)
		}
		if len(fc.begin) != 1 || fc.begin[0] != oldTok || fc.confirm != 0 {
			t.Fatalf("超时不应提前关掉内存双凭据过渡窗口：begin=%v confirm=%d", fc.begin, fc.confirm)
		}

		// 4. 仅轮换 pull 凭据时节点显式返回 ErrOffline 应当完整回滚，不留脏凭据。
		fc.callErr = agentconn.ErrOffline
		prevPull := rotSrv.Status.PullToken.Reveal()
		if _, err := svc.tokenRotate(ctx, &command.Invocation{Args: []string{"hk-pending"}, Flags: map[string]any{"which": "pull"}}); v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("不可达应当返回 unavailable：%v", err)
		}
		afterRB, _ := repo.ByID(ctx, srv.Metadata.ID)
		if afterRB.Status.PullToken.Reveal() != prevPull {
			t.Fatal("不可达回滚后 pull_token 必须恢复原值")
		}

		// 5. pull 服务器经 CollectTraffic 从轮询口拉取快照并写入 MarkPullSuccess，再与 HTTP 上报并入同一累计器。
		pullSrv, err := repo.CreateWith(ctx, core.CreateParams{Name: "hk-pull", Mode: ModePull, PullAddress: "198.51.100.7"})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		fc.pullSnap = xrpc.TrafficSnapshot{
			BootID: "boot-p", Generation: 1, At: now, KernelRunning: true, KernelVersion: "1.14.4",
			Counts: []xrpc.TrafficCount{{Inbound: "in", Outbound: "direct", Credential: "u1", Down: 60 << 20}},
		}
		detail, err := svc.CollectTraffic(func() time.Time { return now })(ctx)
		if err != nil || detail != "拉了 1 台、失败 0 台" {
			t.Fatalf("pull 拉取详情不对：%q %v", detail, err)
		}
		pulled, _ := repo.ByID(ctx, pullSrv.Metadata.ID)
		if pulled.Status.Status != core.StatusConnected || pulled.Status.LastPullAt == nil {
			t.Fatalf("pull 拉取成功后应更新 connected 与 last_pull_at：%+v", pulled.Status)
		}
		// 同一进程继续收到经 HTTP 上报的增量（总 100 MB）：单调累加到 100 MB。
		if err := svc.IngestSnapshot(ctx, pullSrv.Metadata.ID, xrpc.TrafficSnapshot{
			BootID: "boot-p", Generation: 1, At: now.Add(time.Second), KernelRunning: true, KernelVersion: "1.14.4",
			Counts: []xrpc.TrafficCount{{Inbound: "in", Outbound: "direct", Credential: "u1", Down: 100 << 20}},
		}, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		trRes, err := svc.traffic(ctx, &command.Invocation{Args: []string{"hk-pull"}})
		if err != nil {
			t.Fatal(err)
		}
		tv := trRes.(*TrafficView)
		if len(tv.Traffic) != 1 || tv.Traffic[0].Downlink != 100<<20 {
			t.Fatalf("统一累计应当达到 100 MB：%+v", tv.Traffic)
		}
		_ = errors.Is(err, context.Canceled)
	})
}
