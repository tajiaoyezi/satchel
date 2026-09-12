package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func mustServer(t *testing.T, s *store.Store, name string) *model.Server {
	t.Helper()
	srv := &model.Server{Name: name, Token: "tok-" + name, CoreDNS: json.RawMessage(`{}`)}
	mustInsert(t, s, srv)
	return srv
}

func countRows(t *testing.T, bdb *bun.DB, table string) int {
	t.Helper()
	var n int
	if err := bdb.NewRaw("SELECT count(*) FROM "+table).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 物理删除一台服务器：它名下的五个 kind 与批量追踪表级联删除，证据包留下、server_id 变 NULL。
func TestDeletingServerCascades(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		srv := mustServer(t, s, "tokyo-1")
		mustInsert(t, s, &model.Inbound{ServerID: srv.ID, Tag: "in", Protocol: "vless", Port: 443})
		mustInsert(t, s, &model.Outbound{ServerID: srv.ID, Tag: "out", Protocol: "direct"})
		mustInsert(t, s, &model.RoutingRule{ServerID: srv.ID, OutboundTag: "out"})
		mustInsert(t, s, &model.Website{ServerID: srv.ID, Domain: "a.example", Type: "static"})
		mustInsert(t, s, &model.ReturnRoute{ServerID: srv.ID, Carrier: "CT"})
		mustInsert(t, s, &model.BatchInbound{BatchID: "b1", Tag: "in", ServerID: srv.ID, Protocol: "vless", Port: 443})
		mustInsert(t, s, &model.BatchOutbound{BatchID: "b1", Tag: "out", ServerID: srv.ID, Protocol: "direct"})
		ev := &model.EvidencePackage{Category: "server_offline", ServerID: &srv.ID, Items: json.RawMessage(`{}`)}
		mustInsert(t, s, ev)
		if _, err := bdb.NewRaw("DELETE FROM servers WHERE id = ?", srv.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"inbounds", "outbounds", "routing_rules", "websites", "return_routes", "batch_inbounds", "batch_outbounds"} {
			if n := countRows(t, bdb, table); n != 0 {
				t.Errorf("删服务器后 %s 应当为空，还有 %d 行", table, n)
			}
		}
		var got model.EvidencePackage
		if err := s.Get(ctx, &got, ev.ID); err != nil {
			t.Fatal(err)
		}
		if got.ServerID != nil {
			t.Fatalf("证据包应当留下且 server_id 变 NULL，得到 %v", *got.ServerID)
		}
	})
}

func TestForeignKeyToUsersByUsername(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		pkg := &model.Package{Name: "basic", Nodes: json.RawMessage(`[]`)}
		mustInsert(t, s, pkg)
		dangling := &model.PackageAssignment{Username: "nobody", PackageID: pkg.ID, ShortCode: "sc1"}
		e := wantCode(t, s.Insert(ctx, dangling), v1.CodeBadRequest)
		if !containsString(e.Reason, "引用的对象不存在") {
			t.Fatalf("外键违反的 reason 不对：%q", e.Reason)
		}
		mustUser(t, s, "alice")
		ok := &model.PackageAssignment{Username: "alice", PackageID: pkg.ID, ShortCode: "sc2"}
		mustInsert(t, s, ok)
		if ok.Status != "active" || ok.ResourceVersion != 1 {
			t.Fatalf("分配应当带库默认值：%+v", ok)
		}
		if _, err := bdb.NewRaw("DELETE FROM users WHERE username = ?", "alice").Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, bdb, "package_assignments"); n != 0 {
			t.Fatalf("删用户后分配应当级联删除，还有 %d 行", n)
		}
	})
}

