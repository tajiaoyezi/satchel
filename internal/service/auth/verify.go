package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Verifier 是第 05 章七组的当场验证（master-human-verification），实现 authz.HumanVerifier。
// 每次都完整验一遍：不缓存结果、不签票据；恢复码通过时已作废，命令随后失败也不退回。
type Verifier struct {
	s *Service
}

// Verifier 返回验证器。
func (s *Service) Verifier() *Verifier { return &Verifier{s: s} }

func humanRequired(reason, next string) *v1.Error {
	return v1.New(v1.CodeHumanRequired, reason).WithNext(next)
}

// Verify 按身份决定验谁，再验密码与（开了两步验证时）第二因素。
func (v *Verifier) Verify(ctx context.Context, inv *command.Invocation) error {
	id := v1.IdentityFrom(ctx)
	ver := inv.Verify
	if ver == nil {
		ver = &command.Verification{}
	}
	var account *users.Account
	switch id.ActorKind {
	case v1.ActorUser:
		if ver.User != "" && ver.User != id.Actor {
			return humanRequired("当场验证只能验自己的账号", "去掉 verify-user，或填自己的用户名")
		}
		a, err := v.s.users.GetByUsername(ctx, id.Actor)
		if err != nil {
			return humanRequired("会话对应的账号已不存在", "重新登录")
		}
		account = a
	case v1.ActorLocalAdmin:
		if strings.TrimSpace(ver.User) == "" {
			return humanRequired("本机管理员不是账号：当场验证要用 verify-user 指明一个管理员账号", "加上 --verify-user <管理员用户名> 并输入它的密码")
		}
		a, err := v.s.users.GetByUsername(ctx, strings.TrimSpace(ver.User))
		if errors.Is(err, users.ErrNotFound) || (err == nil && (a.Deleted || a.Role != v1.RoleAdmin)) {
			return humanRequired("verify-user 指向的不是一个管理员账号", "填一个存在的管理员用户名")
		}
		if err != nil {
			return err
		}
		account = a
	default:
		return humanRequired("这个身份不能做只有人能做的操作", "在网页或 CLI 上由管理员本人执行")
	}
	if ver.Password == "" || !CheckPassword(account.PasswordHash, ver.Password) {
		return humanRequired("当场验证的密码不对", "重新输入密码")
	}
	if account.TOTPEnabled {
		if strings.TrimSpace(ver.Code) == "" {
			return humanRequired("账号开了两步验证，当场验证还要第二因素", "加上验证器当前的码或一枚恢复码")
		}
		ok, err := v.s.checkSecondFactor(ctx, account, ver.Code)
		if err != nil {
			return err
		}
		if !ok {
			return humanRequired("当场验证的验证码不对，或这枚恢复码已经用过", "输入验证器当前的码，或一枚没用过的恢复码")
		}
	}
	return nil
}
