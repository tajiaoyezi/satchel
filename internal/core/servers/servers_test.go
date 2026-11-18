package servers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func repo(bdb *bun.DB) *Repo { return New(bdb, store.New(bdb, schema.Default())) }

func ts(sec int) time.Time {
	return time.Date(2026, 10, 24, 12, 0, sec, 0, time.UTC)
}

// 建服务器：最小 spec、43 字符的 base64url token、pending、resourceVersion 1、「省略即为真」的两个字段为真；名字重复 name_taken。
func TestCreate(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r, ctx := repo(bdb), context.Background()
		s, err := r.Create(ctx, "hk-1", "websocket")
		if err != nil {
			t.Fatal(err)
		}
		tok := s.Status.Token.Reveal()
		atok := s.Status.AgentToken.Reveal()
		ptok := s.Status.PullToken.Reveal()
		for label, v := range map[string]string{"token": tok, "agent_token": atok, "pull_token": ptok} {
			if raw, err := base64.RawURLEncoding.DecodeString(v); len(v) != 43 || err != nil || len(raw) != 32 {
				t.Fatalf("%s 应当是 32 字节的 base64url（43 个字符）：%q %v", label, v, err)
			}
		}
		if tok == atok || tok == ptok || atok == ptok {
			t.Fatalf("三把凭据应当互不相同：%q %q %q", tok, atok, ptok)
		}
		if s.Spec.Name != "hk-1" || s.Spec.ConnectionMode != "websocket" || s.Status.Status != StatusPending ||
			s.Metadata.ResourceVersion != 1 || !s.Spec.Ipv6Enabled || !s.Spec.IncludeInTrafficStats || s.Metadata.ID == 0 {
			t.Fatalf("新服务器的字段不对：%+v", s)
		}
		out, _ := json.Marshal(s)
		for _, v := range []string{tok, atok, ptok} {
			if strings.Contains(string(out), v) {
				t.Fatalf("序列化时凭据应当打码：%s", out)
			}
		}
		if !strings.Contains(string(out), `"token":"***"`) || !strings.Contains(string(out), `"agent_token":"***"`) || !strings.Contains(string(out), `"pull_token":"***"`) || s.Metadata.Name != "hk-1" {
			t.Fatalf("序列化时三把凭据都应当打码：%s", out)
		}
		if _, err := r.Create(ctx, "hk-1", "websocket"); v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("名字重复应当是 name_taken：%v", err)
		}
		other, _ := r.CreateWith(ctx, CreateParams{
			Name:          "hk-2",
			Domain:        "hk2.example.com",
			DomainV6:      "v6.hk2.example.com",
			ListenPort:    24889,
			PullAddress:   "203.0.113.9",
			PullAddressV6: "2001:db8::9",
			PullPort:      24890,
		})
		if other.Status.Token.Reveal() == tok || other.Spec.ConnectionMode != ModeAuto ||
			other.Spec.Domain != "hk2.example.com" || other.Spec.ListenPort != 24889 || other.Spec.PullPort != 24890 {
			t.Fatalf("CreateWith 字段不对：%+v", other)
		}
	})
}

func TestLookupsAndList(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r, ctx := repo(bdb), context.Background()
		a, _ := r.Create(ctx, "a", "websocket")
		b, _ := r.Create(ctx, "b", "websocket")
		c, _ := r.Create(ctx, "c", "websocket")
		if got, err := r.ByName(ctx, "b"); err != nil || got.Metadata.ID != b.Metadata.ID {
			t.Fatalf("按名字取：%v", err)
		}
		if got, err := r.ByToken(ctx, c.Status.Token.Reveal()); err != nil || got.Metadata.ID != c.Metadata.ID {
			t.Fatalf("按 token 取：%v", err)
		}
		for _, bad := range []string{"", "nope"} {
			if _, err := r.ByToken(ctx, bad); !errors.Is(err, ErrNotFound) {
				t.Fatalf("token %q 应当找不到：%v", bad, err)
			}
		}
		if _, err := r.ByName(ctx, "zz"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("没有的名字应当是 ErrNotFound：%v", err)
		}
		if n, err := r.Count(ctx); err != nil || n != 3 {
			t.Fatalf("总数应当是 3：%d %v", n, err)
		}
		first, err := r.List(ctx, 0, 2)
		if err != nil || len(first) != 2 || first[0].Metadata.ID != a.Metadata.ID || first[1].Metadata.ID != b.Metadata.ID {
			t.Fatalf("第一页应当是 a、b：%v", err)
		}
		rest, err := r.List(ctx, first[1].Metadata.ID, 2)
		if err != nil || len(rest) != 1 || rest[0].Metadata.ID != c.Metadata.ID {
			t.Fatalf("第二页应当只有 c：%v", err)
		}
	})
}

