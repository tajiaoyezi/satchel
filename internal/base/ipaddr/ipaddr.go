// Package ipaddr 是基础设施层的 IP 地址小工具：规范化，以及「本地或内网地址」的判定（skip_local_ip 的口径）。
// 登录限流、令牌猜测的封禁与门都用这一份，免得各处对同一个地址得出不同的结论。
package ipaddr

import "net/netip"

// Canonical 把一个 IP 地址写成规范形式：IPv4 映射的 IPv6（::ffff:1.2.3.4）按 IPv4 写，IPv6 小写压缩。
// 不是单个 IP 地址（网段、主机名、带 zone 的链路本地地址）时返回 ok=false。
func Canonical(s string) (string, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return "", false
	}
	return a.Unmap().String(), true
}

// IsLocalOrPrivate 报告地址是不是本地或内网地址：回环、私有网段（10/8、172.16/12、192.168/16、fc00::/7）、链路本地、未指定。
// 空串或解析不了的也算（照 mmwx：拿不准的地址宁可不计数、不封禁，免得把一整个反代后面的人一起封掉）。
func IsLocalOrPrivate(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return true
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified()
}
