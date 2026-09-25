package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coresecurity "github.com/satchel/satchel/internal/core/security"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// limited 建一个用默认限流参数、接上安全事件、时钟可拨的服务。
func limited(t *testing.T, bdb *bun.DB) (*Service, *time.Time) {
	t.Helper()
	s := New(users.New(bdb, store.New(bdb, schema.Default())), sessions.New(bdb))
	s.SetEvents(coresecurity.New(bdb), nil)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s.SetNow(func() time.Time { return now })
	return s, &now
}

func from(ip string) context.Context {
	return v1.WithRemote(context.Background(), v1.Remote{IP: ip})
}

func overSocket() context.Context {
	return v1.WithRemote(context.Background(), v1.Remote{Socket: true, Local: true})
}

func wantRateLimited(t *testing.T, err error, now time.Time, lock time.Duration) {
	t.Helper()
	e := v1.AsError(err)
	if err == nil || e.Code != v1.CodeRateLimited {
		t.Fatalf("应当 rate_limited，得到 %v", err)
	}
	if until, _ := e.State["until"].(string); until != now.Add(lock).UTC().Format(time.RFC3339) {
		t.Fatalf("state.until 应当是锁定期满的时间，得到 %v", e.State)
	}
}

func securityEvents(t *testing.T, bdb *bun.DB, f coresecurity.EventFilter) []coresecurity.Event {
	t.Helper()
	list, err := coresecurity.New(bdb).ListEvents(context.Background(), f, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// master-login-protection「登录限流」：连错五次后锁定，锁定期内正确的密码也不比对；失败留下事件。
func TestLoginLockAfterFiveFailures(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		ctx := from("127.0.0.1") // 回环：skip_local_ip 下只计账号维度
		for i := 0; i < 5; i++ {
			if _, err := s.Login(ctx, "admin", "wrong", false, ""); v1.AsError(err).Code != v1.CodeUnauthenticated {
				t.Fatalf("第 %d 次错密码应当 unauthenticated：%v", i+1, err)
			}
		}
		res, err := s.Login(ctx, "admin", password, false, "")
		wantRateLimited(t, err, *now, time.Hour)
		if res != nil {
			t.Fatal("锁定时不该发会话")
		}
		list := securityEvents(t, bdb, coresecurity.EventFilter{})
		if len(list) != 5 || list[0].Kind != coresecurity.KindLoginLocked || list[0].Detail != "5/5" || list[1].Kind != coresecurity.KindLoginFail || list[4].Detail != "1/5" {
			t.Fatalf("应当四条 login_fail 加一条 login_locked：%+v", list)
		}
		if list[0].Username != "admin" || list[0].Path != loginPath || list[0].IP != "127.0.0.1" {
			t.Fatalf("事件字段不对：%+v", list[0])
		}
		// 锁定期满后可以再登录。
		*now = now.Add(time.Hour + time.Second)
		if _, err := s.Login(ctx, "admin", password, false, ""); err != nil {
			t.Fatalf("锁定期满应当能登录：%v", err)
		}
	})
}

func TestLoginLimitDimensions(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		seedUser(t, bdb, "carol", "user", true)
		// 账号维度跨 IP：每次换一个公网地址，admin 仍被锁。
		for i, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4", "198.51.100.5"} {
			if _, err := s.Login(from(ip), "admin", "wrong", false, ""); v1.AsError(err).Code != v1.CodeUnauthenticated {
				t.Fatalf("第 %d 次：%v", i+1, err)
			}
		}
		_, err := s.Login(from("198.51.100.6"), "admin", password, false, "")
		wantRateLimited(t, err, *now, time.Hour)

		// IP 维度跨账号：同一个公网地址对五个不同的用户名各错一次，这个地址被锁，换地址登录 carol 照常。
		for _, name := range []string{"u1", "u2", "u3", "u4", "u5"} {
			_, _ = s.Login(from("203.0.113.9"), name, "wrong", false, "")
		}
		_, err = s.Login(from("203.0.113.9"), "carol", password, false, "")
		wantRateLimited(t, err, *now, time.Hour)
		if _, err := s.Login(from("203.0.113.10"), "carol", password, false, ""); err != nil {
			t.Fatalf("换地址登录 carol 应当成功：%v", err)
		}

		// 本地地址不计 IP 维度：回环对五个用户名各错一次，carol 仍能登录。
		for _, name := range []string{"v1", "v2", "v3", "v4", "v5"} {
			_, _ = s.Login(from("127.0.0.1"), name, "wrong", false, "")
		}
		if _, err := s.Login(from("127.0.0.1"), "carol", password, false, ""); err != nil {
			t.Fatalf("skip_local_ip 下回环不该被 IP 维度锁：%v", err)
		}
		// skip_local_ip 关掉后，内网地址也计 IP 维度。
		s.SetLoginLimits(LoginLimits{MaxAttempts: 2, Window: time.Hour, Lock: time.Minute, SkipLocalIP: false})
		for _, name := range []string{"w1", "w2"} {
			_, _ = s.Login(from("10.0.0.5"), name, "wrong", false, "")
		}
		_, err = s.Login(from("10.0.0.5"), "carol", password, false, "")
		wantRateLimited(t, err, *now, time.Minute)

		// unix socket 不受限：admin 的账号维度仍在锁定期内，经 socket 照样登录，也不留事件。
		before := len(securityEvents(t, bdb, coresecurity.EventFilter{}))
		if _, err := s.Login(overSocket(), "admin", password, false, ""); err != nil {
			t.Fatalf("经 unix socket 应当不受限：%v", err)
		}
		_, _ = s.Login(overSocket(), "admin", "wrong", false, "")
		if after := len(securityEvents(t, bdb, coresecurity.EventFilter{})); after != before {
			t.Fatalf("经 unix socket 不记事件：%d → %d", before, after)
		}
	})
}

