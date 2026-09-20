package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// resetPasswordOutput 是 admin reset-password 的输出。
type resetPasswordOutput struct {
	Username        string `json:"username"`
	SessionsRevoked int    `json:"sessions_revoked"`
}

// adminResetPassword 是 satchel admin reset-password <username>（master-accounts）：本地命令，直接开数据目录里的库，
// 主控在不在跑都行；只对管理员账号；--confirm 必须等于用户名；新密码从终端读两遍；作废该账号全部会话；不动两步验证。
// 不经主控、不进审计，stderr 提示这一点。
func adminResetPassword(ctx context.Context, inv *command.Invocation) (any, error) {
	username := inv.Arg(0)
	if inv.String("confirm", "") != username {
		return nil, v1.Newf(v1.CodeConfirmRequired, "重置密码是不可逆的应急操作，要用 --confirm 填用户名确认").
			WithState("expected", username).WithNext("重新执行并加上 --confirm " + username)
	}
	bdb, err := OpenForWrite(ctx, DataDir(ctx))
	if err != nil {
		return nil, err
	}
	defer bdb.Close()
	// 先确认账号存在且是管理员，再让人输密码（免得输完才说没有这个账号）。
	if _, err := lookupAdmin(ctx, bdb, username); err != nil {
		return nil, err
	}
	first, err := promptFrom(ctx)("新密码")
	if err != nil {
		return nil, noTerminalLocal(err)
	}
	second, err := promptFrom(ctx)("再输入一次确认")
	if err != nil {
		return nil, noTerminalLocal(err)
	}
	if first != second {
		return nil, v1.New(v1.CodeBadRequest, "两次输入的新密码不一致")
	}
	revoked, err := ResetAdminPassword(ctx, bdb, username, first)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(stderrOf(ctx), "提示：admin reset-password 不经主控执行，此操作未进审计。")
	return resetPasswordOutput{Username: username, SessionsRevoked: revoked}, nil
}

// lookupAdmin 找到一个管理员账号：不存在或已删除是 not_found，不是管理员是 bad_request。
func lookupAdmin(ctx context.Context, bdb *bun.DB, username string) (*users.Account, error) {
	repo := users.New(bdb, store.New(bdb, schema.Default()))
	a, err := repo.GetByUsername(ctx, username)
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

// ResetAdminPassword 是 admin reset-password 的本体：校验新密码、写入 bcrypt 哈希、作废该账号的全部会话；不动两步验证。
// 返回作废的会话数。装配根的端到端测试直接对已打开的库调它。
func ResetAdminPassword(ctx context.Context, bdb *bun.DB, username, newPassword string) (int, error) {
	a, err := lookupAdmin(ctx, bdb, username)
	if err != nil {
		return 0, err
	}
	if err := auth.ValidatePassword(newPassword); err != nil {
		return 0, err
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return 0, err
	}
	repo := users.New(bdb, store.New(bdb, schema.Default()))
	if err := repo.SetPasswordHash(ctx, a.ID, hash); err != nil {
		return 0, err
	}
	return sessions.New(bdb).DeleteByUser(ctx, username, "")
}

func noTerminalLocal(err error) error {
	if errors.Is(err, ErrNoTerminal) {
		return v1.Wrap(v1.CodeBadRequest, "admin reset-password 要从终端读新密码，当前没有终端", err).WithNext("在主控本机有终端的会话里执行")
	}
	return v1.Wrap(v1.CodeInternal, "从终端读取失败", err)
}

type promptKey struct{}

// withPrompt 把本次执行的终端读取函数放进 ctx；本地命令（admin reset-password）用它读密码，测试里能换成假的。
func withPrompt(ctx context.Context, p func(string) (string, error)) context.Context {
	return context.WithValue(ctx, promptKey{}, p)
}

func promptFrom(ctx context.Context) func(string) (string, error) {
	if p, ok := ctx.Value(promptKey{}).(func(string) (string, error)); ok && p != nil {
		return p
	}
	return terminalPrompt
}

type stderrKey struct{}

// withStderr 把本次执行的 stderr 放进 ctx，本地命令的提示写到它（测试里能截获）。
func withStderr(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, stderrKey{}, w)
}

func stderrOf(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(stderrKey{}).(io.Writer); ok {
		return w
	}
	return os.Stderr
}