// status 的写入不抬 resourceVersion；离线只动 connected 的；批量离线只动心跳早于阈值的。
func TestStatusWrites(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r, ctx := repo(bdb), context.Background()
		s, _ := r.Create(ctx, "hk-1", "websocket")
		id := s.Metadata.ID
		if ok, err := r.MarkOffline(ctx, id, ts(0)); err != nil || ok {
			t.Fatalf("pending 的服务器不应当被标离线：%v %v", ok, err)
		}
		if err := r.MarkOnline(ctx, id, "203.0.113.5", false, ts(1), ts(2)); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkOnline(ctx, id, "2001:db8::1", true, time.Time{}, ts(3)); err != nil {
			t.Fatal(err)
		}
		got, _ := r.ByName(ctx, "hk-1")
		if got.Status.Status != StatusConnected || !got.Status.LastHeartbeat.Equal(ts(3)) || got.Status.IPAddress == nil ||
			*got.Status.IPAddress != "203.0.113.5" || got.Status.IPAddressV6 != "2001:db8::1" || !got.Status.BootTime.Equal(ts(1)) ||
			got.Status.OfflineSince != nil || got.Metadata.ResourceVersion != 1 {
			t.Fatalf("上线之后的 status 不对：%+v rv=%d", got.Status, got.Metadata.ResourceVersion)
		}
		if ok, err := r.MarkOffline(ctx, id, ts(4)); err != nil || !ok {
			t.Fatalf("在线的服务器应当被标离线：%v %v", ok, err)
		}
		if ok, _ := r.MarkOffline(ctx, id, ts(5)); ok {
			t.Fatal("已经离线的不应当再标一次")
		}
		got, _ = r.ByName(ctx, "hk-1")
		if got.Status.Status != StatusOffline || got.Status.OfflineSince == nil || !got.Status.OfflineSince.Equal(ts(4)) || got.Metadata.ResourceVersion != 1 {
			t.Fatalf("离线之后的 status 不对：%+v", got.Status)
		}

		fresh, _ := r.Create(ctx, "fresh", "websocket")
		stale, _ := r.Create(ctx, "stale", "websocket")
		_ = r.MarkOnline(ctx, fresh.Metadata.ID, "", false, time.Time{}, ts(50))
		_ = r.MarkOnline(ctx, stale.Metadata.ID, "", false, time.Time{}, ts(10))
		n, err := r.SweepOffline(ctx, ts(30), ts(60))
		if err != nil || n != 1 {
			t.Fatalf("应当只标一台：%d %v", n, err)
		}
		if g, _ := r.ByName(ctx, "stale"); g.Status.Status != StatusOffline || !g.Status.OfflineSince.Equal(ts(60)) {
			t.Fatalf("心跳过期的应当离线：%+v", g.Status)
		}
		if g, _ := r.ByName(ctx, "fresh"); g.Status.Status != StatusConnected {
			t.Fatalf("心跳没过期的应当还在线：%+v", g.Status)
		}
	})
}

// 内核那几列：只写给定的列，core_boot_count 写的是绝对值（同一个值写两次不变），不碰心跳写的列，不抬 resourceVersion；ByID 取得到。
func TestCoreWrites(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r, ctx := repo(bdb), context.Background()
		s, _ := r.Create(ctx, "hk-1", "websocket")
		id := s.Metadata.ID
		if err := r.MarkOnline(ctx, id, "203.0.113.5", false, ts(1), ts(2)); err != nil {
			t.Fatal(err)
		}
		t1, t2 := ts(10), ts(20)
		if err := r.MarkCore(ctx, id, true, "1.14.4", &t1, 0); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkCore(ctx, id, true, "1.14.4", &t2, 1); err != nil {
			t.Fatal(err)
		}
		for range 2 { // 同一个绝对值写两次
			if err := r.MarkCore(ctx, id, false, "", nil, 3); err != nil {
				t.Fatal(err)
			}
		}
		got, err := r.ByID(ctx, id)
		if err != nil || got.Spec.Name != "hk-1" {
			t.Fatalf("ByID 应当取得到：%v %v", got, err)
		}
		st := got.Status
		if st.CoreRunning || st.CoreVersion != "" || st.CoreBootTime == nil || !st.CoreBootTime.Equal(t2) || st.CoreBootCount != 3 {
			t.Fatalf("内核那几列不对：running=%v version=%q boot=%v count=%d", st.CoreRunning, st.CoreVersion, st.CoreBootTime, st.CoreBootCount)
		}
		if st.Status != StatusConnected || !st.LastHeartbeat.Equal(ts(2)) || got.Metadata.ResourceVersion != 1 {
			t.Fatalf("不应当碰心跳写的列，也不抬 resourceVersion：%+v rv=%d", st, got.Metadata.ResourceVersion)
		}
	})
}

