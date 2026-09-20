package auth

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

const password = "correct horse battery"

func service(t *testing.T, bdb *bun.DB) *Service {
	t.Helper()
	return New(users.New(bdb, store.New(bdb, schema.Default())), sessions.New(bdb))
}

// seedUser 直接插一个账号（M3 之前没有用户管理命令）。
func seedUser(t *testing.T, bdb *bun.DB, name, role string, active bool) *users.Account {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	u := &model.User{Username: name, Role: role, IsActive: active, PasswordHash: hash, RecoveryCodes: json.RawMessage(`[]`),
		NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now(), ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err := users.New(bdb, store.New(bdb, schema.Default())).GetByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func userCtx(a *users.Account) context.Context {
	return v1.WithIdentity(context.Background(), identityFor(a))
}

// code 按服务的时钟算当前 TOTP 码；advance 把服务的时钟拨快，换下一个周期的码（防重放让同一个码只能用一次）。
func code(t *testing.T, s *Service, secret string) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, s.now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func advance(s *Service, d time.Duration) {
	base := s.now()
	s.now = func() time.Time { return base.Add(d) }
}

func TestRules(t *testing.T) {
	for _, ok := range []string{"admin", "a12", "a-b_c", strings.Repeat("a", 32)} {
		if err := ValidateUsername(ok); err != nil {
			t.Errorf("%q 应当合规：%v", ok, err)
		}
	}
	for _, bad := range []string{"Ad", "-bad", "a b", "ab", "a1", "Admin", strings.Repeat("a", 33), "_x", ""} {
		if err := ValidateUsername(bad); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("%q 应当 bad_request", bad)
		}
	}
	if err := ValidatePassword("1234567"); err == nil {
		t.Error("7 个字符应当拒")
	}
	if err := ValidatePassword("密码密码密码密码"); err != nil {
		t.Error("8 个字符（多字节）应当放行")
	}
	if err := ValidatePassword(strings.Repeat("x", 73)); err == nil {
		t.Error("超过 72 字节应当拒")
	}
	h, _ := HashPassword("secret12")
	if !strings.HasPrefix(h, "$2a$") || !CheckPassword(h, "secret12") || CheckPassword(h, "secret13") {
		t.Fatal("bcrypt 哈希与比对不对")
	}
}

// master-web-session：登录、会话哈希、过期、停用、登出、Resolve。
func TestLoginAndSessions(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		ctx := context.Background()
		seedUser(t, bdb, "admin", "admin", true)
		seedUser(t, bdb, "bob", "user", true)
		seedUser(t, bdb, "off", "user", false)

		res, err := s.Login(ctx, "admin", password, false)
		if err != nil || res.Token == "" || res.Username != "admin" || res.Role != v1.RoleAdmin || res.TwoFactorRequired {
			t.Fatalf("登录：%+v %v", res, err)
		}
		if d := time.Until(res.ExpiresAt); d < 23*time.Hour || d > 25*time.Hour {
			t.Fatalf("有效期应当约 24 小时，得到 %v", d)
		}
		long, _ := s.Login(ctx, "admin", password, true)
		if d := time.Until(long.ExpiresAt); d < 29*24*time.Hour {
			t.Fatalf("记住我应当约 30 天，得到 %v", d)
		}
		var rows []model.Session
		if err := bdb.NewSelect().Model(&rows).Scan(ctx); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if len(row.TokenHash) != 64 || row.TokenHash == res.Token || row.TokenHash == long.Token {
				t.Fatalf("库里应当只有 64 位十六进制的哈希：%s", row.TokenHash)
			}
		}
		id, hash, ok, err := s.Resolve(ctx, res.Token)
		if err != nil || !ok || id.ActorKind != v1.ActorUser || id.Actor != "admin" || id.Role != v1.RoleAdmin || len(id.Danger) != 6 || hash != HashToken(res.Token) {
			t.Fatalf("Resolve 管理员：%+v %v %v", id, ok, err)
		}
		bobRes, _ := s.Login(ctx, "bob", password, false)
		bid, _, _, _ := s.Resolve(ctx, bobRes.Token)
		if bid.Role != v1.RoleUser || len(bid.Danger) != 0 || !bid.HasScope(v1.ScopeOperate) || bid.HasScope(v1.ScopeSecrets) {
			t.Fatalf("普通用户的权限：%+v", bid)
		}
		for _, bad := range [][2]string{{"admin", "wrong"}, {"nobody", password}, {"", ""}} {
			_, err := s.Login(ctx, bad[0], bad[1], false)
			if e := v1.AsError(err); err == nil || e.Code != v1.CodeUnauthenticated || strings.Contains(e.Reason, "不存在") {
				t.Errorf("%v 应当 unauthenticated 且不说哪个错：%v", bad, err)
			}
		}
		if _, err := s.Login(ctx, "off", password, false); err == nil || v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("停用应当 forbidden：%v", err)
		}
		// 过期与登出。
		s.now = func() time.Time { return time.Now().UTC().Add(25 * time.Hour) }
		if _, _, ok, _ := s.Resolve(ctx, res.Token); ok {
			t.Fatal("过期会话应当无效")
		}
		if _, _, ok, _ := s.Resolve(ctx, long.Token); !ok {
			t.Fatal("30 天的会话应当还有效")
		}
		s.now = func() time.Time { return time.Now().UTC() }
		if err := s.Logout(ctx, long.Token); err != nil {
			t.Fatal(err)
		}
		if _, _, ok, _ := s.Resolve(ctx, long.Token); ok {
			t.Fatal("登出后应当无效")
		}
		if err := s.Logout(ctx, "garbage"); err != nil {
			t.Fatal("登出不存在的会话不算错")
		}
		if _, _, ok, _ := s.Resolve(ctx, ""); ok {
			t.Fatal("空令牌无效")
		}
	})
}

