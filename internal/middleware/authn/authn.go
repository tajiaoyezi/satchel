// Package authn 是横切层的身份：判定「谁在调用」并把身份对象放进 ctx（master-identity-and-authz）。
// 本 change 只有两种身份：经数据目录 unix socket 连进来、对端 uid 是 0 或运行主控的 OS 用户 → 本机管理员；
// 其余（含一切 TCP 请求）→ anonymous。会话（m1-02）与令牌（m1-04）往同一个形状里填。
// 判定只信操作系统给的对端凭据，不信请求头或请求体里的任何自称。
package authn

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

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

// Middleware 按连接判定身份并放进 ctx：对端 uid 为 0 或等于本进程 uid → 本机管理员，否则 anonymous。
// 它不拒绝任何请求——拒绝由 authz 按命令表做，无身份的入口（healthz、/public/）本来就不看身份。
func Middleware(next http.Handler) http.Handler {
	selfUID := uint32(os.Getuid())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := v1.Anonymous()
		if p, ok := r.Context().Value(peerKey{}).(peer); ok && (p.uid == 0 || p.uid == selfUID) {
			id = v1.LocalAdmin(actorName(p.uid))
		}
		next.ServeHTTP(w, r.WithContext(v1.WithIdentity(r.Context(), id)))
	})
}

// actorName 把 uid 换成用户名；查不到就写 uid:<n>。
func actorName(uid uint32) string {
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid:" + strconv.FormatUint(uint64(uid), 10)
}
