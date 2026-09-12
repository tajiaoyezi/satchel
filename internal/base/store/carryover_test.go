package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func mustCertificate(t *testing.T, s *store.Store, domain string, serverID *int64) *model.Certificate {
	t.Helper()
	c := &model.Certificate{Domain: domain, Email: "ops@example", ServerID: serverID}
	mustInsert(t, s, c)
	return c
}

// SystemSettings 单例：主键固定为 1，只有一个 resource_version，不能软删除。
func TestSystemSettingsSingleton(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		// 不填 id 也落到主键 1 那一行：M1 补单例行时不用记得手填。
		cfg := &model.SystemSettings{}
		mustInsert(t, s, cfg)
		if cfg.ID != 1 || cfg.ResourceVersion != 1 || cfg.HeartbeatInterval != 30 || cfg.SubInfoExpirePrefix != "📅过期时间" {
			t.Fatalf("单例应当落在主键 1 并带库默认值：%+v", cfg)
		}
		e := wantCode(t, s.Insert(ctx, &model.SystemSettings{ID: 1}), v1.CodeConflict)
		if !containsString(e.Reason, "id") {
			t.Fatalf("重复主键的 reason 应当点名 id，得到 %q", e.Reason)
		}
		e = wantCode(t, s.Insert(ctx, &model.SystemSettings{ID: 2}), v1.CodeBadRequest)
		if !containsString(e.Reason, "id") {
			t.Fatalf("主键 2 的 reason 应当点名 id，得到 %q", e.Reason)
		}
		cfg.HeartbeatInterval = 45
		if err := s.UpdateSpec(ctx, cfg, false, "heartbeat_interval"); err != nil {
			t.Fatal(err)
		}
		if cfg.ResourceVersion != 2 {
			t.Fatalf("改设置应当抬版本，得到 %d", cfg.ResourceVersion)
		}
		cfg.TelegramChatID = "-100"
		if err := s.UpdateMasterSelf(ctx, cfg, []string{"telegram_chat_id"}); err != nil {
			t.Fatal(err)
		}
		if cfg.ResourceVersion != 2 {
			t.Fatalf("主控自身类不抬版本，得到 %d", cfg.ResourceVersion)
		}
		stale := &model.SystemSettings{ID: 1, ResourceVersion: 1, HeartbeatInterval: 60}
		wantCode(t, s.UpdateSpec(ctx, stale, false, "heartbeat_interval"), v1.CodeVersionConflict)
		wantCode(t, s.UpdateSpec(ctx, cfg, false, "telegram_bot_token"), v1.CodeBadRequest)
		wantCode(t, s.UpdateSpec(ctx, cfg, false, "silent_mode"), v1.CodeBadRequest)
		cfg.SilentMode = true
		if err := s.UpdateHuman(ctx, cfg, []string{"silent_mode"}); err != nil {
			t.Fatal(err)
		}
		wantCode(t, s.SoftDelete(ctx, cfg, false), v1.CodeBadRequest)
		// key-value 底表：key 主键；Store 没有 upsert 原语，重复 key 报 conflict，M1 的事务自己写 ON CONFLICT。
		mustInsert(t, s, &model.SystemSettingEntry{Key: "brand_title", Value: "Satchel"})
		e = wantCode(t, s.Insert(ctx, &model.SystemSettingEntry{Key: "brand_title", Value: "x"}), v1.CodeConflict)
		if !containsString(e.Reason, "key") {
			t.Fatalf("重复 key 的 reason 应当点名 key，得到 %q", e.Reason)
		}
		var got model.SystemSettings
		if err := s.Get(ctx, &got, 1); err != nil {
			t.Fatal(err)
		}
		if got.HeartbeatInterval != 45 || got.TelegramChatID != "-100" || !got.SilentMode || got.ResourceVersion != 2 {
			t.Fatalf("读回的单例不对：%+v", got)
		}
	})
}

// 同一域名同一归属只能一张证书；server_id 为 NULL（主控本机）时也唯一。
func TestCertificateNaturalKeys(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		a := mustServer(t, s, "tokyo-1")
		b := mustServer(t, s, "osaka-1")
		mustCertificate(t, s, "x.example", &a.ID)
		e := wantCode(t, s.Insert(ctx, &model.Certificate{Domain: "x.example", Email: "e", ServerID: &a.ID}), v1.CodeNameTaken)
		if !containsString(e.Reason, "domain") {
			t.Fatalf("reason 应当点名 domain，得到 %q", e.Reason)
		}
		mustCertificate(t, s, "x.example", &b.ID)
		mustCertificate(t, s, "x.example", nil)
		wantCode(t, s.Insert(ctx, &model.Certificate{Domain: "x.example", Email: "e"}), v1.CodeNameTaken)
		if n := countRows(t, bdb, "certificates"); n != 3 {
			t.Fatalf("应当有三张证书，得到 %d", n)
		}
	})
}

