package singbox

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

const (
	// StealTunnelInboundTag 是服务器偷自己 tunnel 模式下固定使用的 443 direct 入站标签。
	StealTunnelInboundTag = "tunnel-in"
	// DefaultPortRangeMin 是服务器未显式设置 port_range_min/max 时的默认自动分配起始端口。
	DefaultPortRangeMin = 10000
	// DefaultPortRangeMax 是服务器未显式设置 port_range_min/max 时的默认自动分配结束端口。
	DefaultPortRangeMax = 60000
)

func validateDirectNetworkValue(tag string, raw any) error {
	if raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "tcp" || s == "udp" {
			return nil
		}
		if s == "tcp,udp" || s == "udp,tcp" {
			return v1.Newf(v1.CodeBadRequest, "入站 %s: direct 入站 settings.network 不接受逗号字符串 %q，同时监听 TCP 与 UDP 请省略 network 字段或使用 [\"tcp\",\"udp\"]", tag, v)
		}
		return v1.Newf(v1.CodeBadRequest, "入站 %s: direct 入站 settings.network 取值 %q 不合法（仅允许 \"tcp\"、\"udp\" 或 [\"tcp\",\"udp\"]）", tag, v)
	case []any:
		if len(v) == 0 {
			return v1.Newf(v1.CodeBadRequest, "入站 %s: direct 入站 settings.network 数组不能为空", tag)
		}
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return v1.Newf(v1.CodeBadRequest, "入站 %s: direct 入站 settings.network 数组元素必须为字符串", tag)
			}
			n := strings.ToLower(strings.TrimSpace(s))
			if n != "tcp" && n != "udp" {
				return v1.Newf(v1.CodeBadRequest, "入站 %s: direct 入站 settings.network 含非法协议 %q（仅允许 tcp/udp）", tag, s)
			}
		}
		return nil
	default:
		return v1.Newf(v1.CodeBadRequest, "入站 %s: direct 入站 settings.network 必须是字符串或字符串数组", tag)
	}
}

func directNetFromSettings(settingsObj map[string]any) string {
	if settingsObj == nil {
		return "both"
	}
	raw, exists := settingsObj["network"]
	if !exists || raw == nil {
		return "both"
	}
	var hasTCP, hasUDP bool
	switch v := raw.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "tcp":
			hasTCP = true
		case "udp":
			hasUDP = true
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				switch strings.ToLower(strings.TrimSpace(s)) {
				case "tcp":
					hasTCP = true
				case "udp":
					hasUDP = true
				}
			}
		}
	}
	switch {
	case hasTCP && !hasUDP:
		return "tcp"
	case hasUDP && !hasTCP:
		return "udp"
	default:
		return "both"
	}
}

// NormalizeDirectNetwork 将 CLI/API 传入的网络类型参数归一为：
// - settingValue: 写入 direct Inbound.spec.settings.network 的值（"" 表示省略，即 TCP+UDP；"tcp"；"udp"）；
// - label: 对外展示的规范标签（"tcp,udp" / "tcp" / "udp"）。
func NormalizeDirectNetwork(raw string) (settingValue string, label string, err error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "", "tcp,udp", "udp,tcp", "both":
		return "", "tcp,udp", nil
	case "tcp":
		return "tcp", "tcp", nil
	case "udp":
		return "udp", "udp", nil
	default:
		return "", "", v1.Newf(v1.CodeBadRequest, "不支持的网络类型 %q（可选：tcp,udp、tcp、udp）", raw)
	}
}

// DirectNetworkSettingsJSON 返回 direct 入站合法的 settings JSON（双协议为空对象 {}，避免写入非法的 "tcp,udp"）。
func DirectNetworkSettingsJSON(settingValue string) json.RawMessage {
	switch strings.ToLower(strings.TrimSpace(settingValue)) {
	case "tcp":
		return json.RawMessage(`{"network":"tcp"}`)
	case "udp":
		return json.RawMessage(`{"network":"udp"}`)
	default:
		return json.RawMessage(`{}`)
	}
}

// DirectNetworkLabel 从 direct 入站的 settings JSON 提取对外展示的网络类型（"tcp"、"udp" 或 "tcp,udp"）。
func DirectNetworkLabel(settingsRaw json.RawMessage) string {
	obj, err := decodeOptionalObject(settingsRaw, "inbound.settings")
	if err != nil {
		return "tcp,udp"
	}
	switch directNetFromSettings(obj) {
	case "tcp":
		return "tcp"
	case "udp":
		return "udp"
	default:
		return "tcp,udp"
	}
}