// master-two-factor：启用、两步登录、恢复码单次与并发、TOTP 重放、不足两枚、禁用。
func TestTwoFactor(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		ctx := context.Background()
		a := seedUser(t, bdb, "admin", "admin", true)
		uctx := userCtx(a)
		b := s.Bindings()

		if _, err := b["account totp confirm"](uctx, &command.Invocation{Flags: map[string]any{"code": "000000"}}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("没 setup 就 confirm 应当 conflict：%v", err)
		}
		keyAny, err := b["account totp setup"](uctx, &command.Invocation{})
		if err != nil {
			t.Fatal(err)
		}
		key := keyAny.(TOTPKey)
		if key.Secret == "" || !strings.HasPrefix(key.URL, "otpauth://totp/") || !strings.Contains(key.URL, "Satchel") {
			t.Fatalf("密钥形状：%+v", key)
		}
		info, _ := b["account show"](uctx, &command.Invocation{})
		if ai := info.(AccountInfo); !ai.TOTPPending || ai.TOTPEnabled {
			t.Fatalf("setup 后应当待启用：%+v", ai)
		}
		if _, err := b["account totp confirm"](uctx, &command.Invocation{Flags: map[string]any{"code": "000000"}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("码不对应当 bad_request：%v", err)
		}
		codesAny, err := b["account totp confirm"](uctx, &command.Invocation{Flags: map[string]any{"code": code(t, s, key.Secret)}})
		if err != nil {
			t.Fatal(err)
		}
		advance(s, time.Minute)
		codes := codesAny.(RecoveryCodes).RecoveryCodes
		if len(codes) != 8 {
			t.Fatalf("应当 8 枚恢复码：%v", codes)
		}
		for _, c := range codes {
			if len(c) != 8 || strings.ToLower(c) != c {
				t.Fatalf("恢复码形状：%s", c)
			}
		}
		a, _ = s.users.GetByUsername(ctx, "admin")
		for _, h := range a.RecoveryCodes {
			for _, c := range codes {
				if h == c {
					t.Fatal("库里不该有恢复码明文")
				}
			}
		}
		info, _ = b["account show"](uctx, &command.Invocation{})
		if ai := info.(AccountInfo); !ai.TOTPEnabled || ai.RecoveryCodesRemaining != 8 || ai.RecoveryCodesLow {
			t.Fatalf("启用后：%+v", ai)
		}

		// 两步登录：第一步不发会话；TOTP 完成；同一个码不能再用；pending 只能用一次。
		first, err := s.Login(ctx, "admin", password, false)
		if err != nil || !first.TwoFactorRequired || first.Token != "" || first.Pending == "" {
			t.Fatalf("第一步：%+v %v", first, err)
		}
		c1 := code(t, s, key.Secret)
		done, err := s.CompleteTwoFactor(ctx, first.Pending, c1)
		if err != nil || done.Token == "" || *done.RecoveryCodesRemaining != 8 || done.RecoveryCodesLow {
			t.Fatalf("第二步：%+v %v", done, err)
		}
		if _, err := s.CompleteTwoFactor(ctx, first.Pending, c1); err == nil || v1.AsError(err).Code != v1.CodeUnauthenticated {
			t.Fatalf("pending 用过应当 unauthenticated：%v", err)
		}
		second, _ := s.Login(ctx, "admin", password, false)
		if _, err := s.CompleteTwoFactor(ctx, second.Pending, c1); err == nil || v1.AsError(err).Code != v1.CodeUnauthenticated {
			t.Fatalf("同一个 TOTP 码重放应当 unauthenticated：%v", err)
		}

		// 恢复码：一枚只能用一次，不关两步验证。
		third, _ := s.Login(ctx, "admin", password, false)
		if _, err := s.CompleteTwoFactor(ctx, third.Pending, codes[0]); err != nil {
			t.Fatalf("恢复码登录：%v", err)
		}
		fourth, _ := s.Login(ctx, "admin", password, false)
		if _, err := s.CompleteTwoFactor(ctx, fourth.Pending, codes[0]); err == nil || v1.AsError(err).Code != v1.CodeUnauthenticated {
			t.Fatalf("同一枚恢复码第二次应当 unauthenticated：%v", err)
		}
		a, _ = s.users.GetByUsername(ctx, "admin")
		if !a.TOTPEnabled || len(a.RecoveryCodes) != 7 {
			t.Fatalf("用恢复码后两步验证仍开、剩 7 枚：%+v", a)
		}
		// 并发十次同一枚：恰好一个成功（每次都要新 pending）。
		var pendings []string
		for i := 0; i < 10; i++ {
			r, _ := s.Login(ctx, "admin", password, false)
			pendings = append(pendings, r.Pending)
		}
		var wg sync.WaitGroup
		results := make(chan error, 10)
		for _, p := range pendings {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				_, err := s.CompleteTwoFactor(ctx, p, codes[1])
				results <- err
			}(p)
		}
		wg.Wait()
		close(results)
		okCount := 0
		for err := range results {
			if err == nil {
				okCount++
			}
		}
		if okCount != 1 {
			t.Fatalf("并发同一枚恢复码应当恰好一个成功，得到 %d", okCount)
		}
		// 假重启：新的 Service 实例，同一枚码仍然不能再用。
		s2 := service(t, bdb)
		fifth, _ := s2.Login(ctx, "admin", password, false)
		if _, err := s2.CompleteTwoFactor(ctx, fifth.Pending, codes[1]); err == nil {
			t.Fatal("重启后同一枚恢复码仍应当不可用")
		}
		// 用到只剩一枚：codes[0]、codes[1] 已用，再用 codes[2] 到 codes[6]。
		for i := 2; i <= 6; i++ {
			r, _ := s.Login(ctx, "admin", password, false)
			if _, err := s.CompleteTwoFactor(ctx, r.Pending, codes[i]); err != nil {
				t.Fatalf("用恢复码 %d：%v", i, err)
			}
		}
		advance(s, time.Minute)
		r, _ := s.Login(ctx, "admin", password, false)
		last, err := s.CompleteTwoFactor(ctx, r.Pending, code(t, s, key.Secret))
		if err != nil || *last.RecoveryCodesRemaining != 1 || !last.RecoveryCodesLow {
			t.Fatalf("只剩一枚应当 low：%+v %v", last, err)
		}
		// 重新生成后旧码作废。
		regen, err := b["account recovery-codes regenerate"](uctx, &command.Invocation{})
		if err != nil || len(regen.(RecoveryCodes).RecoveryCodes) != 8 {
			t.Fatalf("regenerate：%v %v", regen, err)
		}
		r, _ = s.Login(ctx, "admin", password, false)
		if _, err := s.CompleteTwoFactor(ctx, r.Pending, codes[7]); err == nil {
			t.Fatal("旧码应当作废")
		}
		// 禁用。
		if _, err := b["account totp disable"](uctx, &command.Invocation{}); err != nil {
			t.Fatal(err)
		}
		plain, err := s.Login(ctx, "admin", password, false)
		if err != nil || plain.TwoFactorRequired || plain.Token == "" {
			t.Fatalf("禁用后应当直接登录：%+v %v", plain, err)
		}
		if _, err := b["account recovery-codes regenerate"](uctx, &command.Invocation{}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("没开两步验证不能 regenerate：%v", err)
		}
	})
}

