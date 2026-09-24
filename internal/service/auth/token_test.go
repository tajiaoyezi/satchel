package auth

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-accounts「令牌看签发者的账号」的服务层部分：令牌身份的 account show 是签发者的账号
// （人类专属的 account * 对令牌身份在 authz 就被拒，见 middleware/authz 的测试）。
func TestTokenIdentityAccount(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := service(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		tokenID := int64(5)
		ctx := v1.WithIdentity(context.Background(), v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokenID, Scopes: []v1.Scope{v1.ScopeRead}})
		info, err := s.Bindings()["account show"](ctx, &command.Invocation{})
		if err != nil || info.(AccountInfo).Username != "admin" || info.(AccountInfo).Role != v1.RoleAdmin {
			t.Fatalf("令牌的 account show 应当是签发者的账号：%+v %v", info, err)
		}
		system := v1.WithIdentity(context.Background(), v1.Identity{Actor: "scheduler", ActorKind: v1.ActorSystem, Role: v1.RoleAdmin})
		if _, err := s.Bindings()["account show"](system, &command.Invocation{}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("系统身份没有账号：%v", err)
		}
	})
}