// 复合自然键：custom_rules 的 (name, type)。
func TestCustomRuleCompositeNaturalKey(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		mustInsert(t, s, &model.CustomRule{Name: "ads", Type: "rules", Mode: "prepend", Content: "x"})
		e := wantCode(t, s.Insert(ctx, &model.CustomRule{Name: "ads", Type: "rules", Mode: "append", Content: "y"}), v1.CodeNameTaken)
		if !containsString(e.Reason, "name") || !containsString(e.Reason, "type") {
			t.Fatalf("reason 应当点名 name 与 type，得到 %q", e.Reason)
		}
		mustInsert(t, s, &model.CustomRule{Name: "ads", Type: "dns", Mode: "replace", Content: "z"})
	})
}

// 同一用户同时只能有一条待审的续费申请（部分唯一索引）；申请随用户物理删除一起删，同名新用户不会被旧申请挡住。
func TestRenewalRequestOnePendingPerUser(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		mustUser(t, s, "alice")
		first := &model.RenewalRequest{RequestToken: "t1", Username: "alice", PackageID: 1, RenewDays: 30, Passphrase: "p"}
		mustInsert(t, s, first)
		second := &model.RenewalRequest{RequestToken: "t2", Username: "alice", PackageID: 1, RenewDays: 30, Passphrase: "p"}
		e := wantCode(t, s.Insert(ctx, second), v1.CodeConflict)
		if !containsString(e.Reason, "username") {
			t.Fatalf("reason 应当点名 username，得到 %q", e.Reason)
		}
		if _, err := bdb.NewRaw("UPDATE renewal_requests SET status = 'approved' WHERE id = ?", first.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		mustInsert(t, s, second)
		if _, err := bdb.NewRaw("DELETE FROM users WHERE username = 'alice'").Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, bdb, "renewal_requests"); n != 0 {
			t.Fatalf("删用户后申请应当一起删掉，还有 %d 行", n)
		}
		mustUser(t, s, "alice")
		mustInsert(t, s, &model.RenewalRequest{RequestToken: "t3", Username: "alice", PackageID: 1, RenewDays: 30, Passphrase: "p"})
	})
}

// 删服务器：还有证书指着它就拒绝（证书材料不随服务器消失，也不悄悄变成主控本机）；
// 证书处理掉之后，站点、联邦记录、实时账与日账随服务器删除，主控本机的同名证书不受影响。
func TestDeletingServerWithCertificatesIsRefused(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		srv := mustServer(t, s, "tokyo-1")
		cert := mustCertificate(t, s, "x.example", &srv.ID)
		local := mustCertificate(t, s, "x.example", nil)
		mustInsert(t, s, &model.Website{ServerID: srv.ID, Domain: "x.example", Type: "static", CertificateID: &cert.ID})
		mustInsert(t, s, &model.FederatedServer{ServerID: srv.ID, OwnerURL: "https://owner", ShareToken: "st"})
		mustInsert(t, s, &model.SharedServer{ServerID: srv.ID, TokenHash: "h1"})
		mustInsert(t, s, &model.NodeTraffic{ServerID: srv.ID, Tag: "in", Type: "inbound"})
		mustInsert(t, s, &model.TrafficDailyUser{ServerID: srv.ID, Username: "alice", Date: "2026-09-12"})
		if _, err := bdb.NewRaw("DELETE FROM servers WHERE id = ?", srv.ID).Exec(ctx); err == nil {
			t.Fatal("还有证书指着服务器，物理删除应当被外键拦下")
		}
		if n := countRows(t, bdb, "servers"); n != 1 {
			t.Fatalf("被拦下的删除不该动到服务器，剩 %d 行", n)
		}
		if _, err := bdb.NewRaw("DELETE FROM certificates WHERE id = ?", cert.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := bdb.NewRaw("DELETE FROM servers WHERE id = ?", srv.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"websites", "federated_servers", "shared_servers", "node_traffic", "traffic_daily_users"} {
			if n := countRows(t, bdb, table); n != 0 {
				t.Errorf("删服务器后 %s 应当为空，还有 %d 行", table, n)
			}
		}
		var got model.Certificate
		if err := s.Get(ctx, &got, local.ID); err != nil {
			t.Fatal(err)
		}
		if got.ServerID != nil {
			t.Fatalf("主控本机的证书应当原样留下，得到 %+v", got)
		}
	})
}

