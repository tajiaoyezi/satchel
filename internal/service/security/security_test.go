package security

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/security"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// clock 是测试用的可拨动时钟。
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)} }
func admin() context.Context             { return v1.WithIdentity(context.Background(), v1.LocalAdmin("root")) }
func user() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})
}
func inv(path string, args ...string) *command.Invocation {
	return &command.Invocation{Path: strings.Fields(path), Args: args, Flags: map[string]any{}}
}

func newService(t *testing.T, bdb *bun.DB) (*Service, *clock) {
	t.Helper()
	c := newClock()
	s := New(core.New(bdb), nil)
	s.SetNow(c.now)
	return s, c
}

func events(t *testing.T, bdb *bun.DB, f core.EventFilter) []core.Event {
	t.Helper()
	list, err := core.New(bdb).ListEvents(context.Background(), f, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func wantCode(t *testing.T, err error, code v1.Code) *v1.Error {
	t.Helper()
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("想要错误码 %s，得到 %v", code, err)
	}
	return e
}

// master-login-protection「令牌猜测的计数与自动封禁」：窗口内第五次失败自动封禁，写库、记事件、清计数。
func TestProbesBanAtLimit(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s, c := newService(t, bdb)
		const ip = "198.51.100.7"
		for i := 0; i < 4; i++ {
			s.RecordProbe(ctx, ip, "/api/v1/whoami")
			c.advance(time.Minute)
		}
		if _, banned := s.Banned(ip); banned {
			t.Fatal("四次还不该封")
		}
		s.RecordProbe(ctx, ip, "/api/v1/whoami")
		b, banned := s.Banned(ip)
		if !banned || b.Reason != core.ReasonBruteForce || b.FailCount != 5 || b.Permanent || b.ExpiresAt == nil || !b.ExpiresAt.Equal(c.now().Add(24*time.Hour)) {
			t.Fatalf("第五次应当封 24 小时：%+v %v", b, banned)
		}
		stored, err := core.New(bdb).GetActiveBan(ctx, ip, c.now())
		if err != nil || stored.Reason != core.ReasonBruteForce || stored.FailCount != 5 {
			t.Fatalf("封禁应当写进 ip_bans：%+v %v", stored, err)
		}
		list := events(t, bdb, core.EventFilter{IP: ip})
		if len(list) != 5 || list[0].Kind != core.KindBan || list[0].Detail != "fail=5" || list[1].Kind != core.KindProbe || list[1].Detail != "4/5" || list[4].Detail != "1/5" {
			t.Fatalf("事件应当是四条 probe 加一条 ban：%+v", list)
		}
		// 封禁期间再失败不计数、不记事件。
		s.RecordProbe(ctx, ip, "/api/v1/whoami")
		if n := len(events(t, bdb, core.EventFilter{IP: ip})); n != 5 {
			t.Fatalf("封禁期间不再记事件，得到 %d 条", n)
		}
		// 到期自动失效。
		c.advance(24*time.Hour + time.Second)
		if _, banned := s.Banned(ip); banned {
			t.Fatal("到期后不该还封着")
		}
	})
}

func TestProbeWindowAndSwitches(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s, c := newService(t, bdb)
		const ip = "203.0.113.9"
		// 过了窗口重数：3 次，隔 25 小时，再 4 次，仍没到 5。
		for i := 0; i < 3; i++ {
			s.RecordProbe(ctx, ip, "/mcp")
		}
		c.advance(25 * time.Hour)
		for i := 0; i < 4; i++ {
			s.RecordProbe(ctx, ip, "/mcp")
		}
		if _, banned := s.Banned(ip); banned {
			t.Fatal("窗口过了应当重数，不该封")
		}
		// 本地与内网地址在 skip_local_ip 下不计。
		for i := 0; i < 10; i++ {
			s.RecordProbe(ctx, "127.0.0.1", "/mcp")
			s.RecordProbe(ctx, "10.0.0.5", "/mcp")
		}
		if n := len(events(t, bdb, core.EventFilter{IP: "127.0.0.1"})) + len(events(t, bdb, core.EventFilter{IP: "10.0.0.5"})); n != 0 {
			t.Fatalf("本地与内网地址不计，得到 %d 条事件", n)
		}
		// 关掉开关：不计数、不自动封禁；已有的封禁照常生效。
		if _, err := s.ban(admin(), inv("security ban", "192.0.2.44")); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Enabled = false
		s.Configure(cfg)
		for i := 0; i < 10; i++ {
			s.RecordProbe(ctx, "198.51.100.200", "/mcp")
		}
		if _, banned := s.Banned("198.51.100.200"); banned {
			t.Fatal("开关关掉后不该自动封禁")
		}
		if n := len(events(t, bdb, core.EventFilter{IP: "198.51.100.200"})); n != 0 {
			t.Fatalf("开关关掉后不该记 probe，得到 %d 条", n)
		}
		if _, banned := s.Banned("192.0.2.44"); !banned {
			t.Fatal("开关关掉后已有的封禁照常生效")
		}
		// skip_local_ip 关掉后，本地地址才会被计、被封。
		cfg = DefaultConfig()
		cfg.SkipLocalIP = false
		cfg.MaxFailures = 2
		s.Configure(cfg)
		s.RecordProbe(ctx, "127.0.0.1", "/mcp")
		s.RecordProbe(ctx, "127.0.0.1", "/mcp")
		if _, banned := s.Banned("127.0.0.1"); !banned {
			t.Fatal("skip_local_ip 关掉后本地地址也该被封")
		}
		// 再打开 skip_local_ip：本地地址的封禁不生效。
		s.Configure(DefaultConfig())
		if _, banned := s.Banned("127.0.0.1"); banned {
			t.Fatal("skip_local_ip 开着时本地地址的封禁不生效")
		}
	})
}

