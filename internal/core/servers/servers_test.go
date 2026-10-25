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
		if raw, err := base64.RawURLEncoding.DecodeString(tok); len(tok) != 43 || err != nil || len(raw) != 32 {
			t.Fatalf("token 应当是 32 字节的 base64url（43 个字符）：%q %v", tok, err)
		}
		if s.Spec.Name != "hk-1" || s.Spec.ConnectionMode != "websocket" || s.Status.Status != StatusPending ||
			s.Metadata.ResourceVersion != 1 || !s.Spec.Ipv6Enabled || !s.Spec.IncludeInTrafficStats || s.Metadata.ID == 0 {
			t.Fatalf("新服务器的字段不对：%+v", s)
		}
		out, _ := json.Marshal(s)
		if strings.Contains(string(out), tok) || !strings.Contains(string(out), `"token":"***"`) || s.Metadata.Name != "hk-1" {
			t.Fatalf("序列化时 token 应当打码：%s", out)
		}
		if _, err := r.Create(ctx, "hk-1", "websocket"); v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("名字重复应当是 name_taken：%v", err)
		}
		other, _ := r.Create(ctx, "hk-2", "websocket")
		if other.Status.Token.Reveal() == tok {
			t.Fatal("每台服务器的 token 应当不同")
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
