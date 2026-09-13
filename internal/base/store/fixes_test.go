package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// m0-06：用户改名连带外键、Inbound 家族的复合自然键、去重键不算已软删除的行。

// storage-schema「Cross-cluster foreign keys」：username 外键 ON UPDATE CASCADE。
// 改名本身是 M1 / M3 的专门操作，这里用 raw SQL 代替它，只验证数据库层的连带改动。
func TestRenamingUserCascadesToReferences(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		u := mustUser(t, s, "alice")
		pkg := &model.Package{Name: "basic", Nodes: json.RawMessage(`[]`)}
		mustInsert(t, s, pkg)
		mustInsert(t, s, &model.PackageAssignment{Username: "alice", PackageID: pkg.ID, ShortCode: "sc1"})
		mustInsert(t, s, &model.UserToken{Username: "alice", Token: "t1"})
		mustInsert(t, s, &model.Node{Username: "alice", RawURL: "vless://x", NodeName: "n", Protocol: "vless", ParsedConfig: json.RawMessage(`{}`)})
		mustToken(t, s, "alice", "cli")
		mustInsert(t, s, &model.RenewalRequest{RequestToken: "r1", Username: "alice", PackageID: pkg.ID, RenewDays: 30, Passphrase: "p"})
		mustInsert(t, s, &model.ExternalSubscription{Username: "alice", Name: "ext", URL: "https://example.com/sub"})
		mustInsert(t, s, &model.Session{TokenHash: "h1", Username: "alice", ExpiresAt: time.Now().Add(time.Hour).UTC()})

		// UpdateSpec 仍然拒绝改 username：改名不是 spec 写。
		u.Username = "alice2"
		wantCode(t, s.UpdateSpec(ctx, u, false, "username"), v1.CodeBadRequest)

		if _, err := bdb.NewRaw("UPDATE users SET username = 'alice2' WHERE username = 'alice'").Exec(ctx); err != nil {
			t.Fatalf("改名应当被外键级联接住：%v", err)
		}
		for table, column := range map[string]string{
			"package_assignments": "username", "user_tokens": "username", "nodes": "username", "api_tokens": "owner",
			"renewal_requests": "username", "external_subscriptions": "username", "sessions": "username",
		} {
			var stale, fresh int
			if err := bdb.NewRaw("SELECT count(*) FROM "+table+" WHERE "+column+" = 'alice'").Scan(ctx, &stale); err != nil {
				t.Fatal(err)
			}
			if err := bdb.NewRaw("SELECT count(*) FROM "+table+" WHERE "+column+" = 'alice2'").Scan(ctx, &fresh); err != nil {
				t.Fatal(err)
			}
			if stale != 0 || fresh != 1 {
				t.Errorf("%s.%s 改名后应当全是新名：旧名 %d 行、新名 %d 行", table, column, stale, fresh)
			}
		}
	})
}

// storage-schema「Core kind tables」：同一服务器下的 tag / domain / carrier 是自然键，跨服务器同名合法。
func TestServerScopedNaturalKeys(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		a := mustServer(t, s, "a")
		b := mustServer(t, s, "b")
		cases := []struct {
			name    string
			first   func(serverID int64) any
			columns []string
		}{
			{"inbounds", func(id int64) any { return &model.Inbound{ServerID: id, Tag: "in", Protocol: "vless", Port: 443} }, []string{"server_id", "tag"}},
			{"outbounds", func(id int64) any { return &model.Outbound{ServerID: id, Tag: "out", Protocol: "direct"} }, []string{"server_id", "tag"}},
			{"websites", func(id int64) any {
				return &model.Website{ServerID: id, Domain: "www.example.com", Type: "static", Target: "/srv"}
			}, []string{"server_id", "domain"}},
			{"return_routes", func(id int64) any { return &model.ReturnRoute{ServerID: id, Carrier: "CT"} }, []string{"server_id", "carrier"}},
		}
		for _, tc := range cases {
			mustInsert(t, s, tc.first(a.ID))
			e := wantCode(t, s.Insert(ctx, tc.first(a.ID)), v1.CodeNameTaken)
			for _, col := range tc.columns {
				if !containsString(e.Reason, col) {
					t.Errorf("%s 撞自然键的 reason 应当点名 %s，得到 %q", tc.name, col, e.Reason)
				}
			}
			if err := s.Insert(ctx, tc.first(b.ID)); err != nil {
				t.Errorf("%s 跨服务器同名应当合法：%v", tc.name, err)
			}
		}
	})
}

// storage-schema「Task and alert dedup at the database level」：软删除的行不占去重键。
func TestDedupKeysIgnoreSoftDeletedRows(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		tk := task("k")
		mustInsert(t, s, tk)
		al := alert("k")
		mustInsert(t, s, al)
		wantCode(t, s.Insert(ctx, task("k")), v1.CodeConflict)
		wantCode(t, s.Insert(ctx, alert("k")), v1.CodeConflict)
		if err := s.SoftDelete(ctx, tk, false); err != nil {
			t.Fatal(err)
		}
		if err := s.SoftDelete(ctx, al, false); err != nil {
			t.Fatal(err)
		}
		if err := s.Insert(ctx, task("k")); err != nil {
			t.Fatalf("软删后同键待办应当能再插：%v", err)
		}
		if err := s.Insert(ctx, alert("k")); err != nil {
			t.Fatalf("软删后同键告警应当能再插：%v", err)
		}
		// 新行仍是 open：再插一条同键要被挡，说明去重索引还在起作用。
		wantCode(t, s.Insert(ctx, task("k")), v1.CodeConflict)
	})
}
