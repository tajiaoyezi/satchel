package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"
)

// Gates 是门这一组的设置（第 05 章七组的「门」），加上关闭公网访问要用的主控地址：门、登录限流、令牌猜测的封禁与
// Turnstile 都从这一份取值（master-access-gates、master-login-protection）。分钟数已换成时长。
type Gates struct {
	MasterLocalOnly         bool
	SilentMode              bool
	SilentModeTimeout       time.Duration
	ProbeDisguiseBlockLogin bool
	BruteForceEnabled       bool
	BruteForceMaxFailures   int
	BruteForceWindow        time.Duration
	BruteForceBlock         time.Duration
	LoginRateMaxAttempts    int
	LoginRateWindow         time.Duration
	LoginRateLock           time.Duration
	SkipLocalIP             bool
	TurnstileSiteKey        string
	TurnstileSecretKey      string
	TrustedProxies          []TrustedProxy
	MasterURL               string
}

// TrustedProxy 是反代登记的一项：TCP 对端落在 Prefix 里的请求，按 Header 取客户端地址（第 06 章）。
type TrustedProxy struct {
	Prefix netip.Prefix
	Header string
}

// 反代登记能信的三个请求头（规范写法）。
const (
	HeaderXForwardedFor  = "X-Forwarded-For"
	HeaderXRealIP        = "X-Real-IP"
	HeaderCFConnectingIP = "CF-Connecting-IP"
)

// MaxTrustedProxies 是反代登记的上限：Cloudflare 公布的边缘网段是二十来个，够用。
const MaxTrustedProxies = 64

var proxyHeaders = map[string]string{
	strings.ToLower(HeaderXForwardedFor):  HeaderXForwardedFor,
	strings.ToLower(HeaderXRealIP):        HeaderXRealIP,
	strings.ToLower(HeaderCFConnectingIP): HeaderCFConnectingIP,
}

// ParseTrustedProxies 按 master-settings 的形状规则解析 trusted_proxies：JSON 数组、至多 64 项，每项是恰好含 cidr 与
// header 两个键的对象，cidr 是一个 IP 地址或网段，header 是三个头名之一（不分大小写，解析后是规范写法）。
// 写入时由字段规则调它拒绝坏值；错误文案接在字段名后面读。
func ParseTrustedProxies(raw []byte) ([]TrustedProxy, error) {
	raw = bytes.TrimSpace(raw)
	var items []json.RawMessage
	// json.Unmarshal 会把 null 解成空切片，所以先看第一个字符。
	if !bytes.HasPrefix(raw, []byte("[")) || json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("必须是 JSON 数组")
	}
	if len(items) > MaxTrustedProxies {
		return nil, fmt.Errorf("至多 %d 项，得到 %d 项", MaxTrustedProxies, len(items))
	}
	out := make([]TrustedProxy, 0, len(items))
	for i, item := range items {
		var entry map[string]json.RawMessage
		item = bytes.TrimSpace(item)
		if !bytes.HasPrefix(item, []byte("{")) || json.Unmarshal(item, &entry) != nil || len(entry) != 2 {
			return nil, fmt.Errorf("第 %d 项必须是恰好含 cidr 与 header 两个键的对象", i+1)
		}
		var cidr, header string
		if json.Unmarshal(entry["cidr"], &cidr) != nil || json.Unmarshal(entry["header"], &header) != nil {
			return nil, fmt.Errorf("第 %d 项必须是恰好含 cidr 与 header 两个键的对象，两个值都是字符串", i+1)
		}
		prefix, ok := parsePrefix(cidr)
		if !ok {
			return nil, fmt.Errorf("第 %d 项的 cidr 不是 IP 地址或网段，得到 %q", i+1, cidr)
		}
		canonical, ok := proxyHeaders[strings.ToLower(strings.TrimSpace(header))]
		if !ok {
			return nil, fmt.Errorf("第 %d 项的 header 只能是 %s、%s、%s 之一，得到 %q", i+1, HeaderXForwardedFor, HeaderXRealIP, HeaderCFConnectingIP, header)
		}
		out = append(out, TrustedProxy{Prefix: prefix, Header: canonical})
	}
	return out, nil
}

// parsePrefix 收一个 IP 地址（当成单个地址的网段）或网段；IPv4 映射的 IPv6 地址按 IPv4 记，带 zone 的不收。
func parsePrefix(s string) (netip.Prefix, bool) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return netip.Prefix{}, false
		}
		return p.Masked(), true
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, false
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), true
}

// Gates 取出门这一组的设置。库里的值在写入时已过规则；万一是坏的（只可能是导入或手改），数值按默认值取、
// trusted_proxies 按「没有登记」取（不信任何请求头），并记一条 warn。
func (st *State) Gates() Gates {
	g := Gates{
		MasterLocalOnly:         st.boolean("master_local_only"),
		SilentMode:              st.boolean("silent_mode"),
		SilentModeTimeout:       st.minutes("silent_mode_timeout"),
		ProbeDisguiseBlockLogin: st.boolean("probe_disguise_block_login"),
		BruteForceEnabled:       st.boolean("brute_force_enabled"),
		BruteForceMaxFailures:   st.count("brute_force_max_failures"),
		BruteForceWindow:        st.minutes("brute_force_window_minutes"),
		BruteForceBlock:         st.minutes("brute_force_block_minutes"),
		LoginRateMaxAttempts:    st.count("login_rate_max_attempts"),
		LoginRateWindow:         st.minutes("login_rate_window_minutes"),
		LoginRateLock:           st.minutes("login_rate_lock_minutes"),
		SkipLocalIP:             st.boolean("skip_local_ip"),
		TurnstileSiteKey:        st.text("turnstile_site_key"),
		TurnstileSecretKey:      st.text("turnstile_secret_key"),
		MasterURL:               st.text("master_url"),
	}
	raw, _ := st.Values["trusted_proxies"].(json.RawMessage)
	proxies, err := ParseTrustedProxies(raw)
	if err != nil {
		slog.Warn("系统设置里的 trusted_proxies 不合形状，按没有登记反向代理处理", "error", err.Error())
		proxies = nil
	}
	g.TrustedProxies = proxies
	return g
}

func (st *State) boolean(name string) bool {
	b, _ := st.Values[name].(bool)
	return b
}

func (st *State) text(name string) string {
	s, _ := st.Values[name].(string)
	return s
}

// silentModeTimeoutDefault 是 system_config.silent_mode_timeout 列的库默认值（tables_ops.go；mmwx 把 <=0 改回 15）。
// 列的默认值在 DDL 里、不在默认值表里，门只用到这一列，单独记一个。
const silentModeTimeoutDefault = 15

// count 取一个至少为 1 的整数；不是正数时按默认值取（区间规则在写入时已挡过，这里只兜住导入或手改的坏值）。
func (st *State) count(name string) int {
	if n, ok := st.Values[name].(int64); ok && n >= 1 {
		return int(n)
	}
	if d, ok := DefaultOf(name); ok {
		if n, ok := d.(int64); ok {
			return int(n)
		}
	}
	return silentModeTimeoutDefault
}

// minutes 取一个分钟数并换成时长；不是正数时按默认值取。
func (st *State) minutes(name string) time.Duration {
	return time.Duration(st.count(name)) * time.Minute
}