// master-login-protection「封禁的效果与恢复」：重启后从 ip_bans 恢复，到期的不恢复。
func TestRestore(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		c := newClock()
		repo := core.New(bdb)
		later := c.now().Add(time.Hour)
		earlier := c.now().Add(-time.Hour)
		for _, b := range []core.Ban{
			{IP: "198.51.100.7", Reason: core.ReasonBruteForce, BannedAt: c.now().Add(-time.Hour), ExpiresAt: &later, FailCount: 5},
			{IP: "203.0.113.9", Reason: core.ReasonManual, BannedAt: c.now().Add(-time.Hour), Permanent: true, Actor: "admin"},
			{IP: "192.0.2.1", Reason: core.ReasonBruteForce, BannedAt: c.now().Add(-48 * time.Hour), ExpiresAt: &earlier, FailCount: 5},
		} {
			if err := repo.UpsertBan(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
		s := New(repo, nil)
		s.SetNow(c.now)
		if err := s.Restore(ctx); err != nil {
			t.Fatal(err)
		}
		for ip, want := range map[string]bool{"198.51.100.7": true, "203.0.113.9": true, "192.0.2.1": false} {
			if _, banned := s.Banned(ip); banned != want {
				t.Errorf("%s 恢复后是否封禁应当是 %v", ip, want)
			}
		}
		// 清理：到期的封禁与过了窗口的计数从内存里清掉。
		s.RecordProbe(ctx, "198.51.100.99", "/mcp")
		c.advance(2 * time.Hour)
		s.Sweep()
		s.mu.Lock()
		_, hasBan := s.bans["198.51.100.7"]
		_, hasPerm := s.bans["203.0.113.9"]
		s.mu.Unlock()
		if hasBan || !hasPerm {
			t.Fatalf("清理后到期的应当没了、永久的还在：到期 %v 永久 %v", hasBan, hasPerm)
		}
		c.advance(24 * time.Hour)
		s.Sweep()
		s.mu.Lock()
		_, hasProbe := s.probes["198.51.100.99"]
		s.mu.Unlock()
		if hasProbe {
			t.Fatal("过了窗口的计数应当被清掉")
		}
	})
}