// master-human-verification：验谁、密码、第二因素、四种失败、恢复码消耗。
func TestVerifier(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		ctx := context.Background()
		admin := seedUser(t, bdb, "admin", "admin", true)
		bob := seedUser(t, bdb, "bob", "user", true)
		v := s.Verifier()
		inv := func(pw, code, user string) *command.Invocation {
			return &command.Invocation{Path: []string{"account", "set-password"}, Verify: &command.Verification{Password: pw, Code: code, User: user}}
		}
		if err := v.Verify(userCtx(admin), inv(password, "", "")); err != nil {
			t.Fatalf("用户验自己：%v", err)
		}
		cases := []struct {
			name string
			ctx  context.Context
			inv  *command.Invocation
			want string
		}{
			{"密码错", userCtx(admin), inv("wrong", "", ""), "密码不对"},
			{"没带 verify", userCtx(admin), &command.Invocation{}, "密码不对"},
			{"验别人", userCtx(admin), inv(password, "", "bob"), "只能验自己"},
			{"本机管理员缺 verify-user", v1.WithIdentity(ctx, v1.LocalAdmin("root")), inv(password, "", ""), "指明"},
			{"本机管理员指普通用户", v1.WithIdentity(ctx, v1.LocalAdmin("root")), inv(password, "", "bob"), "不是一个管理员"},
			{"本机管理员指不存在", v1.WithIdentity(ctx, v1.LocalAdmin("root")), inv(password, "", "nobody"), "不是一个管理员"},
			{"令牌身份", v1.WithIdentity(ctx, v1.Identity{ActorKind: v1.ActorToken, Role: v1.RoleAdmin}), inv(password, "", "admin"), "不能做"},
		}
		for _, tc := range cases {
			err := v.Verify(tc.ctx, tc.inv)
			if e := v1.AsError(err); err == nil || e.Code != v1.CodeHumanRequired || !strings.Contains(e.Reason, tc.want) {
				t.Errorf("%s 应当 human_required 且含 %q：%v", tc.name, tc.want, err)
			}
		}
		if err := v.Verify(v1.WithIdentity(ctx, v1.LocalAdmin("root")), inv(password, "", "admin")); err != nil {
			t.Fatalf("本机管理员指明管理员账号应当通过：%v", err)
		}
		_ = bob
		// 开两步验证后要第二因素。
		b := s.Bindings()
		key, _ := b["account totp setup"](userCtx(admin), &command.Invocation{})
		secret := key.(TOTPKey).Secret
		codesAny, err := b["account totp confirm"](userCtx(admin), &command.Invocation{Flags: map[string]any{"code": code(t, s, secret)}})
		if err != nil {
			t.Fatal(err)
		}
		advance(s, time.Minute)
		codes := codesAny.(RecoveryCodes).RecoveryCodes
		admin, _ = s.users.GetByUsername(ctx, "admin")
		if err := v.Verify(userCtx(admin), inv(password, "", "")); err == nil || !strings.Contains(v1.AsError(err).Reason, "第二因素") {
			t.Fatalf("缺第二因素：%v", err)
		}
		if err := v.Verify(userCtx(admin), inv(password, "000000", "")); err == nil || !strings.Contains(v1.AsError(err).Reason, "验证码不对") {
			t.Fatalf("验证码错：%v", err)
		}
		c := code(t, s, secret)
		if err := v.Verify(userCtx(admin), inv(password, c, "")); err != nil {
			t.Fatalf("TOTP 通过：%v", err)
		}
		if err := v.Verify(userCtx(admin), inv(password, c, "")); err == nil {
			t.Fatal("同一个 TOTP 码不能再用（不缓存、不签票据）")
		}
		if err := v.Verify(userCtx(admin), inv(password, codes[0], "")); err != nil {
			t.Fatalf("恢复码通过：%v", err)
		}
		if err := v.Verify(userCtx(admin), inv(password, codes[0], "")); err == nil {
			t.Fatal("恢复码用过一次就作废")
		}
		admin, _ = s.users.GetByUsername(ctx, "admin")
		if len(admin.RecoveryCodes) != 7 || !admin.TOTPEnabled {
			t.Fatalf("消耗一枚、开关不变：%+v", admin)
		}
	})
}