func TestLoginWindowAndReset(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		ctx := from("198.51.100.7")
		// 过了窗口重数：错四次，隔 61 分钟，再错四次，仍能登录。
		for i := 0; i < 4; i++ {
			_, _ = s.Login(ctx, "admin", "wrong", false, "")
		}
		*now = now.Add(61 * time.Minute)
		for i := 0; i < 4; i++ {
			_, _ = s.Login(ctx, "admin", "wrong", false, "")
		}
		if _, err := s.Login(ctx, "admin", password, false, ""); err != nil {
			t.Fatalf("窗口过了应当重数：%v", err)
		}
		// 成功清零：错四次后成功，再错四次，仍能登录。
		for i := 0; i < 4; i++ {
			_, _ = s.Login(ctx, "admin", "wrong", false, "")
		}
		if _, err := s.Login(ctx, "admin", password, false, ""); err != nil {
			t.Fatalf("没到上限应当能登录：%v", err)
		}
		for i := 0; i < 4; i++ {
			_, _ = s.Login(ctx, "admin", "wrong", false, "")
		}
		if _, err := s.Login(ctx, "admin", password, false, ""); err != nil {
			t.Fatalf("成功之后计数应当清零：%v", err)
		}
	})
}

// master-login-protection「两步登录与当场验证的计数」：知道密码也不能无限次猜验证码。
func TestTwoFactorCounts(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		a := seedUser(t, bdb, "admin", "admin", true)
		b := s.Bindings()
		keyAny, err := b["account totp setup"](userCtx(a), &command.Invocation{})
		if err != nil {
			t.Fatal(err)
		}
		secret := keyAny.(TOTPKey).Secret
		c, _ := totp.GenerateCode(secret, *now)
		if _, err := b["account totp confirm"](userCtx(a), &command.Invocation{Flags: map[string]any{"code": c}}); err != nil {
			t.Fatal(err)
		}
		ctx := from("198.51.100.7")
		for i := 0; i < 5; i++ {
			first, err := s.Login(ctx, "admin", password, false, "")
			if err != nil || !first.TwoFactorRequired {
				t.Fatalf("第 %d 轮第一步应当拿到票据：%+v %v", i+1, first, err)
			}
			if _, err := s.CompleteTwoFactor(ctx, first.Pending, "000000"); v1.AsError(err).Code != v1.CodeUnauthenticated {
				t.Fatalf("第 %d 轮第二步应当 unauthenticated：%v", i+1, err)
			}
		}
		_, err = s.Login(ctx, "admin", password, false, "")
		wantRateLimited(t, err, *now, time.Hour)
		list := securityEvents(t, bdb, coresecurity.EventFilter{Kind: coresecurity.KindLoginLocked})
		if len(list) != 1 || list[0].Path != twoFactorPath {
			t.Fatalf("触发锁定的是第二步：%+v", list)
		}
		// 锁定期满后，整个登录成功才清零：密码对了拿票据不清零，第二步通过才清。
		*now = now.Add(time.Hour + time.Second)
		for i := 0; i < 4; i++ {
			_, _ = s.Login(ctx, "admin", "wrong", false, "")
		}
		first, err := s.Login(ctx, "admin", password, false, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteTwoFactor(ctx, first.Pending, "000000"); v1.AsError(err).Code != v1.CodeUnauthenticated {
			t.Fatalf("第二步错码：%v", err)
		}
		_, err = s.Login(ctx, "admin", password, false, "")
		wantRateLimited(t, err, *now, time.Hour)
		// 票据的来源 IP 在锁定期内时，第二步是 rate_limited，但票据不作废：锁定期满（且票据还没过 5 分钟）后还能拿它继续。
		*now = now.Add(time.Hour + time.Second)
		s.SetLoginLimits(LoginLimits{MaxAttempts: 5, Window: time.Hour, Lock: 2 * time.Minute, SkipLocalIP: true})
		first, _ = s.Login(ctx, "admin", password, false, "")
		for _, name := range []string{"x1", "x2", "x3", "x4", "x5"} {
			_, _ = s.Login(ctx, name, "wrong", false, "")
		}
		_, err = s.CompleteTwoFactor(ctx, first.Pending, "000000")
		wantRateLimited(t, err, *now, 2*time.Minute)
		*now = now.Add(2*time.Minute + time.Second)
		_, err = s.CompleteTwoFactor(ctx, first.Pending, "000000")
		if e := v1.AsError(err); e.Code != v1.CodeUnauthenticated || !strings.Contains(e.Reason, "验证码不对") {
			t.Fatalf("锁定期满后同一张票据还能用（这次错码）：%v", err)
		}
		if _, err := s.CompleteTwoFactor(ctx, first.Pending, "000000"); !strings.Contains(v1.AsError(err).Reason, "票据不存在") {
			t.Fatalf("比对过一次后票据作废：%v", err)
		}
	})
}

