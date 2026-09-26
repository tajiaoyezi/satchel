package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	coresecurity "github.com/satchel/satchel/internal/core/security"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// LoginResult 是登录（或两步验证第二步）的结果。Token 只给投影层放进 cookie，不进 JSON。
type LoginResult struct {
	Username               string    `json:"username,omitempty"`
	Role                   v1.Role   `json:"role,omitempty"`
	ExpiresAt              time.Time `json:"expires_at,omitzero"`
	RecoveryCodesRemaining *int      `json:"recovery_codes_remaining,omitempty"`
	RecoveryCodesLow       bool      `json:"recovery_codes_low,omitempty"`
	TwoFactorRequired      bool      `json:"two_factor_required,omitempty"`
	Pending                string    `json:"pending,omitempty"`
	Token                  string    `json:"-"`
}

func errBadCredentials() *v1.Error {
	return v1.New(v1.CodeUnauthenticated, "用户名或密码不对").WithNext("检查后重试；忘了管理员密码在主控本机执行 satchel admin reset-password")
}

// Login 用密码登录：账号不存在、密码不对、已删除都是同一条 unauthenticated；停用是 forbidden；
// 开了两步验证时不发会话，发一个 5 分钟的 pending 票据让客户端走第二步。
// 次序是查登录限流并先占上这一次（锁定期内是 rate_limited）→ 核对验证码（Turnstile 启用时）→ 比对密码；比对不上计一次失败，
// 账号已停用时即使密码正确也计（否则能不受限地试出停用账号的密码）；没开两步验证时成功清零，开了两步验证时密码对了
// 既不计也不清零，第二步通过才清零（master-login-protection）。
func (s *Service) Login(ctx context.Context, username, password string, rememberMe bool, turnstileToken string) (*LoginResult, error) {
	username = strings.TrimSpace(username)
	if err := s.reserve(ctx, username); err != nil {
		return nil, err
	}
	if err := s.checkCaptcha(ctx, turnstileToken); err != nil {
		s.released(ctx, username)
		return nil, err
	}
	a, err := s.users.GetByUsername(ctx, username)
	if errors.Is(err, users.ErrNotFound) || (err == nil && a.Deleted) {
		// 账号不存在时也跑一次 bcrypt，让两种失败耗时接近。
		CheckPassword(dummyHash, password)
		s.failed(ctx, username, loginPath, coresecurity.KindLoginFail, coresecurity.KindLoginLocked)
		return nil, errBadCredentials()
	}
	if err != nil {
		s.released(ctx, username)
		return nil, err
	}
	if !CheckPassword(a.PasswordHash, password) {
		s.failed(ctx, username, loginPath, coresecurity.KindLoginFail, coresecurity.KindLoginLocked)
		return nil, errBadCredentials()
	}
	if !a.IsActive {
		s.failed(ctx, username, loginPath, coresecurity.KindLoginFail, coresecurity.KindLoginLocked)
		return nil, v1.New(v1.CodeForbidden, "账号已停用").WithNext("联系管理员启用")
	}
	if a.TOTPEnabled {
		s.released(ctx, username)
		pending, err := s.issuePending(a.Username, rememberMe)
		if err != nil {
			return nil, err
		}
		return &LoginResult{TwoFactorRequired: true, Pending: pending}, nil
	}
	s.succeeded(ctx, username)
	return s.issue(ctx, a, rememberMe)
}

// CompleteTwoFactor 用 pending 票据与第二因素（TOTP 或恢复码）完成登录；票据只能用一次。
// 比对之前按票据里的账号与本次来源 IP 查登录限流：锁定期内票据不作废，期满后还能拿它继续；没锁才作废票据、比对。
// 验证码不对计一次失败；通过才清零。
func (s *Service) CompleteTwoFactor(ctx context.Context, pending, code string) (*LoginResult, error) {
	peeked, ok := s.peekPending(pending)
	if !ok {
		return nil, v1.New(v1.CodeUnauthenticated, "两步验证的票据不存在或已过期").WithNext("重新登录")
	}
	if err := s.reserve(ctx, peeked.username); err != nil {
		return nil, err
	}
	entry, ok := s.consumePending(pending)
	if !ok { // 查锁定的这一会儿被别的请求用掉了
		s.released(ctx, peeked.username)
		return nil, v1.New(v1.CodeUnauthenticated, "两步验证的票据不存在或已过期").WithNext("重新登录")
	}
	a, err := s.users.GetByUsername(ctx, entry.username)
	if err != nil || a.Deleted || !a.IsActive || !a.TOTPEnabled {
		s.released(ctx, entry.username)
		return nil, v1.New(v1.CodeUnauthenticated, "账号状态已变化，请重新登录")
	}
	ok, err = s.checkSecondFactor(ctx, a, code)
	if err != nil {
		s.released(ctx, entry.username)
		return nil, err
	}
	if !ok {
		s.failed(ctx, entry.username, twoFactorPath, coresecurity.KindLoginFail, coresecurity.KindLoginLocked)
		return nil, v1.New(v1.CodeUnauthenticated, "验证码不对，或这枚恢复码已经用过").WithNext("重新登录后输入验证器当前的码，或一枚没用过的恢复码")
	}
	s.succeeded(ctx, entry.username)
	return s.issue(ctx, a, entry.rememberMe)
}

