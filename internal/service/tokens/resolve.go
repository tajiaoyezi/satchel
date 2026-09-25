package tokens

import (
	"context"
	"errors"
	"strings"

	core "github.com/satchel/satchel/internal/core/tokens"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Resolve 把一把令牌解析成身份对象（给 authn 用，master-api-tokens「令牌解析成身份」）。返回 false 表示令牌无效：
// 不是 sat_ 开头、找不到、已吊销、已过期、签发者不存在或已软删除或已停用，或查库出错（失败时关门，错误只记日志）。
// 有效时身份的 actor 是签发者、role 是签发者当下的角色、scopes 与 danger 是权限范围与角色上限的交集；
// 最后使用时间按 touchInterval 节流写入。
func (s *Service) Resolve(ctx context.Context, token string) (v1.Identity, bool) {
	id, ok, _ := s.ResolveToken(ctx, token)
	return id, ok
}

// ResolveToken 同 Resolve，但把「查库出错」作为 error 单独交出来（ok 仍为假）：authn 据此只对真正无效的令牌计一次
// 令牌校验失败，库出问题时的正常重试不会被当成猜令牌而封掉来源 IP（master-login-protection）。
func (s *Service) ResolveToken(ctx context.Context, token string) (v1.Identity, bool, error) {
	if !strings.HasPrefix(token, Prefix) {
		return v1.Identity{}, false, nil
	}
	t, err := s.repo.GetByHash(ctx, Hash(token))
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			s.logger.Error("解析令牌时查库出错，按无效处理", "error", err)
			return v1.Identity{}, false, err
		}
		return v1.Identity{}, false, nil
	}
	now := s.now()
	if t.Revoked || (t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)) {
		return v1.Identity{}, false, nil
	}
	a, err := s.users.GetByUsername(ctx, t.Owner)
	if err != nil {
		if !errors.Is(err, users.ErrNotFound) {
			s.logger.Error("解析令牌时查签发者出错，按无效处理", "error", err, "token_id", t.ID)
			return v1.Identity{}, false, err
		}
		return v1.Identity{}, false, nil
	}
	if a.Deleted || !a.IsActive {
		return v1.Identity{}, false, nil
	}
	scopes, danger := intersect(t.Grant, a.Role)
	id := t.ID
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= touchInterval {
		// 写失败不影响这次请求：最后使用时间只是给人看的。写入不随请求取消。
		if err := s.repo.TouchLastUsed(context.WithoutCancel(ctx), t.ID, now); err != nil {
			s.logger.Warn("写令牌的最后使用时间失败", "error", err, "token_id", t.ID)
		}
	}
	return v1.Identity{Actor: t.Owner, ActorKind: v1.ActorToken, Role: a.Role, TokenID: &id, Scopes: scopes, Danger: danger}, true, nil
}
