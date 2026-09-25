package gate

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// resolve 算出一个经 TCP 进来的请求从哪来（master-access-gates「来源 IP 与反代登记」「本机与经 HTTPS 到达」）：
// 来源 IP 默认是 TCP 对端地址，请求头一律不看；只有对端落在反代登记的某一项里，才按那一项的头取客户端地址，
// 取不到合法地址就退回对端地址。经登记的反代进来的请求不算本机（不论头里是什么）；X-Forwarded-Proto 只在对端在登记里时才看。
func resolve(r *http.Request, proxies []settings.TrustedProxy) v1.Remote {
	rem := v1.Remote{HTTPS: r.TLS != nil}
	peer, ok := peerAddr(r.RemoteAddr)
	if !ok {
		return rem
	}
	rem.IP = peer.String()
	header, trusted := matchProxy(peer, proxies)
	if !trusted {
		rem.Local = peer.IsLoopback()
		return rem
	}
	if ip, ok := clientFromHeader(r, header, proxies); ok {
		rem.IP = ip.String()
	}
	if strings.EqualFold(lastForwardedProto(r), "https") {
		rem.HTTPS = true
	}
	return rem
}

// lastForwardedProto 取 X-Forwarded-Proto 的最后一个值（多个头连起来按逗号拆）：那是离主控最近的、登记过的反代写的；
// 左边的值可能是客户端自己带来的，取它的话客户端写一个 http 就能让会话 cookie 不带 Secure。
func lastForwardedProto(r *http.Request) string {
	var vals []string
	for _, v := range r.Header.Values("X-Forwarded-Proto") {
		vals = append(vals, strings.Split(v, ",")...)
	}
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(vals[len(vals)-1])
}

// peerAddr 从 RemoteAddr（host:port）里取对端地址，IPv4 映射的 IPv6 按 IPv4。
func peerAddr(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return parseAddr(host)
}

// parseAddr 解析一个 IP 地址（去掉首尾空白，IPv4 映射的 IPv6 按 IPv4；带 zone 的不收）。
func parseAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// matchProxy 找对端落在的第一项登记，返回那一项信的头。
func matchProxy(a netip.Addr, proxies []settings.TrustedProxy) (string, bool) {
	for _, p := range proxies {
		if p.Prefix.Contains(a) {
			return p.Header, true
		}
	}
	return "", false
}

// clientFromHeader 按登记的头取客户端地址。X-Real-IP 与 CF-Connecting-IP 取整个值作为一个地址；X-Forwarded-For 把所有同名头
// 按逗号拆开，从最右边往左跳过落在任何一项登记里的地址，取第一个不在登记里的（左边是客户端自己能随便写的，不能取最左边）。
// 值里带端口（1.2.3.4:5678、[2001:db8::1]:443）的也认。
func clientFromHeader(r *http.Request, header string, proxies []settings.TrustedProxy) (netip.Addr, bool) {
	if header != settings.HeaderXForwardedFor {
		return hopAddr(r.Header.Get(header))
	}
	var hops []string
	for _, v := range r.Header.Values(header) {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := hopAddr(hops[i])
		if !ok {
			return netip.Addr{}, false
		}
		if _, isProxy := matchProxy(a, proxies); isProxy {
			continue
		}
		return a, true
	}
	return netip.Addr{}, false
}

func hopAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, ok := parseAddr(s); ok {
		return a, true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return parseAddr(host)
	}
	return netip.Addr{}, false
}