// master-login-protection「两步登录与当场验证的计数」与 master-human-verification「锁定期内不比对」。
func TestVerifyCounts(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		a := seedUser(t, bdb, "admin", "admin", true)
		v := s.Verifier()
		ctx := v1.WithRemote(userCtx(a), v1.Remote{IP: "198.51.100.7"})
		inv := func(pw string) *command.Invocation {
			return &command.Invocation{Path: []string{"settings", "gates", "set"}, Verify: &command.Verification{Password: pw}}
		}
		for i := 0; i < 5; i++ {
			if err := v.Verify(ctx, inv("wrong")); v1.AsError(err).Code != v1.CodeHumanRequired {
				t.Fatalf("第 %d 次错密码应当 human_required：%v", i+1, err)
			}
		}
		wantRateLimited(t, v.Verify(ctx, inv(password)), *now, time.Hour)
		list := securityEvents(t, bdb, coresecurity.EventFilter{})
		if len(list) != 5 || list[0].Kind != coresecurity.KindVerifyLocked || list[1].Kind != coresecurity.KindVerifyFail || list[0].Path != "settings gates set" || list[0].Username != "admin" {
			t.Fatalf("当场验证的事件：%+v", list)
		}
		// 当场验证与登录共用计数：锁定期内网页登录同样被拒。
		_, err := s.Login(from("198.51.100.8"), "admin", password, false, "")
		wantRateLimited(t, err, *now, time.Hour)
		// 期满后：通过清零；本机管理员经 socket 不受限。
		*now = now.Add(time.Hour + time.Second)
		for i := 0; i < 4; i++ {
			_ = v.Verify(ctx, inv("wrong"))
		}
		if err := v.Verify(ctx, inv(password)); err != nil {
			t.Fatalf("没到上限应当通过：%v", err)
		}
		for i := 0; i < 4; i++ {
			_ = v.Verify(ctx, inv("wrong"))
		}
		if err := v.Verify(ctx, inv(password)); err != nil {
			t.Fatalf("通过之后计数应当清零：%v", err)
		}
		for i := 0; i < 5; i++ {
			_ = v.Verify(ctx, inv("wrong"))
		}
		local := v1.WithRemote(v1.WithIdentity(context.Background(), v1.LocalAdmin("root")), v1.Remote{Socket: true, Local: true})
		if err := v.Verify(local, &command.Invocation{Path: []string{"settings", "gates", "set"}, Verify: &command.Verification{Password: password, User: "admin"}}); err != nil {
			t.Fatalf("经 unix socket 的当场验证不受限：%v", err)
		}
	})
}

// 只缺第二因素不算一次猜测。
func TestVerifyMissingSecondFactorDoesNotCount(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		a := seedUser(t, bdb, "admin", "admin", true)
		b := s.Bindings()
		keyAny, _ := b["account totp setup"](userCtx(a), &command.Invocation{})
		c, _ := totp.GenerateCode(keyAny.(TOTPKey).Secret, *now)
		if _, err := b["account totp confirm"](userCtx(a), &command.Invocation{Flags: map[string]any{"code": c}}); err != nil {
			t.Fatal(err)
		}
		ctx := v1.WithRemote(userCtx(a), v1.Remote{IP: "198.51.100.7"})
		for i := 0; i < 10; i++ {
			err := s.Verifier().Verify(ctx, &command.Invocation{Path: []string{"account", "set-password"}, Verify: &command.Verification{Password: password}})
			if v1.AsError(err).Code != v1.CodeHumanRequired {
				t.Fatalf("缺第二因素应当 human_required：%v", err)
			}
		}
		if n := len(securityEvents(t, bdb, coresecurity.EventFilter{})); n != 0 {
			t.Fatalf("缺第二因素不计失败、不记事件，得到 %d 条", n)
		}
	})
}