// master-login-protection「手动封禁与解封」的服务层部分。
func TestBanAndUnban(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, c := newService(t, bdb)
		// 参数与可见范围。
		_, err := s.ban(admin(), inv("security ban", "not-an-ip"))
		wantCode(t, err, v1.CodeBadRequest)
		_, err = s.ban(admin(), inv("security ban", "10.0.0.5"))
		if e := wantCode(t, err, v1.CodeBadRequest); !strings.Contains(e.Reason, "skip_local_ip") {
			t.Fatalf("reason 应当说明 skip_local_ip：%s", e.Reason)
		}
		_, err = s.ban(user(), inv("security ban", "198.51.100.7"))
		wantCode(t, err, v1.CodeForbidden)
		_, err = s.unban(admin(), inv("security unban", "203.0.113.9"))
		wantCode(t, err, v1.CodeNotFound)
		_, err = s.bansList(user(), inv("security bans list"))
		wantCode(t, err, v1.CodeForbidden)
		_, err = s.eventsList(user(), inv("security events list"))
		wantCode(t, err, v1.CodeForbidden)

		// 封禁：到期是现在加 brute_force_block_minutes，IPv4 映射的 IPv6 按 IPv4 存。
		out, err := s.ban(admin(), inv("security ban", "::ffff:198.51.100.7"))
		if err != nil {
			t.Fatal(err)
		}
		b := out.(core.Ban)
		if b.IP != "198.51.100.7" || b.Reason != core.ReasonManual || b.Permanent || b.ExpiresAt == nil || !b.ExpiresAt.Equal(c.now().Add(24*time.Hour)) || b.Actor != "root" {
			t.Fatalf("手动封禁的输出不对：%+v", b)
		}
		if _, banned := s.Banned("198.51.100.7"); !banned {
			t.Fatal("封禁后应当生效")
		}
		// 永久：覆盖写。
		permanent := inv("security ban", "198.51.100.7")
		permanent.Flags["permanent"] = true
		out, err = s.ban(admin(), permanent)
		if err != nil || !out.(core.Ban).Permanent || out.(core.Ban).ExpiresAt != nil {
			t.Fatalf("永久封禁：%+v %v", out, err)
		}
		c.advance(1000 * time.Hour)
		if _, banned := s.Banned("198.51.100.7"); !banned {
			t.Fatal("永久封禁不会到期")
		}
		// 列表：分页。
		if _, err := s.ban(admin(), inv("security ban", "203.0.113.9")); err != nil {
			t.Fatal(err)
		}
		list := inv("security bans list")
		list.Page = &command.Page{Limit: 1}
		res, err := s.bansList(admin(), list)
		if err != nil {
			t.Fatal(err)
		}
		page := res.(*command.PageResult)
		if page.Total != 2 || len(page.Items) != 1 || page.Items[0].(core.Ban).IP != "203.0.113.9" || page.NextCursor == "" {
			t.Fatalf("第一页应当是最新封的 203.0.113.9：%+v", page)
		}
		list.Page = &command.Page{Limit: 1, Cursor: page.NextCursor}
		res, _ = s.bansList(admin(), list)
		if page := res.(*command.PageResult); len(page.Items) != 1 || page.Items[0].(core.Ban).IP != "198.51.100.7" || page.NextCursor != "" {
			t.Fatalf("第二页应当是 198.51.100.7 且是末页：%+v", page)
		}
		// 解封：库里留痕，内存里去掉，再解一次 not_found。
		out, err = s.unban(admin(), inv("security unban", "198.51.100.7"))
		if err != nil || out.(core.Ban).ReleasedAt == nil || out.(core.Ban).Actor != "root" {
			t.Fatalf("解封的输出不对：%+v %v", out, err)
		}
		if _, banned := s.Banned("198.51.100.7"); banned {
			t.Fatal("解封后不该生效")
		}
		_, err = s.unban(admin(), inv("security unban", "198.51.100.7"))
		wantCode(t, err, v1.CodeNotFound)
		// 事件：ban_manual 三条（第一次、改永久、另一个 IP），unban 一条；按种类过滤。
		ev := inv("security events list")
		ev.Flags["kind"] = core.KindBanManual
		res, err = s.eventsList(admin(), ev)
		if err != nil || res.(*command.PageResult).Total != 3 {
			t.Fatalf("ban_manual 应当 3 条：%+v %v", res, err)
		}
		ev.Flags = map[string]any{"ip": "198.51.100.7", "kind": core.KindUnban}
		res, _ = s.eventsList(admin(), ev)
		if p := res.(*command.PageResult); p.Total != 1 || p.Items[0].(core.Event).Actor != "root" {
			t.Fatalf("unban 应当 1 条、记操作者：%+v", p)
		}
	})
}

// master-login-protection「安全事件」：写不进事件只记 error 日志，不影响计数与封禁。
func TestEventWriteFailureOnlyLogs(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		var logs bytes.Buffer
		s := New(core.New(bdb), slog.New(slog.NewTextHandler(&logs, nil)))
		if _, err := bdb.ExecContext(ctx, "ALTER TABLE security_events RENAME TO security_events_broken"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = bdb.ExecContext(context.Background(), "ALTER TABLE security_events_broken RENAME TO security_events")
		})
		for i := 0; i < 5; i++ {
			s.RecordProbe(ctx, "198.51.100.7", "/api/v1/whoami")
		}
		if _, banned := s.Banned("198.51.100.7"); !banned {
			t.Fatal("写不进事件也该照常封禁")
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "写安全事件失败") {
			t.Fatalf("应当有 error 级别的日志：%s", logs.String())
		}
	})
}

// master-scheduler「本站的内置任务」的 security_event_cleanup：90 天以前的删掉，以内的留着。
func TestPruneEvents(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s, c := newService(t, bdb)
		repo := core.New(bdb)
		for _, at := range []time.Time{c.now().Add(-91 * 24 * time.Hour), c.now().Add(-91 * 24 * time.Hour), c.now().Add(-89 * 24 * time.Hour)} {
			if err := repo.InsertEvent(ctx, core.Event{At: at, IP: "198.51.100.7", Kind: core.KindProbe}); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := s.PruneEvents(ctx); err != nil || n != 2 {
			t.Fatalf("应当删掉 2 条，得到 %d %v", n, err)
		}
		if left, _ := repo.CountEvents(ctx, core.EventFilter{}); left != 1 {
			t.Fatalf("应当剩 1 条，得到 %d", left)
		}
	})
}
