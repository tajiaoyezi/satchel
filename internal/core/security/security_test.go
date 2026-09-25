package security

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
)

// master-login-protection「安全事件」的仓储部分：追加、按种类与 IP 过滤、按 id 倒序翻页不重不漏。
func TestEvents(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := New(bdb)
		base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
		for i := 0; i < 7; i++ {
			kind, ip := "probe", "198.51.100.7"
			if i%2 == 1 {
				kind, ip = "login_fail", "203.0.113.9"
			}
			if err := r.InsertEvent(ctx, Event{At: base.Add(time.Duration(i) * time.Minute), IP: ip, Kind: kind, Path: "/api/v1/whoami", Detail: fmt.Sprintf("%d/5", i+1)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.InsertEvent(ctx, Event{IP: "198.51.100.7", Kind: "unban", Actor: "admin"}); err != nil {
			t.Fatal(err)
		}
		all, err := r.ListEvents(ctx, EventFilter{}, 100, 0)
		if err != nil || len(all) != 8 {
			t.Fatalf("应当 8 条：%d %v", len(all), err)
		}
		if all[0].Kind != "unban" || all[0].Actor != "admin" || all[0].At.IsZero() {
			t.Fatalf("按 id 倒序，第一条是最后写的 unban，没给时间时用当前时间：%+v", all[0])
		}
		if !all[1].At.Equal(base.Add(6*time.Minute)) || all[1].Detail != "7/5" {
			t.Fatalf("字段应当原样读回：%+v", all[1])
		}
		probes, _ := r.ListEvents(ctx, EventFilter{Kind: "probe"}, 100, 0)
		logins, _ := r.ListEvents(ctx, EventFilter{Kind: "login_fail", IP: "203.0.113.9"}, 100, 0)
		byIP, _ := r.ListEvents(ctx, EventFilter{IP: "198.51.100.7"}, 100, 0)
		if len(probes) != 4 || len(logins) != 3 || len(byIP) != 5 {
			t.Fatalf("过滤：probe %d、login_fail %d、按 IP %d", len(probes), len(logins), len(byIP))
		}
		if n, err := r.CountEvents(ctx, EventFilter{Kind: "probe"}); err != nil || n != 4 {
			t.Fatalf("计数应当 4：%d %v", n, err)
		}
		// keyset 翻页：每页 3 条，翻完恰好 8 条、不重复。
		seen := map[int64]bool{}
		var before int64
		for {
			page, err := r.ListEvents(ctx, EventFilter{}, 3, before)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, e := range page {
				if seen[e.ID] {
					t.Fatalf("id %d 重复", e.ID)
				}
				seen[e.ID] = true
			}
			before = page[len(page)-1].ID
		}
		if len(seen) != 8 {
			t.Fatalf("翻完应当 8 条，得到 %d", len(seen))
		}
	})
}

// master-login-protection「封禁的效果与恢复」「手动封禁与解封」的仓储部分。
func TestBans(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := New(bdb)
		now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
		in := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
		mustUpsert := func(b Ban) {
			t.Helper()
			if err := r.UpsertBan(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
		mustUpsert(Ban{IP: "198.51.100.7", Reason: "brute_force", BannedAt: now.Add(-time.Hour), ExpiresAt: in(23 * time.Hour), FailCount: 5})
		mustUpsert(Ban{IP: "203.0.113.9", Reason: "manual", BannedAt: now.Add(-2 * time.Hour), Permanent: true, Actor: "admin"})
		mustUpsert(Ban{IP: "192.0.2.1", Reason: "brute_force", BannedAt: now.Add(-48 * time.Hour), ExpiresAt: in(-24 * time.Hour), FailCount: 5})

		// 生效中的：到期的那条不算；永久的不看到期时间。
		b, err := r.GetActiveBan(ctx, "198.51.100.7", now)
		if err != nil || b.Reason != "brute_force" || b.FailCount != 5 || b.ExpiresAt == nil || !b.ExpiresAt.Equal(*in(23 * time.Hour)) || !b.Active(now) {
			t.Fatalf("自动封禁应当生效：%+v %v", b, err)
		}
		if b, err := r.GetActiveBan(ctx, "203.0.113.9", now); err != nil || !b.Permanent || b.ExpiresAt != nil || b.Actor != "admin" {
			t.Fatalf("永久封禁应当生效：%+v %v", b, err)
		}
		if _, err := r.GetActiveBan(ctx, "192.0.2.1", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("到期的封禁不该生效：%v", err)
		}
		if _, err := r.GetActiveBan(ctx, "10.9.9.9", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("没封过的应当 ErrNotFound：%v", err)
		}

		// 列表：按封禁时间倒序，只有生效中的；偏移量翻页。
		if n, err := r.CountActiveBans(ctx, now); err != nil || n != 2 {
			t.Fatalf("生效中的应当 2 条：%d %v", n, err)
		}
		list, err := r.ListActiveBans(ctx, now, 10, 0)
		if err != nil || len(list) != 2 || list[0].IP != "198.51.100.7" || list[1].IP != "203.0.113.9" {
			t.Fatalf("列表应当按封禁时间倒序：%+v %v", list, err)
		}
		if page, _ := r.ListActiveBans(ctx, now, 1, 1); len(page) != 1 || page[0].IP != "203.0.113.9" {
			t.Fatalf("第二页应当是 203.0.113.9：%+v", page)
		}
		if all, err := r.ActiveBans(ctx, now); err != nil || len(all) != 2 {
			t.Fatalf("恢复用的全量应当 2 条：%+v %v", all, err)
		}

		// 覆盖写：同一 IP 改成永久，解封时间清空。
		released, err := r.ReleaseBan(ctx, "198.51.100.7", "admin", now)
		if err != nil || released.ReleasedAt == nil || !released.ReleasedAt.Equal(now) || released.Actor != "admin" {
			t.Fatalf("解封应当写解封时间与操作者：%+v %v", released, err)
		}
		if _, err := r.GetActiveBan(ctx, "198.51.100.7", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("解封后不该生效：%v", err)
		}
		if _, err := r.ReleaseBan(ctx, "198.51.100.7", "admin", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("再解封一次应当 ErrNotFound：%v", err)
		}
		if _, err := r.ReleaseBan(ctx, "192.0.2.1", "admin", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("已到期的解封应当 ErrNotFound：%v", err)
		}
		mustUpsert(Ban{IP: "198.51.100.7", Reason: "manual", BannedAt: now, Permanent: true, Actor: "root"})
		if b, err := r.GetActiveBan(ctx, "198.51.100.7", now.Add(1000*time.Hour)); err != nil || !b.Permanent || b.ReleasedAt != nil || b.Reason != "manual" || b.FailCount != 0 {
			t.Fatalf("覆盖写之后应当是新的永久封禁：%+v %v", b, err)
		}
	})
}
