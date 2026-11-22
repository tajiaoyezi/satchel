package auth

import (
	"context"
	"errors"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// NewForDB 按已打开的数据库连接装配 auth.Service；本地 CLI 命令与测试用它做应急账号操作。
func NewForDB(bdb *bun.DB) *Service {
	return New(users.New(bdb, store.New(bdb, schema.Default())), sessions.New(bdb))
}

// LookupAdmin 按用户名查找未删除的管理员账号：不存在或已删除返回 not_found，不是管理员返回 bad_request。
func (s *Service) LookupAdmin(ctx context.Context, username string) (*users.Account, error) {
	a, err := s.users.GetByUsername(ctx, username)
	if errors.Is(err, users.ErrNotFound) || (err == nil && a.Deleted) {
		return nil, v1.Newf(v1.CodeNotFound, "没有账号 %s", username)
	}
	if err != nil {
		return nil, err
	}
	if a.Role != v1.RoleAdmin {
		return nil, v1.Newf(v1.CodeBadRequest, "%s 不是管理员账号；普通用户的密码由管理员在用户管理里重置", username)
	}
	return a, nil
}

// ResetAdminPassword 应急重置管理员密码：校验新密码、写入 bcrypt 哈希、作废该账号全部会话，保留两步验证。
func (s *Service) ResetAdminPassword(ctx context.Context, username, newPassword string) (int, error) {
	a, err := s.LookupAdmin(ctx, username)
	if err != nil {
		return 0, err
	}
	if err := ValidatePassword(newPassword); err != nil {
		return 0, err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return 0, err
	}
	if err := s.users.SetPasswordHash(ctx, a.ID, hash); err != nil {
		return 0, err
	}
	n, err := s.sessions.DeleteByUser(ctx, username, "")
	if err != nil {
		return 0, err
	}
	s.notifySessionRevoked("", username, "")
	return n, nil
}

// LookupAdmin 对已打开的数据库执行管理员查找。
func LookupAdmin(ctx context.Context, bdb *bun.DB, username string) (*users.Account, error) {
	return NewForDB(bdb).LookupAdmin(ctx, username)
}

// ResetAdminPassword 对已打开的数据库执行管理员应急密码重置，返回作废的会话数。
func ResetAdminPassword(ctx context.Context, bdb *bun.DB, username, newPassword string) (int, error) {
	return NewForDB(bdb).ResetAdminPassword(ctx, username, newPassword)
}
