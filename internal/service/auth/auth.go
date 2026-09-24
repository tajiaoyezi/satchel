// Package auth 是业务层的身份：网页登录与会话（master-web-session）、两步验证与恢复码（master-two-factor）、
// 第 05 章七组的当场验证（master-human-verification）、初始化向导与账号命令（master-setup-wizard、master-accounts）。
package auth

import (
	"context"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 会话与票据的时长（照 mmwx）。
const (
	SessionTTL      = 24 * time.Hour
	RememberMeTTL   = 30 * 24 * time.Hour
	PendingTTL      = 5 * time.Minute
	totpReplayGrace = 90 * time.Second
	// Issuer 是 TOTP otpauth URL 里的发行方。
	Issuer = "Satchel"
)

// Service 持有用户与会话两个仓储，加两张只在本进程里有意义的内存表：登录第二步的 pending 票据与 TOTP 防重放。
type Service struct {
	users    *users.Repo
	sessions *sessions.Repo
	now      func() time.Time

	mu       sync.Mutex
	pending  map[string]pendingEntry
	usedTOTP map[string]time.Time
}

type pendingEntry struct {
	username   string
	rememberMe bool
	expires    time.Time
}

// New 建服务。
func New(u *users.Repo, s *sessions.Repo) *Service {
	return &Service{users: u, sessions: s, now: func() time.Time { return time.Now().UTC() }, pending: map[string]pendingEntry{}, usedTOTP: map[string]time.Time{}}
}

// identityFor 把账号变成身份对象：管理员全部 scope 与六个危险类，普通用户 read + operate、不带危险类。
func identityFor(a *users.Account) v1.Identity {
	id := v1.Identity{Actor: a.Username, ActorKind: v1.ActorUser, Role: a.Role, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{}}
	if a.Role == v1.RoleAdmin {
		id.Scopes = append([]v1.Scope{}, v1.AllScopes...)
		id.Danger = append([]v1.Danger{}, v1.AllDangers...)
	}
	return id
}

type sessionHashKey struct{}

// WithSessionHash 把当前请求的会话哈希放进 ctx（authn 写，账号命令读：改密码时保留当前会话）。
func WithSessionHash(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, sessionHashKey{}, hash)
}

// SessionHashFrom 取 ctx 里的会话哈希；没有为空串。
func SessionHashFrom(ctx context.Context) string {
	h, _ := ctx.Value(sessionHashKey{}).(string)
	return h
}

// currentAccount 取调用者自己的账号：身份是 user 时是该用户，是 token 时是令牌的签发者（actor 就是签发者用户名）；
// local_admin 不是账号，给指引。
func (s *Service) currentAccount(ctx context.Context) (*users.Account, error) {
	id := v1.IdentityFrom(ctx)
	switch id.ActorKind {
	case v1.ActorUser, v1.ActorToken:
		a, err := s.users.GetByUsername(ctx, id.Actor)
		if err != nil {
			return nil, v1.Wrap(v1.CodeUnauthenticated, "调用者对应的账号已不存在", err)
		}
		return a, nil
	case v1.ActorLocalAdmin:
		return nil, v1.New(v1.CodeBadRequest, "本机管理员不是账号，account 命令只作用在登录的账号上").
			WithNext("以账号登录后再执行；忘了管理员密码用 satchel admin reset-password <用户名> --confirm <用户名>")
	}
	return nil, v1.Newf(v1.CodeBadRequest, "身份 %s 没有账号", id.ActorKind)
}