func TestRotationRevocationAndAutoFallback(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r, ctx := repo(bdb), context.Background()
		srv, err := r.CreateWith(ctx, CreateParams{Name: "hk-auto", Mode: ModeAuto, PullAddress: "198.51.100.1"})
		if err != nil {
			t.Fatal(err)
		}
		noAddr, err := r.CreateWith(ctx, CreateParams{Name: "hk-noaddr", Mode: ModeAuto})
		if err != nil {
			t.Fatal(err)
		}
		pullSrv, err := r.CreateWith(ctx, CreateParams{Name: "hk-pull", Mode: ModePull, PullAddress: "198.51.100.2"})
		if err != nil {
			t.Fatal(err)
		}

		// 凭据轮换 + 回滚 + 确认，全过程不抬 resourceVersion。
		newTok, newAgent := "newtok0123456789012345678901234567890123456", "newagt0123456789012345678901234567890123456"
		rot, err := r.BeginRotation(ctx, srv.Metadata.ID, TokensPatch{Token: &newTok, AgentToken: &newAgent}, ts(10))
		if err != nil {
			t.Fatal(err)
		}
		if rot.Metadata.ResourceVersion != 1 || !rot.Status.RotationPending || rot.Status.LastRotatedAt == nil ||
			rot.Status.Token.Reveal() != newTok || rot.Status.AgentToken.Reveal() != newAgent ||
			rot.Status.PullToken.Reveal() != srv.Status.PullToken.Reveal() {
			t.Fatalf("BeginRotation 状态不对：%+v", rot)
		}
		if err := r.RollbackRotation(ctx, srv); err != nil {
			t.Fatal(err)
		}
		restored, _ := r.ByID(ctx, srv.Metadata.ID)
		if restored.Status.RotationPending || restored.Status.Token.Reveal() != srv.Status.Token.Reveal() ||
			restored.Status.AgentToken.Reveal() != srv.Status.AgentToken.Reveal() {
			t.Fatalf("RollbackRotation 应当恢复原凭据与标志：%+v", restored)
		}
		if _, err := r.BeginRotation(ctx, srv.Metadata.ID, TokensPatch{Token: &newTok}, ts(12)); err != nil {
			t.Fatal(err)
		}
		if err := r.FinishRotation(ctx, srv.Metadata.ID); err != nil {
			t.Fatal(err)
		}
		if afterFinish, _ := r.ByID(ctx, srv.Metadata.ID); afterFinish.Status.RotationPending {
			t.Fatalf("FinishRotation 应当清除 rotation_pending")
		}

		// 吊销：立即覆盖且可选置 revoke_pending，ClearPendingFlags 清回 false。
		revTok := "revtok0123456789012345678901234567890123456"
		rev, err := r.RevokeTokens(ctx, srv.Metadata.ID, TokensPatch{Token: &revTok}, ts(15), true)
		if err != nil {
			t.Fatal(err)
		}
		if !rev.Status.RevokePending || rev.Status.RotationPending || rev.Status.Token.Reveal() != revTok || rev.Metadata.ResourceVersion != 1 {
			t.Fatalf("RevokeTokens 状态不对：%+v", rev)
		}
		if err := r.ClearPendingFlags(ctx, srv.Metadata.ID); err != nil {
			t.Fatal(err)
		}
		if cleared, _ := r.ByID(ctx, srv.Metadata.ID); cleared.Status.RevokePending || cleared.Status.RotationPending {
			t.Fatalf("ClearPendingFlags 应当清零待确认标志：%+v", cleared.Status)
		}

		// auto 回退扫盘：仅无活跃 WS 且有轮询地址的服务器累计；3 轮后转 fallback_to_pull=true；
		// 轮询采集成功不清除 fallback_to_pull，主动上线心跳后才清零。
		if targets, err := r.PollTargets(ctx); err != nil || len(targets) != 1 || targets[0].Metadata.ID != pullSrv.Metadata.ID {
			t.Fatalf("初始 PollTargets 应当只有 hk-pull：%+v err=%v", targets, err)
		}
		for round := 1; round <= 2; round++ {
			n, err := r.SweepAutoFallback(ctx, ts(30), ts(30+round), nil)
			if err != nil || n != 0 {
				t.Fatalf("第 %d 轮不应当转入轮询：n=%d err=%v", round, n, err)
			}
		}
		// 若已有活跃 WS 连接则跳过计数。
		if n, err := r.SweepAutoFallback(ctx, ts(30), ts(33), map[int64]bool{srv.Metadata.ID: true}); err != nil || n != 0 {
			t.Fatalf("有活跃 WS 不应计数：n=%d err=%v", n, err)
		}
		// 第 3 轮触发 fallback_to_pull。
		if n, err := r.SweepAutoFallback(ctx, ts(30), ts(34), nil); err != nil || n != 1 {
			t.Fatalf("第 3 轮应当触发 1 台转入轮询：n=%d err=%v", n, err)
		}
		if gNoAddr, _ := r.ByID(ctx, noAddr.Metadata.ID); gNoAddr.Status.PushFailCount != 0 || gNoAddr.Status.FallbackToPull {
			t.Fatalf("没有轮询地址的 auto 不应计数或回退：%+v", gNoAddr.Status)
		}
		if targets, err := r.PollTargets(ctx); err != nil || len(targets) != 2 {
			t.Fatalf("PollTargets 应当包含 hk-auto 与 hk-pull：%d err=%v", len(targets), err)
		}
		if err := r.MarkPullSuccess(ctx, srv.Metadata.ID, "198.51.100.1", false, ts(40)); err != nil {
			t.Fatal(err)
		}
		pulled, _ := r.ByID(ctx, srv.Metadata.ID)
		if pulled.Status.Status != StatusConnected || !pulled.Status.FallbackToPull || pulled.Status.LastPullAt == nil || !pulled.Status.LastPullAt.Equal(ts(40)) {
			t.Fatalf("MarkPullSuccess 状态不对：%+v", pulled.Status)
		}
		if err := r.MarkOnline(ctx, srv.Metadata.ID, "198.51.100.1", false, ts(1), ts(50)); err != nil {
			t.Fatal(err)
		}
		restoredAuto, _ := r.ByID(ctx, srv.Metadata.ID)
		if restoredAuto.Status.FallbackToPull || restoredAuto.Status.PushFailCount != 0 {
			t.Fatalf("MarkOnline 应当清零 fallback_to_pull 与 push_fail_count：%+v", restoredAuto.Status)
		}

		// SetSameHost 只写 status 档 same_host_as_master，不抬 resourceVersion；UpdatePorts 按乐观并发更新端口并抬版本。
		verBeforeSame := restoredAuto.Metadata.ResourceVersion
		if err := r.SetSameHost(ctx, srv.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}
		sameSrv, _ := r.ByID(ctx, srv.Metadata.ID)
		if !sameSrv.Status.SameHostAsMaster || sameSrv.Metadata.ResourceVersion != verBeforeSame {
			t.Fatalf("SetSameHost 应当只改 same_host_as_master 且不抬版本：%+v", sameSrv)
		}
		if _, err := r.UpdatePorts(ctx, srv.Metadata.ID, verBeforeSame+99, 24889, 24890); v1.AsError(err).Code != v1.CodeVersionConflict {
			t.Fatalf("过期 version 调用 UpdatePorts 应当返回 version_conflict：%v", err)
		}
		updatedPorts, err := r.UpdatePorts(ctx, srv.Metadata.ID, verBeforeSame, 24889, 24890)
		if err != nil {
			t.Fatal(err)
		}
		if updatedPorts.Spec.ListenPort != 24889 || updatedPorts.Spec.PullPort != 24890 || updatedPorts.Metadata.ResourceVersion != verBeforeSame+1 {
			t.Fatalf("UpdatePorts 应当更新双口并抬升 resourceVersion：%+v", updatedPorts)
		}
	})
}
