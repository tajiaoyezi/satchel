// Package authn 是横切层的身份：判定「谁在调用」并把身份对象放进 ctx（master-identity-and-authz「身份的判定顺序」）。
// 按顺序取第一个：带 Authorization 头 → 只看令牌（有效是令牌身份，否则是无效凭据）；经数据目录 unix socket 连进来、
// 对端 uid 是 0 或运行主控的 OS 用户 → 本机管理员；带有效会话 cookie → 用户（master-web-session）；其余 → anonymous。
// 判定只信操作系统给的对端凭据、库里的会话与令牌，不信请求头或请求体里的任何自称。
package authn

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// SessionCookie 是会话 cookie 的名字。
const SessionCookie = "satchel_session"

// SessionResolver 把会话令牌解析成身份（service/auth 实现）：返回身份、会话哈希、是否有效。
type SessionResolver interface {
	Resolve(ctx context.Context, token string) (v1.Identity, string, bool, error)
}

// TokenResolver 把 API 令牌解析成身份（service/tokens 实现）：返回身份与是否有效；查库出错也算无效，由实现记日志。
type TokenResolver interface {
	Resolve(ctx context.Context, token string) (v1.Identity, bool)
}

type peerKey struct{}

// peer 是连接对端的凭据：只有 unix socket 才有。
type peer struct {
	uid uint32
}

// ConnContext 给 http.Server.ConnContext 用：连接来自 unix socket 时取对端 uid 放进连接的 ctx，
// 该连接上的每个请求都能从 r.Context() 拿到它。TCP 连接原样返回。
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	uid, ok := peerUID(uc)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, peerKey{}, peer{uid: uid})
}

// Middleware 判定身份并放进 ctx。请求带 Authorization 头就只走令牌这一条路：是 Bearer 且 tokens 认得 → 令牌身份，
// 来源 token；其余情形（不是 Bearer、令牌无效、tokens 为 nil）→ anonymous、来源 SourceInvalid，不再看 socket 与 cookie
// （本机进程配了令牌就只有令牌的权限；无效凭据由 authz 拒绝）。没有这个头时：对端 uid 为 0 或等于本进程 uid → 本机管理员
// （cookie 不看）；否则有会话 cookie 且 sessions 认 → 用户（会话哈希与来源一并进 ctx）；否则 anonymous。
// 它不拒绝任何请求——拒绝由 authz 按命令表做，无身份的入口（healthz、/public/）本来就不看身份。
// sessions 为 nil 时不认会话。
func Middleware(sessions SessionResolver, tokens TokenResolver, next http.Handler) http.Handler {
	selfUID := uint32(os.Getuid())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := v1.Anonymous()
		if values := r.Header.Values("Authorization"); len(values) > 0 {
			ctx = v1.WithCredentialSource(ctx, v1.SourceInvalid)
			if tok, ok := bearer(values); ok && tokens != nil {
				if tid, ok := tokens.Resolve(ctx, tok); ok {
					id = tid
					ctx = v1.WithCredentialSource(ctx, v1.SourceToken)
				}
			}
		} else if p, ok := ctx.Value(peerKey{}).(peer); ok && (p.uid == 0 || p.uid == selfUID) {
			id = v1.LocalAdmin(actorName(p.uid))
			ctx = v1.WithCredentialSource(ctx, v1.SourceSocket)
		} else if sessions != nil {
			if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
				if sid, hash, ok, rerr := sessions.Resolve(ctx, c.Value); rerr == nil && ok {
					id = sid
					ctx = v1.WithCredentialSource(auth.WithSessionHash(ctx, hash), v1.SourceSession)
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(v1.WithIdentity(ctx, id)))
	})
}

// bearer 从 Authorization 头里取令牌：只有一个头、方案是 Bearer（不分大小写）、令牌非空且不含空白才算。
func bearer(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(strings.TrimSpace(values[0]), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", false
	}
	return tok, true
}

// actorName 把 uid 换成用户名；查不到就写 uid:<n>。
func actorName(uid uint32) string {
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid:" + strconv.FormatUint(uint64(uid), 10)
}