// Server 的令牌只由动作写：UpdateAction 成功且版本不变，UpdateSpec 被拒。
func TestServerTokensAreActionOnly(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		srv := mustServer(t, s, "osaka-1")
		srv.Token = "rotated"
		srv.AgentToken = "agent-rotated"
		if err := s.UpdateAction(ctx, srv, []string{"token", "agent_token"}); err != nil {
			t.Fatal(err)
		}
		var got model.Server
		if err := s.Get(ctx, &got, srv.ID); err != nil {
			t.Fatal(err)
		}
		if got.Token != "rotated" || got.ResourceVersion != 1 {
			t.Fatalf("令牌应当落库且版本不变：token=%q version=%d", got.Token, got.ResourceVersion)
		}
		wantCode(t, s.UpdateSpec(ctx, srv, true, "token"), v1.CodeBadRequest)
		wantCode(t, s.UpdateStatus(ctx, srv, []string{"pull_token"}), v1.CodeBadRequest)
		srv.Domain = "osaka.example"
		if err := s.UpdateSpec(ctx, srv, false, "domain"); err != nil {
			t.Fatalf("spec 列照常能写：%v", err)
		}
		if srv.ResourceVersion != 2 {
			t.Fatalf("spec 写应当抬版本到 2，得到 %d", srv.ResourceVersion)
		}
	})
}

// Node 的摘挂标记只由动作写，与用户自己的 enabled 分开。
func TestNodeDetachIsActionOnly(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		mustUser(t, s, "bob")
		node := &model.Node{Username: "bob", RawURL: "vless://x", NodeName: "n", Protocol: "vless",
			ParsedConfig: json.RawMessage(`{}`), ClashConfig: "", Enabled: true}
		mustInsert(t, s, node)
		node.Detached = true
		reason := "node_unreachable"
		node.DetachReason = &reason
		if err := s.UpdateAction(ctx, node, []string{"detached", "detach_reason"}); err != nil {
			t.Fatal(err)
		}
		var got model.Node
		if err := s.Get(ctx, &got, node.ID); err != nil {
			t.Fatal(err)
		}
		if !got.Detached || got.DetachReason == nil || !got.Enabled || got.ResourceVersion != 1 {
			t.Fatalf("摘挂应当落库、不动 enabled、不抬版本：%+v", got)
		}
		wantCode(t, s.UpdateSpec(ctx, node, true, "detached"), v1.CodeBadRequest)
	})
}

// username 是自然主键，创建后不能改；短码与同用户同入站的凭据都唯一。
func TestUsernameImmutableAndSideTableUniques(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		u := mustUser(t, s, "carol")
		u.Username = "carol2"
		e := wantCode(t, s.UpdateSpec(ctx, u, false, "username"), v1.CodeBadRequest)
		if !containsString(e.Reason, "username") || !containsString(e.Reason, "不能改") {
			t.Fatalf("改 username 应当被拒并点名：%q", e.Reason)
		}
		u.Username = "carol"
		u.Nickname = strPtr("Carol")
		if err := s.UpdateSpec(ctx, u, false, "nickname"); err != nil {
			t.Fatalf("其它 spec 列照常能改：%v", err)
		}
		mustUser(t, s, "dave")
		mustInsert(t, s, &model.UserToken{Username: "carol", Token: "t1", UserShortCode: "abc"})
		e = wantCode(t, s.Insert(ctx, &model.UserToken{Username: "dave", Token: "t2", UserShortCode: "abc"}), v1.CodeConflict)
		if !containsString(e.Reason, "user_short_code") {
			t.Fatalf("短码撞上应当点名 user_short_code：%q", e.Reason)
		}
		if err := s.Insert(ctx, &model.UserToken{Username: "dave", Token: "t2", UserShortCode: ""}); err != nil {
			t.Fatalf("空短码不受唯一约束：%v", err)
		}
		srv := mustServer(t, s, "s1")
		first := &model.UserInboundConfig{Username: "carol", ServerID: srv.ID, InboundTag: "in", Protocol: "vless", CredentialJSON: json.RawMessage(`{}`)}
		mustInsert(t, s, first)
		dup := &model.UserInboundConfig{Username: "carol", ServerID: srv.ID, InboundTag: "in", Protocol: "vless", CredentialJSON: json.RawMessage(`{}`)}
		wantCode(t, s.Insert(ctx, dup), v1.CodeConflict)
	})
}

func strPtr(s string) *string { return &s }
