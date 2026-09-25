package auth

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 审查修补：停用的管理员不能给本机管理员当「当场验证」用；停用账号自己的身份也验不过。
func TestVerifierRejectsDisabledAccounts(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		off := seedUser(t, bdb, "off", "admin", false)
		seedUser(t, bdb, "admin", "admin", true)
		v := s.Verifier()
		root := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		err := v.Verify(root, &command.Invocation{Verify: &command.Verification{Password: password, User: "off"}})
		if e := v1.AsError(err); err == nil || e.Code != v1.CodeHumanRequired || e.Reason == "" {
			t.Fatalf("停用的管理员不能当场验证：%v", err)
		}
		if err := v.Verify(root, &command.Invocation{Verify: &command.Verification{Password: password, User: "admin"}}); err != nil {
			t.Fatalf("启用中的管理员应当验过：%v", err)
		}
		if err := v.Verify(userCtx(off), &command.Invocation{Verify: &command.Verification{Password: password}}); err == nil || v1.AsError(err).Code != v1.CodeHumanRequired {
			t.Fatalf("停用账号的身份验自己也不该过：%v", err)
		}
	})
}

// 审查修补：已启用两步验证后 account totp setup 是 conflict，密钥与恢复码不动；登录第二步验错票据即作废。
func TestTOTPSetupAfterEnabledAndPendingConsumed(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		ctx := context.Background()
		a := seedUser(t, bdb, "admin", "admin", true)
		uctx := userCtx(a)
		b := s.Bindings()
		keyAny, err := b["account totp setup"](uctx, &command.Invocation{})
		if err != nil {
			t.Fatal(err)
		}
		secret := keyAny.(TOTPKey).Secret
		// 未启用时重复 setup 换一把新密钥。
		key2Any, _ := b["account totp setup"](uctx, &command.Invocation{})
		if key2Any.(TOTPKey).Secret == secret {
			t.Fatal("未启用时重复 setup 应当换密钥")
		}
		secret = key2Any.(TOTPKey).Secret
		if _, err := b["account totp confirm"](uctx, &command.Invocation{Flags: map[string]any{"code": code(t, s, secret)}}); err != nil {
			t.Fatal(err)
		}
		advance(s, time.Minute)
		if _, err := b["account totp setup"](uctx, &command.Invocation{}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("已启用后 setup 应当 conflict：%v", err)
		}
		a, _ = s.users.GetByUsername(ctx, "admin")
		if !a.TOTPEnabled || a.TOTPSecret != secret || len(a.RecoveryCodes) != 8 {
			t.Fatalf("被拒的 setup 不该动密钥或恢复码：%+v", a)
		}
		// 第二步验错：票据作废，同一票据再给真码也不认。
		first, err := s.Login(ctx, "admin", password, false, "")
		if err != nil || !first.TwoFactorRequired {
			t.Fatalf("登录第一步：%+v %v", first, err)
		}
		if _, err := s.CompleteTwoFactor(ctx, first.Pending, "000000"); err == nil {
			t.Fatal("错码应当失败")
		}
		if _, err := s.CompleteTwoFactor(ctx, first.Pending, code(t, s, secret)); err == nil || v1.AsError(err).Code != v1.CodeUnauthenticated {
			t.Fatalf("验错后票据应当已作废：%v", err)
		}
		// 重新登录拿新票据就能过。
		again, _ := s.Login(ctx, "admin", password, false, "")
		if _, err := s.CompleteTwoFactor(ctx, again.Pending, code(t, s, secret)); err != nil {
			t.Fatalf("新票据应当能完成：%v", err)
		}
	})
}