// 定期清理：锁定期满的、没锁而窗口已过的条目清掉，其余留着。
func TestLimiterSweep(t *testing.T) {
	l := newLimiter()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	failAt := func(ip, user string, at time.Time) {
		l.reserve(ip, user, at)
		l.fail(ip, user, at)
	}
	for i := 0; i < 5; i++ {
		failAt("198.51.100.7", "admin", now) // 两个维度都锁到 now+1h
	}
	failAt("198.51.100.8", "old", now.Add(-2*time.Hour)) // 窗口早已过
	failAt("198.51.100.9", "fresh", now)                 // 还在窗口里
	l.sweep(now)
	if _, ok := l.users["old"]; ok {
		t.Fatal("窗口已过的条目应当清掉")
	}
	if _, ok := l.users["fresh"]; !ok {
		t.Fatal("还在窗口里的条目要留着")
	}
	if _, ok := l.users["admin"]; !ok {
		t.Fatal("锁定期内的条目要留着")
	}
	l.sweep(now.Add(time.Hour + time.Second))
	if _, ok := l.users["admin"]; ok {
		t.Fatal("锁定期满后应当清掉")
	}
	if _, ok := l.ips["198.51.100.7"]; ok {
		t.Fatal("IP 维度锁定期满后也清掉")
	}
}

// 同一波并发的错密码：最多只有上限那么多个走到比对，其余直接 rate_limited（先占后比对）。
func TestLoginLimitConcurrent(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, _ := limited(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		ctx := from("198.51.100.7")
		var wg sync.WaitGroup
		codes := make(chan v1.Code, 30)
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.Login(ctx, "admin", "wrong", false, "")
				codes <- v1.AsError(err).Code
			}()
		}
		wg.Wait()
		close(codes)
		compared := 0
		for c := range codes {
			if c == v1.CodeUnauthenticated {
				compared++
			} else if c != v1.CodeRateLimited {
				t.Fatalf("只该有 unauthenticated 与 rate_limited，得到 %s", c)
			}
		}
		if compared != 5 {
			t.Fatalf("三十个并发里恰好五个走到比对，得到 %d", compared)
		}
	})
}

// 不算一次猜测的结局把占的那一次退回去：开了两步验证时密码对了、验证码没过都不计。
func TestReservationReleased(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		a := seedUser(t, bdb, "admin", "admin", true)
		b := s.Bindings()
		keyAny, _ := b["account totp setup"](userCtx(a), &command.Invocation{})
		c, _ := totp.GenerateCode(keyAny.(TOTPKey).Secret, *now)
		if _, err := b["account totp confirm"](userCtx(a), &command.Invocation{Flags: map[string]any{"code": c}}); err != nil {
			t.Fatal(err)
		}
		ctx := from("198.51.100.7")
		for i := 0; i < 10; i++ {
			if res, err := s.Login(ctx, "admin", password, false, ""); err != nil || !res.TwoFactorRequired {
				t.Fatalf("第 %d 次密码对了应当拿到票据：%v", i+1, err)
			}
		}
		f := &fakeCaptcha{pass: false}
		s.SetCaptcha(f)
		s.SetTurnstileKeys("0x4AAAAAAAsitekeyforsatchel", "0x4AAAAAAAsecretforsatchel")
		for i := 0; i < 10; i++ {
			_, _ = s.Login(ctx, "admin", password, false, "bad")
		}
		f.pass = true
		if res, err := s.Login(ctx, "admin", password, false, "good"); err != nil || !res.TwoFactorRequired {
			t.Fatalf("前面那些都不算猜测，不该被锁：%v", err)
		}
	})
}

// 停用账号：密码正确也计一次失败（回应照旧是 forbidden），不然能不受限地试出停用账号的密码。
func TestDisabledAccountCounts(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, now := limited(t, bdb)
		seedUser(t, bdb, "dora", "admin", false)
		ctx := from("198.51.100.7")
		for i := 0; i < 5; i++ {
			if _, err := s.Login(ctx, "dora", password, false, ""); v1.AsError(err).Code != v1.CodeForbidden {
				t.Fatalf("第 %d 次停用账号应当 forbidden：%v", i+1, err)
			}
		}
		_, err := s.Login(ctx, "dora", password, false, "")
		wantRateLimited(t, err, *now, time.Hour)
		if n := len(securityEvents(t, bdb, coresecurity.EventFilter{})); n != 5 {
			t.Fatalf("五次都记安全事件，得到 %d", n)
		}
	})
}