// DefaultDirectNetworkForProtocol 根据目标入站协议返回缺省的任意门隧道网络类型标签。
func DefaultDirectNetworkForProtocol(protocol string) string {
	switch inboundTransportNet(protocol, nil) {
	case "udp":
		return "udp"
	case "tcp":
		return "tcp"
	default:
		return "tcp,udp"
	}
}

// ValidateChainLabel 校验链式隧道 label（仅允许小写字母、数字与短横线，长度 2..32，首尾为字母或数字）。
func ValidateChainLabel(label string) (string, error) {
	s := strings.TrimSpace(label)
	if len(s) < 2 || len(s) > 32 {
		return "", v1.Newf(v1.CodeBadRequest, "链式隧道 label %q 长度必须在 2..32 之间", label)
	}
	isAlphaNum := func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
	}
	if !isAlphaNum(s[0]) || !isAlphaNum(s[len(s)-1]) {
		return "", v1.Newf(v1.CodeBadRequest, "链式隧道 label %q 首尾必须为小写字母或数字", label)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlphaNum(c) && c != '-' {
			return "", v1.Newf(v1.CodeBadRequest, "链式隧道 label %q 仅允许小写字母、数字与短横线", label)
		}
	}
	return s, nil
}

// FormatChainTag 构造链式隧道第 hop 跳（从 0 起）的固定入站标签 tunnel-<label>-h<hop>。
func FormatChainTag(label string, hop int) string {
	return fmt.Sprintf("tunnel-%s-h%d", label, hop)
}

// ParseChainTag 解析符合 tunnel-<label>-h<hop> 规范的链式隧道入站标签。
func ParseChainTag(tag string) (label string, hop int, ok bool) {
	const prefix = "tunnel-"
	if !strings.HasPrefix(tag, prefix) {
		return "", 0, false
	}
	rest := tag[len(prefix):]
	idx := strings.LastIndex(rest, "-h")
	if idx <= 0 || idx+2 >= len(rest) {
		return "", 0, false
	}
	candLabel := rest[:idx]
	hopStr := rest[idx+2:]
	n, err := strconv.Atoi(hopStr)
	if err != nil || n < 0 || strconv.Itoa(n) != hopStr {
		return "", 0, false
	}
	if _, err := ValidateChainLabel(candLabel); err != nil {
		return "", 0, false
	}
	return candLabel, n, true
}

func resolvePortBounds(minPort, maxPort int) (int, int, error) {
	if minPort == 0 && maxPort == 0 {
		return DefaultPortRangeMin, DefaultPortRangeMax, nil
	}
	if minPort < 1 || minPort > 65535 || maxPort < 1 || maxPort > 65535 || minPort > maxPort {
		return 0, 0, v1.Newf(v1.CodeBadRequest, "端口范围 %d..%d 不合法（要求 1..65535 且 min <= max）", minPort, maxPort)
	}
	return minPort, maxPort, nil
}

// AllocateFreePort 在 [minPort, maxPort]（均为 0 时取默认 10000..60000）内随机挑选一个未占用端口。
// 当 forbid443 为真时自动跳过 443。通过加密随机起点加单圈线性探测保证只要范围内存在空闲端口就必能命中。
func AllocateFreePort(used map[int]bool, minPort, maxPort int, forbid443 bool) (int, error) {
	lo, hi, err := resolvePortBounds(minPort, maxPort)
	if err != nil {
		return 0, err
	}
	span := hi - lo + 1
	start := 0
	if span > 1 {
		n, rerr := rand.Int(rand.Reader, big.NewInt(int64(span)))
		if rerr == nil {
			start = int(n.Int64())
		}
	}
	for i := 0; i < span; i++ {
		p := lo + (start+i)%span
		if forbid443 && p == 443 {
			continue
		}
		if !used[p] {
			return p, nil
		}
	}
	return 0, v1.Newf(v1.CodeConflict, "端口范围 %d..%d 内已无可用空闲端口", lo, hi)
}

// NextFreePort 在 [minPort, maxPort]（均为 0 时取默认 10000..60000）内确定性返回最小的可用空闲端口（供 status 预览使用）。
func NextFreePort(used map[int]bool, minPort, maxPort int, forbid443 bool) (int, bool) {
	lo, hi, err := resolvePortBounds(minPort, maxPort)
	if err != nil {
		return 0, false
	}
	for p := lo; p <= hi; p++ {
		if forbid443 && p == 443 {
			continue
		}
		if !used[p] {
			return p, true
		}
	}
	return 0, false
}
