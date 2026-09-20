package auth

import (
	"context"
	"strings"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Bindings 是本服务提供的命令处理函数，按命令名给 cmd/satchel 绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"setup status":                      s.setupStatus,
		"setup init":                        s.setupInit,
		"account show":                      s.accountShow,
		"account set-password":              s.accountSetPassword,
		"account totp setup":                s.totpSetup,
		"account totp confirm":              s.totpConfirm,
		"account totp disable":              s.totpDisable,
		"account recovery-codes regenerate": s.recoveryRegenerate,
	}
}

// SetupStatus 是 setup status 的输出。
type SetupStatus struct {
	Initialized bool                 `json:"initialized"`
	Paths       map[string]SetupPath `json:"paths"`
}

// SetupPath 是向导第一步的一条起步路径。
type SetupPath struct {
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

func (s *Service) setupStatus(ctx context.Context, _ *command.Invocation) (any, error) {
	n, err := s.users.Count(ctx)
	if err != nil {
		return nil, err
	}
	initialized := n > 0
	return SetupStatus{Initialized: initialized, Paths: map[string]SetupPath{
		"create_admin":   {Available: !initialized},
		"restore_backup": {Available: false, Note: "恢复 Satchel 备份随 m1-07 交付"},
		"import_mmwx":    {Available: false, Note: "导入 mmwx 备份随 M9 交付"},
	}}, nil
}

// SetupResult 是 setup init 的输出。
type SetupResult struct {
	Username string  `json:"username"`
	Role     v1.Role `json:"role"`
}

func (s *Service) setupInit(ctx context.Context, inv *command.Invocation) (any, error) {
	username := strings.TrimSpace(inv.String("username", ""))
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	password := inv.String("password", "")
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	a, err := s.users.InitAdmin(ctx, username, hash, strings.TrimSpace(inv.String("email", "")))
	if err != nil {
		return nil, err
	}
	return SetupResult{Username: a.Username, Role: a.Role}, nil
}

// AccountInfo 是 account show 的输出。
type AccountInfo struct {
	Username               string  `json:"username"`
	Role                   v1.Role `json:"role"`
	Email                  string  `json:"email"`
	Nickname               string  `json:"nickname"`
	TOTPEnabled            bool    `json:"totp_enabled"`
	TOTPPending            bool    `json:"totp_pending"`
	RecoveryCodesRemaining int     `json:"recovery_codes_remaining"`
	RecoveryCodesLow       bool    `json:"recovery_codes_low"`
	Sessions               int     `json:"sessions"`
}

func (s *Service) accountShow(ctx context.Context, _ *command.Invocation) (any, error) {
	a, err := s.currentAccount(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.sessions.CountByUser(ctx, a.Username, s.now())
	if err != nil {
		return nil, err
	}
	return AccountInfo{
		Username: a.Username, Role: a.Role, Email: a.Email, Nickname: a.Nickname,
		TOTPEnabled: a.TOTPEnabled, TOTPPending: a.TOTPSecret != "" && !a.TOTPEnabled,
		RecoveryCodesRemaining: len(a.RecoveryCodes), RecoveryCodesLow: recoveryLow(a), Sessions: n,
	}, nil
}

// PasswordChanged 是 account set-password 的输出。
type PasswordChanged struct {
	Username        string `json:"username"`
	SessionsRevoked int    `json:"sessions_revoked"`
}

func (s *Service) accountSetPassword(ctx context.Context, inv *command.Invocation) (any, error) {
	a, err := s.currentAccount(ctx)
	if err != nil {
		return nil, err
	}
	pw := inv.String("new-password", "")
	if err := ValidatePassword(pw); err != nil {
		return nil, err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return nil, err
	}
	if err := s.users.SetPasswordHash(ctx, a.ID, hash); err != nil {
		return nil, err
	}
	// 其它会话作废、当前会话保留。
	revoked, err := s.sessions.DeleteByUser(ctx, a.Username, SessionHashFrom(ctx))
	if err != nil {
		return nil, err
	}
	return PasswordChanged{Username: a.Username, SessionsRevoked: revoked}, nil
}

func (s *Service) totpSetup(ctx context.Context, _ *command.Invocation) (any, error) {
	a, err := s.currentAccount(ctx)
	if err != nil {
		return nil, err
	}
	key, err := newTOTPKey(a.Username)
	if err != nil {
		return nil, err
	}
	if err := s.users.SetPendingTOTP(ctx, a.ID, key.Secret); err != nil {
		return nil, err
	}
	return key, nil
}

// RecoveryCodes 是启用两步验证与重新生成恢复码的输出：明文只在这里出现一次。
type RecoveryCodes struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

func (s *Service) totpConfirm(ctx context.Context, inv *command.Invocation) (any, error) {
	a, err := s.currentAccount(ctx)
	if err != nil {
		return nil, err
	}
	if a.TOTPEnabled {
		return nil, v1.New(v1.CodeConflict, "两步验证已经启用").WithNext("要换密钥先 account totp disable 再 setup")
	}
	if a.TOTPSecret == "" {
		return nil, v1.New(v1.CodeConflict, "还没有生成两步验证密钥").WithNext("先执行 account totp setup")
	}
	code := strings.TrimSpace(inv.String("code", ""))
	if code == "" || !s.checkTOTP(a.Username, a.TOTPSecret, code) {
		return nil, v1.New(v1.CodeBadRequest, "验证码不对：用验证器扫 setup 给的二维码后输入它当前显示的码")
	}
	plain, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := s.users.EnableTOTP(ctx, a.ID, a.TOTPSecret, hashes); err != nil {
		return nil, err
	}
	return RecoveryCodes{RecoveryCodes: plain}, nil
}

// TOTPDisabled 是 account totp disable 的输出。
type TOTPDisabled struct {
	TOTPEnabled bool `json:"totp_enabled"`
}

func (s *Service) totpDisable(ctx context.Context, _ *command.Invocation) (any, error) {
	a, err := s.currentAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.users.DisableTOTP(ctx, a.ID); err != nil {
		return nil, err
	}
	return TOTPDisabled{TOTPEnabled: false}, nil
}

func (s *Service) recoveryRegenerate(ctx context.Context, _ *command.Invocation) (any, error) {
	a, err := s.currentAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !a.TOTPEnabled {
		return nil, v1.New(v1.CodeConflict, "两步验证没有启用，没有恢复码可以重新生成")
	}
	plain, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := s.users.SetRecoveryCodes(ctx, a.ID, hashes, nil); err != nil {
		return nil, err
	}
	return RecoveryCodes{RecoveryCodes: plain}, nil
}
