package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// rules 是字段的写侧规则（master-settings「字段规则照 mmwx 的写侧校验，只拒绝不改写」）：只登记 mmwx 写侧真有的规则，
// 每条注释来源（详见本 change 的 notes/mmwx-settings-defaults.md）；mmwx 里 clamp 或静默换默认值的地方这里一律拒绝并点名范围。
// 没有登记的字段只做类型检查。要看文件系统或别的资源才能验的规则不在这里：默认模板文件名要存在于 rule_templates/（M3）、
// 探针 ping 目标的 SSRF 校验（M7）、Reality 域名的归一化（M6）、user_perm_pages 的白名单（M3）。
// 规则收到的值已经是字段类型的 Go 值：bool / int64 / string / json.RawMessage。
var rules = map[string]func(v any) error{
	// handler/system_settings.go:1230-1235
	"subscription_output_format": oneOf("yaml", "json"),
	// handler/system_settings.go:1307-1316；premium 在 mmwx 要付费许可证，Satchel 没有这道门（License 不适用）。
	"default_theme": oneOf("flat", "pixel", "anime", "premium"),
	// handler/system_settings.go:1042-1053：mmwx 小于下限改回默认，这里拒绝。
	"speed_collect_interval":   atLeast(1),
	"traffic_collect_interval": atLeast(10),
	"traffic_check_interval":   atLeast(10),
	"heartbeat_interval":       atLeast(5),
	// handler/system_settings.go:973-978（clamp 到 [1000, 60000]）。
	"dashboard_refresh_interval_ms": between(1000, 60000),
	// handler/system_settings.go:802-815（clamp 到 [2000, 300000]）。
	"probe_disguise_ping_interval_ms": between(2000, 300000),
	// handler/system_settings.go:396-409：空视为 follow；1..64 字节、只有 [A-Za-z0-9_-]。
	"probe_disguise_theme": themeName,
	// handler/notify_config.go:214-233：越界时 mmwx 静默保留旧值，这里拒绝。
	"notify_traffic_threshold_percent":  between(1, 100),
	"notify_package_expiring_days":      between(1, 365),
	"notify_agent_long_offline_minutes": between(1, 1440),
	// handler/notify_config.go:244-249。
	"notify_server_tolerance_seconds": atLeast(0),
	// handler/user_config.go:358-373。
	"proxy_groups_source_url": httpURLOrEmpty,
	// handler/system_settings.go:1384-1390 只限 2000 字节、不看形状；Satchel 加一条与探针 logo 相同的形状规则，
	// 免得 javascript: 之类进了库、m1-10 登录页当 URL 用时变成存储型 XSS。
	"login_wallpaper": imageRef(2000),
	// handler/system_settings.go:638-666：<= 128KB，非空须以 /、http://、https:// 或 data:image/ 开头。
	"probe_disguise_logo": imageRef(128 * 1024),
	// Satchel 新增：mmwx 收明文 token 自己算 SHA-256（handler/system_settings.go:616-624），Satchel 由客户端算好交上来，
	// 库里存的就是 64 位小写十六进制；不合形状的哈希 M7 比对时只会让外部探针全部 401，现在就拦住。
	"probe_external_token_sha256": sha256HexOrEmpty,
	// handler/security_settings.go:138-153（> 0 否则 400）。
	"brute_force_max_failures":   atLeast(1),
	"brute_force_window_minutes": atLeast(1),
	"brute_force_block_minutes":  atLeast(1),
	"login_rate_max_attempts":    atLeast(1),
	"login_rate_window_minutes":  atLeast(1),
	"login_rate_lock_minutes":    atLeast(1),
	"sub_rate_limit":             atLeast(1),
	"sub_rate_window_minutes":    atLeast(1),
	// handler/security_settings.go:154-159（非空时长度 >= 20）。
	"turnstile_site_key": emptyOrMinLen(20),
	// handler/system_settings.go:221-240。
	"master_recovery_failure_minutes":       between(1, 60),
	"master_recovery_startup_grace_minutes": between(1, 120),
	// handler/system_settings.go:1435-1437（<=0 改成 15，这里拒绝）。
	"silent_mode_timeout": atLeast(1),
	// handler/user_permissions.go:197-214（负数归 0，这里拒绝）。
	"user_quota_subscribe":             atLeast(0),
	"user_quota_template":              atLeast(0),
	"user_quota_override":              atLeast(0),
	"user_quota_routed_outbound":       atLeast(0),
	"user_routed_outbound_daily_limit": atLeast(0),
	// handler/system_settings.go:751-768（超过 30 个截断，这里拒绝）。
	"probe_disguise_ping_targets": maxItems(30),
}