// master-setup-wizard 与 master-accounts 的命令。
func TestSetupAndAccountCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		ctx := context.Background()
		b := s.Bindings()
		st, _ := b["setup status"](ctx, &command.Invocation{})
		if ss := st.(SetupStatus); ss.Initialized || !ss.Paths["create_admin"].Available || ss.Paths["restore_backup"].Available || ss.Paths["import_mmwx"].Available {
			t.Fatalf("空库状态：%+v", ss)
		}
		for _, bad := range []map[string]any{{"username": "Ad", "password": password}, {"username": "admin", "password": "short"}, {"username": "", "password": password}} {
			if _, err := b["setup init"](ctx, &command.Invocation{Flags: bad}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
				t.Errorf("%v 应当 bad_request：%v", bad, err)
			}
		}
		res, err := b["setup init"](ctx, &command.Invocation{Flags: map[string]any{"username": "admin", "password": password, "email": "a@b.c"}})
		if err != nil || res.(SetupResult).Username != "admin" || res.(SetupResult).Role != v1.RoleAdmin {
			t.Fatalf("setup init：%v %v", res, err)
		}
		st, _ = b["setup status"](ctx, &command.Invocation{})
		if ss := st.(SetupStatus); !ss.Initialized || ss.Paths["create_admin"].Available {
			t.Fatalf("初始化后状态：%+v", ss)
		}
		if _, err := b["setup init"](ctx, &command.Invocation{Flags: map[string]any{"username": "again", "password": password}}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("已初始化应当 conflict：%v", err)
		}
		if _, err := s.Login(ctx, "admin", password, false); err != nil {
			t.Fatalf("建好的管理员应当能登录：%v", err)
		}
		// account show / set-password：两处登录，改密后另一处作废、当前保留。
		a, _ := s.users.GetByUsername(ctx, "admin")
		one, _ := s.Login(ctx, "admin", password, false)
		two, _ := s.Login(ctx, "admin", password, false)
		uctx := WithSessionHash(userCtx(a), HashToken(one.Token))
		info, _ := b["account show"](uctx, &command.Invocation{})
		if ai := info.(AccountInfo); ai.Sessions != 3 || ai.Email != "a@b.c" || ai.Role != v1.RoleAdmin {
			t.Fatalf("account show：%+v", ai)
		}
		if _, err := b["account set-password"](uctx, &command.Invocation{Flags: map[string]any{"new-password": "short"}}); err == nil {
			t.Fatal("短密码应当拒")
		}
		out, err := b["account set-password"](uctx, &command.Invocation{Flags: map[string]any{"new-password": "new password 123"}})
		if err != nil || out.(PasswordChanged).SessionsRevoked != 2 {
			t.Fatalf("改密：%v %v", out, err)
		}
		if _, _, ok, _ := s.Resolve(ctx, one.Token); !ok {
			t.Fatal("当前会话应当保留")
		}
		if _, _, ok, _ := s.Resolve(ctx, two.Token); ok {
			t.Fatal("另一处会话应当作废")
		}
		if _, err := s.Login(ctx, "admin", password, false); err == nil {
			t.Fatal("旧密码应当不能登录")
		}
		if _, err := s.Login(ctx, "admin", "new password 123", false); err != nil {
			t.Fatalf("新密码应当能登录：%v", err)
		}
		// 本机管理员不是账号。
		for _, name := range []string{"account show", "account set-password", "account totp setup"} {
			_, err := b[name](v1.WithIdentity(ctx, v1.LocalAdmin("root")), &command.Invocation{Flags: map[string]any{"new-password": "whatever123"}})
			if e := v1.AsError(err); err == nil || e.Code != v1.CodeBadRequest || !strings.Contains(e.Next, "admin reset-password") {
				t.Errorf("%s 对本机管理员应当 bad_request 并指引：%v", name, err)
			}
		}
	})
}
