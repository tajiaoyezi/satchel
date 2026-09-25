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

// TokenResolver 把 API 令牌解析成身份（service/tokens 实现）：返回身份与是否有效；查库出错也算无效（由实现记日志），
// 但另外交回 error，authn 不把这种失败计成一次令牌校验失败。
type TokenResolver interface {
	ResolveToken(ctx context.Context, token string) (v1.Identity, bool, error)
}

// ProbeRecorder 记一次令牌校验失败（service/security 实现，master-login-protection「令牌猜测的计数与自动封禁」）：
// 按来源 IP 计数，达到上限自动封禁。authn 只在请求不经 unix socket 时调它。
type ProbeRecorder interface {
	RecordProbe(ctx context.Context, ip, path string)
}

type peerKey struct{}

type socketKey struct{}

// peer 是连接对端的凭据：只有 unix socket 才有。
type peer struct {
	uid uint32
}

// ConnContext 给 http.Server.ConnContext 用：连接来自 unix socket 时先打上「经 socket」的标记（取不取得到对端 uid 都打，
// 门据此放行，master-access-gates），再取对端 uid 放进连接的 ctx，该连接上的每个请求都能从 r.Context() 拿到它们。
// TCP 连接原样返回。
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	ctx = MarkSocket(ctx)
	uid, ok := peerUID(uc)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, peerKey{}, peer{uid: uid})
}

// MarkSocket 给 ctx 打上「经数据目录下的 unix socket 进来」的标记（ConnContext 用，测试也用它造 socket 来的请求）。
func MarkSocket(ctx context.Context) context.Context {
	return context.WithValue(ctx, socketKey{}, true)
}

// OverSocket 报告请求是不是经 unix socket 进来的。判断靠建连接时打的标记，不靠 RemoteAddr（unix 连接上它是空串或 @）。
func OverSocket(ctx context.Context) bool {
	b, _ := ctx.Value(socketKey{}).(bool)
	return b
}

// Middleware 判定身份并放进 ctx。请求带 Authorization 头就只走令牌这一条路：是 Bearer 且 tokens 认得 → 令牌身份，
// 来源 token；其余情形（不是 Bearer、令牌无效、tokens 为 nil）→ anonymous、来源 SourceInvalid，不再看 socket 与 cookie
// （本机进程配了令牌就只有令牌的权限；无效凭据由 authz 拒绝）。没有这个头时：对端 uid 为 0 或等于本进程 uid → 本机管理员
// （cookie 不看）；否则有会话 cookie 且 sessions 认 → 用户（会话哈希与来源一并进 ctx）；否则 anonymous。
// 它不拒绝任何请求——拒绝由 authz 按命令表做，无身份的入口（healthz、/public/）本来就不看身份。判定出无效凭据、
// 且请求不经 unix socket 时，按门放进 ctx 的来源 IP 记一次令牌校验失败（probes 为 nil 时不记）。
// sessions 为 nil 时不认会话。
func Middleware(sessions SessionResolver, tokens TokenResolver, probes ProbeRecorder, next http.Handler) http.Handler {
	selfUID := uint32(os.Getuid())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := v1.Anonymous()
		if values := r.Header.Values("Authorization"); len(values) > 0 {
			ctx = v1.WithCredentialSource(ctx, v1.SourceInvalid)
			lookupFailed := false
			if tok, ok := bearer(values); ok && tokens != nil {
				tid, ok, err := tokens.ResolveToken(ctx, tok)
				if ok {
					id = tid
					ctx = v1.WithCredentialSource(ctx, v1.SourceToken)
				}
				lookupFailed = err != nil
			}
			// 查库出错不是猜令牌：照样按无效凭据拒绝这次请求，但不计数，免得库的故障把正常重试的来源 IP 封掉。
			if v1.CredentialSourceFrom(ctx) == v1.SourceInvalid && !lookupFailed && probes != nil && !OverSocket(ctx) {
				probes.RecordProbe(ctx, v1.RemoteFrom(ctx).IP, r.URL.Path)
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