// 删证书不删站点：websites.certificate_id 变 NULL。
func TestDeletingCertificateKeepsWebsite(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		srv := mustServer(t, s, "tokyo-1")
		cert := mustCertificate(t, s, "x.example", nil)
		site := &model.Website{ServerID: srv.ID, Domain: "x.example", Type: "static", CertificateID: &cert.ID}
		mustInsert(t, s, site)
		if _, err := bdb.NewRaw("DELETE FROM certificates WHERE id = ?", cert.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		var got model.Website
		if err := s.Get(ctx, &got, site.ID); err != nil {
			t.Fatal(err)
		}
		if got.CertificateID != nil {
			t.Fatalf("站点应当留下且 certificate_id 变 NULL，得到 %v", *got.CertificateID)
		}
	})
}

// 删外部订阅：代理集合配置与它的日账随之删除。
func TestDeletingExternalSubscriptionCascades(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		mustUser(t, s, "alice")
		ext := &model.ExternalSubscription{Username: "alice", Name: "up", URL: "https://up.example/sub?token=secret-abc"}
		mustInsert(t, s, ext)
		mustInsert(t, s, &model.ProxyProviderConfig{Username: "alice", ExternalSubscriptionID: ext.ID, Name: "pp"})
		mustInsert(t, s, &model.TrafficDailyExternalSubscription{ExternalSubscriptionID: ext.ID, Date: "2026-09-12"})
		if _, err := bdb.NewRaw("DELETE FROM external_subscriptions WHERE id = ?", ext.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"proxy_provider_configs", "traffic_daily_external_subscriptions"} {
			if n := countRows(t, bdb, table); n != 0 {
				t.Errorf("删外部订阅后 %s 应当为空，还有 %d 行", table, n)
			}
		}
	})
}

// 照抄簇里的记录表只追加。
func TestCarryoverAppendOnlyTables(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		ev := &model.SecurityEvent{IP: "1.2.3.4", Kind: "login_failed"}
		rv := &model.RuleVersion{Filename: "a.yaml", Version: 1, Content: "x", CreatedBy: "alice"}
		tg := &model.TgAudit{Action: "bind"}
		for _, m := range []any{ev, rv, tg} {
			mustInsert(t, s, m)
		}
		ev.Detail = "changed"
		wantCode(t, s.UpdateStatus(ctx, ev, []string{"detail"}), v1.CodeAppendOnly)
		rv.Content = "changed"
		wantCode(t, s.UpdateSpec(ctx, rv, false, "content"), v1.CodeAppendOnly)
		tg.Action = "unbind"
		wantCode(t, s.UpdateAction(ctx, tg, []string{"action"}), v1.CodeAppendOnly)
		wantCode(t, s.SoftDelete(ctx, ev, false), v1.CodeAppendOnly)
		// task_runs 不是 append-only：mmwx 先插 running，跑完原地改状态（附属表没有分档更新原语，M1 照 mmwx 用 SQL 收尾）。
		run := &model.TaskRun{TaskName: "renew", Status: "running", StartedAt: time.Now().UTC()}
		mustInsert(t, s, run)
		if _, err := bdb.NewRaw("UPDATE task_runs SET status = 'ok', duration_ms = 12 WHERE id = ?", run.ID).Exec(ctx); err != nil {
			t.Fatalf("任务运行记录应当能原地收尾：%v", err)
		}
	})
}

// 流量账本的复合主键与 JSON 列在两库都能写能读。
func TestTrafficLedgerAndPresetRoundTrip(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		srv := mustServer(t, s, "tokyo-1")
		row := &model.TrafficDailyUserEmail{ServerID: srv.ID, Email: "a@x", AttributedUsername: "alice", Date: "2026-09-12", Uplink: 1, WeightedUplink: 1.5}
		mustInsert(t, s, row)
		e := wantCode(t, s.Insert(ctx, &model.TrafficDailyUserEmail{ServerID: srv.ID, Email: "a@x", AttributedUsername: "alice", Date: "2026-09-12"}), v1.CodeConflict)
		if !containsString(e.Reason, "attributed_username") {
			t.Fatalf("reason 应当点名复合主键的列，得到 %q", e.Reason)
		}
		mustUser(t, s, "alice")
		preset := &model.RoutingRulePreset{Username: "alice", Name: "p", RuleJSON: json.RawMessage(`{"domain":["x"]}`)}
		mustInsert(t, s, preset)
		wantCode(t, s.Insert(ctx, &model.RoutingRulePreset{Username: "alice", Name: "q", RuleJSON: json.RawMessage(`{"domain":["x"]}`)}), v1.CodeConflict)
		var got model.RoutingRulePreset
		if err := s.Get(ctx, &got, preset.ID); err != nil {
			t.Fatal(err)
		}
		var rule map[string][]string
		if err := json.Unmarshal(got.RuleJSON, &rule); err != nil || rule["domain"][0] != "x" {
			t.Fatalf("规则 JSON 读回不对：%s %v", got.RuleJSON, err)
		}
	})
}