func oneOf(allowed ...string) func(any) error {
	return func(v any) error {
		s := v.(string)
		for _, a := range allowed {
			if s == a {
				return nil
			}
		}
		return fmt.Errorf("只能是 %s，得到 %q", strings.Join(allowed, " / "), s)
	}
}

func atLeast(min int64) func(any) error {
	return func(v any) error {
		if n := v.(int64); n < min {
			return fmt.Errorf("至少为 %d，得到 %d", min, n)
		}
		return nil
	}
}

func between(min, max int64) func(any) error {
	return func(v any) error {
		if n := v.(int64); n < min || n > max {
			return fmt.Errorf("必须在 %d 到 %d 之间，得到 %d", min, max, n)
		}
		return nil
	}
}

func maxBytes(limit int) func(any) error {
	return func(v any) error {
		if n := len(v.(string)); n > limit {
			return fmt.Errorf("最长 %d 字节，得到 %d 字节", limit, n)
		}
		return nil
	}
}

func emptyOrMinLen(min int) func(any) error {
	return func(v any) error {
		if s := v.(string); s != "" && len(s) < min {
			return fmt.Errorf("非空时至少 %d 个字符，得到 %d 个", min, len(s))
		}
		return nil
	}
}

func maxItems(limit int) func(any) error {
	return func(v any) error {
		raw := bytes.TrimSpace(v.(json.RawMessage))
		var items []json.RawMessage
		// json.Unmarshal 会把 null 解成空切片，所以先看第一个字符。
		if !bytes.HasPrefix(raw, []byte("[")) || json.Unmarshal(raw, &items) != nil {
			return fmt.Errorf("必须是 JSON 数组")
		}
		if len(items) > limit {
			return fmt.Errorf("至多 %d 项，得到 %d 项", limit, len(items))
		}
		return nil
	}
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func sha256HexOrEmpty(v any) error {
	if s := v.(string); s != "" && !sha256HexRe.MatchString(s) {
		return fmt.Errorf("必须是 64 位小写十六进制的 SHA-256（或空串表示清掉）")
	}
	return nil
}

var themeNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func themeName(v any) error {
	if s := v.(string); s != "" && !themeNameRe.MatchString(s) {
		return fmt.Errorf("只能是 1 到 64 个字母、数字、下划线或连字符（空表示 follow），得到 %q", s)
	}
	return nil
}

func httpURLOrEmpty(v any) error {
	s := v.(string)
	if s == "" {
		return nil
	}
	u, err := url.ParseRequestURI(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("必须是 http 或 https 的 URL，得到 %q", s)
	}
	return nil
}

// imageRef 是图片引用的形状：不超过 limit 字节，非空须以 /、http://、https:// 或 data:image/ 开头。
func imageRef(limit int) func(any) error {
	return func(v any) error {
		s := v.(string)
		if len(s) > limit {
			return fmt.Errorf("最长 %d 字节，得到 %d 字节", limit, len(s))
		}
		if s == "" {
			return nil
		}
		for _, prefix := range []string{"/", "http://", "https://", "data:image/"} {
			if strings.HasPrefix(s, prefix) {
				return nil
			}
		}
		return fmt.Errorf("必须以 /、http://、https:// 或 data:image/ 开头")
	}
}

// normalizeOrigin 把主控地址或订阅域名归一成干净的 HTTP(S) origin（master-settings「主控地址是人类专属的设置写」）：
// scheme 是 http / https、有 host、没有路径（单个 / 除外）、查询、片段与用户信息；返回 scheme://host[:port]。空串原样返回（表示清掉）。
func normalizeOrigin(flag, raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	bad := func() (string, error) {
		return "", v1.Newf(v1.CodeBadRequest, "参数 %s 必须是干净的 HTTP(S) 地址（只有 scheme 与 host，可带端口），得到 %q", flag, raw).
			WithNext("例如 https://panel.example.com 或 https://panel.example.com:8443")
	}
	u, err := url.ParseRequestURI(s)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		strings.HasSuffix(u.Host, ":") { // url 包放过 host: 这种空端口，这里不收
		return bad()
	}
	return u.Scheme + "://" + u.Host, nil
}
