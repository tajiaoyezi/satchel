package gate

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func proxy(cidr, header string) settings.TrustedProxy {
	return settings.TrustedProxy{Prefix: netip.MustParsePrefix(cidr), Header: header}
}

func request(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// master-access-gates「来源 IP 与反代登记」。
func TestSourceIP(t *testing.T) {
	loop := []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderCFConnectingIP)}
	xff := []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderXForwardedFor), proxy("10.0.0.0/8", settings.HeaderXForwardedFor)}
	realIP := []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderXRealIP)}
	cases := []struct {
		name    string
		remote  string
		headers map[string]string
		proxies []settings.TrustedProxy
		want    string
	}{
		{"没登记反代时不信请求头", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "198.51.100.7", "CF-Connecting-IP": "198.51.100.8"}, nil, "127.0.0.1"},
		{"对端不在登记里也不信", "203.0.113.5:5000", map[string]string{"CF-Connecting-IP": "198.51.100.8"}, loop, "203.0.113.5"},
		{"登记了就信登记的那个头", "127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "198.51.100.8", "X-Forwarded-For": "198.51.100.7"}, loop, "198.51.100.8"},
		{"XFF 跳过登记过的地址", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "198.51.100.7, 10.1.2.3"}, xff, "198.51.100.7"},
		{"XFF 左边伪造的不采用", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.66, 198.51.100.7"}, xff, "198.51.100.7"},
		{"XFF 全是登记的地址退回对端", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "10.0.0.1, 127.0.0.1"}, xff, "127.0.0.1"},
		{"XFF 带端口", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "[2001:db8::7]:443"}, xff, "2001:db8::7"},
		{"XFF 里不是地址退回对端", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "not-an-ip"}, xff, "127.0.0.1"},
		{"头缺失退回对端", "127.0.0.1:5000", nil, realIP, "127.0.0.1"},
		{"头不是地址退回对端", "127.0.0.1:5000", map[string]string{"X-Real-IP": "not-an-ip"}, realIP, "127.0.0.1"},
		{"IPv4 映射的 IPv6 对端按 IPv4", "[::ffff:198.51.100.9]:5000", nil, nil, "198.51.100.9"},
		{"IPv6 对端", "[2001:db8::1]:5000", nil, nil, "2001:db8::1"},
		{"IPv4 映射的头按 IPv4", "127.0.0.1:5000", map[string]string{"X-Real-IP": "::ffff:198.51.100.10"}, realIP, "198.51.100.10"},
	}
	for _, tc := range cases {
		if got := resolve(request(tc.remote, tc.headers), tc.proxies).IP; got != tc.want {
			t.Errorf("%s：来源 IP 应当是 %s，得到 %s", tc.name, tc.want, got)
		}
	}
	// 多个 X-Forwarded-For 头连起来看。
	r := request("127.0.0.1:5000", nil)
	r.Header.Add("X-Forwarded-For", "203.0.113.66")
	r.Header.Add("X-Forwarded-For", "198.51.100.7, 10.9.9.9")
	if got := resolve(r, xff).IP; got != "198.51.100.7" {
		t.Errorf("多个 XFF 头应当连起来从右往左取，得到 %s", got)
	}
}

// master-access-gates「本机与经 HTTPS 到达」。
func TestLocalAndHTTPS(t *testing.T) {
	loop := []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderXForwardedFor)}
	cases := []struct {
		name    string
		remote  string
		headers map[string]string
		proxies []settings.TrustedProxy
		tls     bool
		want    v1.Remote
	}{
		{"回环对端算本机", "127.0.0.1:5000", nil, nil, false, v1.Remote{IP: "127.0.0.1", Local: true}},
		{"IPv6 回环也算", "[::1]:5000", nil, nil, false, v1.Remote{IP: "::1", Local: true}},
		{"经登记的反代不算本机", "127.0.0.1:5000", nil, loop, false, v1.Remote{IP: "127.0.0.1"}},
		{"经登记的反代、头里是回环也不算", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "127.0.0.2"}, []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderXForwardedFor)}, false, v1.Remote{IP: "127.0.0.2"}},
		{"公网对端不算本机", "198.51.100.7:5000", nil, nil, false, v1.Remote{IP: "198.51.100.7"}},
		{"TLS 直连是 HTTPS", "198.51.100.7:5000", nil, nil, true, v1.Remote{IP: "198.51.100.7", HTTPS: true}},
		{"登记的反代标了 https", "127.0.0.1:5000", map[string]string{"X-Forwarded-Proto": "HTTPS"}, loop, false, v1.Remote{IP: "127.0.0.1", HTTPS: true}},
		{"取最后一个值：客户端写在左边的 http 不算", "127.0.0.1:5000", map[string]string{"X-Forwarded-Proto": "http, https"}, loop, false, v1.Remote{IP: "127.0.0.1", HTTPS: true}},
		{"取最后一个值：最近的反代标了 http", "127.0.0.1:5000", map[string]string{"X-Forwarded-Proto": "https, http"}, loop, false, v1.Remote{IP: "127.0.0.1"}},
		{"登记的反代标了 http", "127.0.0.1:5000", map[string]string{"X-Forwarded-Proto": "http"}, loop, false, v1.Remote{IP: "127.0.0.1"}},
		{"没登记的对端标 https 不算", "127.0.0.1:5000", map[string]string{"X-Forwarded-Proto": "https"}, nil, false, v1.Remote{IP: "127.0.0.1", Local: true}},
	}
	for _, tc := range cases {
		r := request(tc.remote, tc.headers)
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := resolve(r, tc.proxies); got != tc.want {
			t.Errorf("%s：应当是 %+v，得到 %+v", tc.name, tc.want, got)
		}
	}
}