// issue 发一条会话：随机令牌，库里只存哈希。
func (s *Service) issue(ctx context.Context, a *users.Account, rememberMe bool) (*LoginResult, error) {
	token, expires, err := s.IssueSession(ctx, a.Username, rememberMe)
	if err != nil {
		return nil, err
	}
	res := &LoginResult{Username: a.Username, Role: a.Role, ExpiresAt: expires, Token: token}
	if a.TOTPEnabled {
		n := len(a.RecoveryCodes)
		res.RecoveryCodesRemaining = &n
		res.RecoveryCodesLow = recoveryLow(a)
	}
	return res, nil
}

// IssueSession 为一个账号发会话（登录与 setup init 都用），返回明文令牌与过期时间。
func (s *Service) IssueSession(ctx context.Context, username string, rememberMe bool) (string, time.Time, error) {
	token, err := randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	ttl := SessionTTL
	if rememberMe {
		ttl = RememberMeTTL
	}
	expires := s.now().Add(ttl)
	if err := s.sessions.Insert(ctx, sessions.Session{TokenHash: HashToken(token), Username: username, ExpiresAt: expires, CreatedAt: s.now()}); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// PruneSessions 删掉已过期的会话，返回删掉的条数（session_cleanup 任务每小时调一次；过期的会话本来就视同不存在）。
func (s *Service) PruneSessions(ctx context.Context) (int, error) {
	return s.sessions.DeleteExpired(ctx, s.now())
}

// Logout 删掉一条会话（按明文令牌）；没有也算成功。
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.sessions.Delete(ctx, HashToken(token))
}

// Resolve 把会话令牌解析成身份对象（authn 用）：查不到、过期、账号停用或已删除都返回 ok=false。
// 返回的第二个值是会话哈希，authn 放进 ctx 供账号命令用。
func (s *Service) Resolve(ctx context.Context, token string) (v1.Identity, string, bool, error) {
	if token == "" {
		return v1.Identity{}, "", false, nil
	}
	hash := HashToken(token)
	sess, err := s.sessions.GetByHash(ctx, hash)
	if errors.Is(err, sessions.ErrNotFound) {
		return v1.Identity{}, "", false, nil
	}
	if err != nil {
		return v1.Identity{}, "", false, err
	}
	if !sess.ExpiresAt.After(s.now()) {
		return v1.Identity{}, "", false, nil
	}
	a, err := s.users.GetByUsername(ctx, sess.Username)
	if errors.Is(err, users.ErrNotFound) {
		return v1.Identity{}, "", false, nil
	}
	if err != nil {
		return v1.Identity{}, "", false, err
	}
	if a.Deleted || !a.IsActive {
		return v1.Identity{}, "", false, nil
	}
	return identityFor(a), hash, true, nil
}

// HashToken 是会话令牌在库里的样子：SHA-256 十六进制。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", v1.Wrap(v1.CodeInternal, "生成随机令牌失败", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pending 票据：内存里、5 分钟、只能用一次、进程重启即失效。
func (s *Service) issuePending(username string, rememberMe bool) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.pending {
		if e.expires.Before(now) {
			delete(s.pending, k)
		}
	}
	s.pending[token] = pendingEntry{username: username, rememberMe: rememberMe, expires: now.Add(PendingTTL)}
	return token, nil
}

// peekPending 读票据但不作废（查登录限流用）；不存在或已过期返回 false。
func (s *Service) peekPending(token string) (pendingEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.pending[token]
	if !ok || e.expires.Before(s.now()) {
		return pendingEntry{}, false
	}
	return e, true
}

func (s *Service) consumePending(token string) (pendingEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.pending[token]
	if !ok {
		return pendingEntry{}, false
	}
	delete(s.pending, token)
	if e.expires.Before(s.now()) {
		return pendingEntry{}, false
	}
	return e, true
}
