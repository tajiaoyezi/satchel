package settings

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func stateWith(values map[string]any) *State {
	st := &State{Values: map[string]any{}}
	for name, v := range defaults {
		st.Values[name] = v
	}
	st.Values["silent_mode"] = false
	st.Values["silent_mode_timeout"] = int64(15)
	for name, v := range values {
		st.Values[name] = v
	}
	return st
}

// 默认值下的视图：照 mmwx 的默认参数，没有登记反代。
func TestGatesDefaults(t *testing.T) {
	g := stateWith(nil).Gates()
	want := Gates{
		SilentModeTimeout: 15 * time.Minute, BruteForceEnabled: true, BruteForceMaxFailures: 5, BruteForceWindow: 1440 * time.Minute,
		BruteForceBlock: 1440 * time.Minute, LoginRateMaxAttempts: 5, LoginRateWindow: time.Hour, LoginRateLock: time.Hour, SkipLocalIP: true,
		TrustedProxies: []TrustedProxy{},
	}
	if g.MasterLocalOnly || g.SilentMode || g.ProbeDisguiseBlockLogin || g.TurnstileSiteKey != "" || g.MasterURL != "" {
		t.Fatalf("门默认都不拦：%+v", g)
	}
	if g.SilentModeTimeout != want.SilentModeTimeout || g.BruteForceMaxFailures != 5 || g.BruteForceWindow != want.BruteForceWindow ||
		g.BruteForceBlock != want.BruteForceBlock || g.LoginRateMaxAttempts != 5 || g.LoginRateWindow != time.Hour || g.LoginRateLock != time.Hour ||
		!g.BruteForceEnabled || !g.SkipLocalIP || len(g.TrustedProxies) != 0 {
		t.Fatalf("默认参数不对：%+v", g)
	}
}

func TestGatesValues(t *testing.T) {
	g := stateWith(map[string]any{
		"master_local_only": true, "silent_mode": true, "silent_mode_timeout": int64(30), "probe_disguise_block_login": true,
		"brute_force_enabled": false, "brute_force_max_failures": int64(3), "brute_force_window_minutes": int64(10), "brute_force_block_minutes": int64(20),
		"login_rate_max_attempts": int64(7), "login_rate_window_minutes": int64(15), "login_rate_lock_minutes": int64(45), "skip_local_ip": false,
		"turnstile_site_key": "site", "turnstile_secret_key": "secret", "master_url": "https://panel.example.com",
		"trusted_proxies": json.RawMessage(`[{"cidr":"127.0.0.1","header":"cf-connecting-ip"},{"cidr":"10.1.2.3/8","header":"X-Forwarded-For"},{"cidr":"::ffff:192.0.2.1","header":"x-real-ip"}]`),
	}).Gates()
	if !g.MasterLocalOnly || !g.SilentMode || g.SilentModeTimeout != 30*time.Minute || !g.ProbeDisguiseBlockLogin || g.BruteForceEnabled ||
		g.BruteForceMaxFailures != 3 || g.BruteForceWindow != 10*time.Minute || g.BruteForceBlock != 20*time.Minute || g.LoginRateMaxAttempts != 7 ||
		g.LoginRateWindow != 15*time.Minute || g.LoginRateLock != 45*time.Minute || g.SkipLocalIP || g.TurnstileSiteKey != "site" ||
		g.TurnstileSecretKey != "secret" || g.MasterURL != "https://panel.example.com" {
		t.Fatalf("取值不对：%+v", g)
	}
	want := []TrustedProxy{
		{netip.MustParsePrefix("127.0.0.1/32"), HeaderCFConnectingIP},
		{netip.MustParsePrefix("10.0.0.0/8"), HeaderXForwardedFor},
		{netip.MustParsePrefix("192.0.2.1/32"), HeaderXRealIP},
	}
	if len(g.TrustedProxies) != len(want) {
		t.Fatalf("反代登记：%+v", g.TrustedProxies)
	}
	for i := range want {
		if g.TrustedProxies[i] != want[i] {
			t.Errorf("第 %d 项应当是 %+v，得到 %+v", i+1, want[i], g.TrustedProxies[i])
		}
	}
}

// 坏值（只可能是导入或手改）：数值按默认值、反代登记按没有。
func TestGatesBadStoredValues(t *testing.T) {
	g := stateWith(map[string]any{"silent_mode_timeout": int64(0), "login_rate_max_attempts": int64(-1), "trusted_proxies": json.RawMessage(`{"cidr":"x"}`)}).Gates()
	if g.SilentModeTimeout != 15*time.Minute || g.LoginRateMaxAttempts != 5 || g.TrustedProxies != nil {
		t.Fatalf("坏值应当按默认值：%+v", g)
	}
}

func TestParseTrustedProxiesRejects(t *testing.T) {
	cases := map[string]string{
		`{"cidr":"127.0.0.1"}`: "JSON 数组",
		`null`:                 "JSON 数组",
		`[{"cidr":"not-a-net","header":"X-Real-IP"}]`:             "cidr",
		`[{"cidr":"10.0.0.0/8","header":"X-Client-IP"}]`:          "header",
		`[{"cidr":"10.0.0.0/8","header":"X-Real-IP","note":"x"}]`: "恰好含",
		`[{"cidr":"10.0.0.0/8"}]`:                                 "恰好含",
		`["10.0.0.0/8"]`:                                          "恰好含",
		`[{"cidr":8,"header":"X-Real-IP"}]`:                       "字符串",
		`[{"cidr":"fe80::1%eth0","header":"X-Real-IP"}]`:          "cidr",
		`[{"cidr":"10.0.0.0/33","header":"X-Real-IP"}]`:           "cidr",
	}
	for raw, want := range cases {
		if _, err := ParseTrustedProxies([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s 应当被拒并含 %q：%v", raw, want, err)
		}
	}
	many := "[" + strings.TrimSuffix(strings.Repeat(`{"cidr":"10.0.0.1","header":"X-Real-IP"},`, MaxTrustedProxies+1), ",") + "]"
	if _, err := ParseTrustedProxies([]byte(many)); err == nil || !strings.Contains(err.Error(), "至多 64 项") {
		t.Errorf("超过 64 项应当被拒：%v", err)
	}
	if got, err := ParseTrustedProxies([]byte(`[]`)); err != nil || len(got) != 0 {
		t.Errorf("空数组合法：%v %v", got, err)
	}
}
