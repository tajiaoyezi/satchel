package ipaddr

import "testing"

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.7":         "198.51.100.7",
		"::ffff:198.51.100.7":  "198.51.100.7",
		"2001:DB8:0:0:0:0:0:1": "2001:db8::1",
		"::1":                  "::1",
	} {
		if got, ok := Canonical(in); !ok || got != want {
			t.Errorf("Canonical(%q) = %q %v，想要 %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "not-an-ip", "10.0.0.0/8", "example.com", "fe80::1%eth0", " 1.2.3.4"} {
		if got, ok := Canonical(bad); ok {
			t.Errorf("Canonical(%q) 应当拒绝，得到 %q", bad, got)
		}
	}
}

func TestIsLocalOrPrivate(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.5.4", "192.168.1.1", "fd00::1", "169.254.1.1", "fe80::1", "0.0.0.0", "::ffff:127.0.0.1", "", "garbage"} {
		if !IsLocalOrPrivate(ip) {
			t.Errorf("%q 应当算本地或内网", ip)
		}
	}
	for _, ip := range []string{"198.51.100.7", "203.0.113.9", "8.8.8.8", "2001:db8::1", "172.32.0.1"} {
		if IsLocalOrPrivate(ip) {
			t.Errorf("%q 不该算本地或内网", ip)
		}
	}
}
