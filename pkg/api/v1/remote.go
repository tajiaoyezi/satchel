package v1

import "context"

// Remote 是一个请求从哪来（master-access-gates「来源 IP 与反代登记」「本机与经 HTTPS 到达」）。
// 门在处理链的最外层算一次放进 ctx；登录限流、令牌猜测的计数、安全事件、会话 cookie 的 Secure 都从这里取，
// 不再各自看 RemoteAddr 或请求头。放在契约层是因为 service 层不能引用 middleware。
type Remote struct {
	// IP 是来源 IP 的规范形式（IPv4 映射的 IPv6 按 IPv4 写）；经 unix socket 进来时为空。
	IP string
	// Socket 表示请求经数据目录下的 unix socket 进来。
	Socket bool
	// Local 表示本机来的：经 unix socket，或 TCP 对端是回环地址且不在反代登记里。
	Local bool
	// HTTPS 表示经 HTTPS 到达：TCP 连接本身是 TLS，或对端在反代登记里且标了 X-Forwarded-Proto: https。
	HTTPS bool
}

type remoteKey struct{}

// WithRemote 把请求的来源放进 ctx。
func WithRemote(ctx context.Context, r Remote) context.Context {
	return context.WithValue(ctx, remoteKey{}, r)
}

// RemoteFrom 取 ctx 里的来源；没有就是零值（没有 IP、不是 socket、不是本机、不是 HTTPS）。
func RemoteFrom(ctx context.Context) Remote {
	if r, ok := ctx.Value(remoteKey{}).(Remote); ok {
		return r
	}
	return Remote{}
}
