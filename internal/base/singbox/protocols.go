package singbox

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// ProtocolInfo 描述 sing-box@v1.14.4 原生支持的单个入站协议能力与约束。
type ProtocolInfo struct {
	Protocol       string   `json:"protocol"`
	DisplayName    string   `json:"display_name"`
	Category       string   `json:"category"` // "proxy" | "tunnel" | "plain"
	Transports     []string `json:"transports"`
	Securities     []string `json:"securities"`
	AuthModel      string   `json:"auth_model"`
	RequiredFields []string `json:"required_fields"`
	Notes          string   `json:"notes,omitempty"`
}

// ParallelProtocolNote 说明并行协议移植线的协议状态。
type ParallelProtocolNote struct {
	Protocol string `json:"protocol"`
	Status   string `json:"status"`
	Notes    string `json:"notes"`
}

// ProtocolMatrixResult 是 inbound protocols 返回的完整协议矩阵。
type ProtocolMatrixResult struct {
	KernelVersion string                 `json:"kernel_version"`
	Native        []ProtocolInfo         `json:"native"`
	ParallelTrack []ParallelProtocolNote `json:"parallel_track"`
	Unsupported   []string               `json:"unsupported"`
}

// SS2022KeyBytes 记录 Shadowsocks 2022 算法要求的原始密钥字节长度。
var SS2022KeyBytes = map[string]int{
	"2022-blake3-aes-128-gcm":       16,
	"2022-blake3-aes-256-gcm":       32,
	"2022-blake3-chacha20-poly1305": 32,
}

var nativeProtocols = []ProtocolInfo{
	{
		Protocol: "vless", DisplayName: "VLESS", Category: "proxy",
		Transports: []string{"tcp", "ws", "grpc", "http", "httpupgrade"},
		Securities: []string{"none", "tls", "reality"},
		AuthModel:  "uuid", RequiredFields: []string{"users[].uuid"},
		Notes: "tcp + tls/reality 可配 flow=xtls-rprx-vision；Reality 公钥仅用于客户端、不得写入 kernel.json",
	},
	{
		Protocol: "vmess", DisplayName: "VMess", Category: "proxy",
		Transports: []string{"tcp", "ws", "grpc", "http", "httpupgrade"},
		Securities: []string{"none", "tls"},
		AuthModel:  "uuid", RequiredFields: []string{"users[].uuid"},
	},
	{
		Protocol: "trojan", DisplayName: "Trojan", Category: "proxy",
		Transports: []string{"tcp", "ws", "grpc", "http", "httpupgrade"},
		Securities: []string{"none", "tls", "reality"},
		AuthModel:  "password", RequiredFields: []string{"users[].password"},
	},
	{
		Protocol: "shadowsocks", DisplayName: "Shadowsocks (AEAD / 2022)", Category: "proxy",
		Transports: []string{"tcp"},
		Securities: []string{"none"},
		AuthModel:  "method+password", RequiredFields: []string{"method", "password"},
		Notes: "支持传统 AEAD 与 SS2022（2022-blake3-aes-128-gcm / aes-256-gcm / chacha20-poly1305）多用户",
	},
	{
		Protocol: "hysteria2", DisplayName: "Hysteria2", Category: "proxy",
		Transports: []string{"quic"},
		Securities: []string{"tls"},
		AuthModel:  "password", RequiredFields: []string{"users[].password", "tls"},
		Notes: "基于 QUIC，强制要求 TLS；可选 obfs.type=salamander",
	},
	{
		Protocol: "hysteria", DisplayName: "Hysteria v1", Category: "proxy",
		Transports: []string{"quic"},
		Securities: []string{"tls"},
		AuthModel:  "auth_str", RequiredFields: []string{"users[].auth_str", "tls"},
		Notes: "基于 QUIC，强制要求 TLS",
	},
	{
		Protocol: "anytls", DisplayName: "AnyTLS", Category: "proxy",
		Transports: []string{"tcp"},
		Securities: []string{"tls"},
		AuthModel:  "password", RequiredFields: []string{"users[].password", "tls"},
		Notes: "强制要求 TLS；明确不支持 Reality",
	},
	{
		Protocol: "tuic", DisplayName: "TUIC", Category: "proxy",
		Transports: []string{"quic"},
		Securities: []string{"tls"},
		AuthModel:  "uuid+password", RequiredFields: []string{"users[].uuid", "users[].password", "tls"},
		Notes: "基于 QUIC，强制要求 TLS",
	},
	{
		Protocol: "socks", DisplayName: "SOCKS5", Category: "plain",
		Transports: []string{"tcp"},
		Securities: []string{"none"},
		AuthModel:  "optional username+password", RequiredFields: []string{},
	},
	{
		Protocol: "http", DisplayName: "HTTP Proxy", Category: "plain",
		Transports: []string{"tcp"},
		Securities: []string{"none", "tls"},
		AuthModel:  "optional username+password", RequiredFields: []string{},
	},
	{
		Protocol: "mixed", DisplayName: "Mixed (HTTP + SOCKS5)", Category: "plain",
		Transports: []string{"tcp"},
		Securities: []string{"none"},
		AuthModel:  "optional username+password", RequiredFields: []string{},
	},
	{
		Protocol: "shadowtls", DisplayName: "ShadowTLS", Category: "proxy",
		Transports: []string{"tcp"},
		Securities: []string{"none"},
		AuthModel:  "handshake+users", RequiredFields: []string{"version", "handshake"},
		Notes: "v3 支持多用户 password 与 detour 转发至内部入站",
	},
	{
		Protocol: "naive", DisplayName: "NaiveProxy", Category: "proxy",
		Transports: []string{"tcp"},
		Securities: []string{"tls"},
		AuthModel:  "username+password", RequiredFields: []string{"users[].username", "users[].password", "tls"},
		Notes: "强制要求 TLS",
	},
	{
		Protocol: "snell", DisplayName: "Snell (sing-box 原生 v5/v6)", Category: "proxy",
		Transports: []string{"tcp"},
		Securities: []string{"none"},
		AuthModel:  "psk+users", RequiredFields: []string{"version", "psk"},
		Notes: "sing-box@v1.14.4 原生支持 v5/v6（共享 psk + 每用户 userkey）；v4 每用户独立 PSK 归并行协议线",
	},
	{
		Protocol: "direct", DisplayName: "Direct (任意门/端口转发)", Category: "tunnel",
		Transports: []string{"tcp"},
		Securities: []string{"none"},
		AuthModel:  "none", RequiredFields: []string{},
		Notes: "隧道中转端口转发用；sing-box 1.13+ 目标覆盖走 route.rules 的 override_address / override_port",
	},
}

var nativeProtocolMap = func() map[string]ProtocolInfo {
	m := make(map[string]ProtocolInfo, len(nativeProtocols))
	for _, p := range nativeProtocols {
		m[p.Protocol] = p
	}
	return m
}()

// ProtocolMatrix 返回完整的原生入站协议矩阵及并行移植线说明。
func ProtocolMatrix() ProtocolMatrixResult {
	out := make([]ProtocolInfo, len(nativeProtocols))
	copy(out, nativeProtocols)
	return ProtocolMatrixResult{
		KernelVersion: "sing-box@v1.14.4",
		Native:        out,
		ParallelTrack: []ParallelProtocolNote{
			{Protocol: "xhttp", Status: "parallel_track", Notes: "XHTTP 传输层归并行协议移植线，等待自定义入站向导挂接后独立交付"},
			{Protocol: "snell-v4", Status: "parallel_track", Notes: "Snell v4 每用户独立 PSK 模式归并行协议移植线（sing-box 原生仅支持 v5/v6）"},
			{Protocol: "mieru", Status: "parallel_track", Notes: "Mieru 服务端归并行协议移植线"},
			{Protocol: "vless-encryption", Status: "parallel_track", Notes: "VLESS Encryption（mlkem768x25519plus）归并行协议移植线"},
		},
		Unsupported: []string{
			"anytls + reality（无客户端支持）",
			"mKCP（已淘汰）",
			"TCP HTTP 伪装头（已淘汰）",
			"入站 sniff / sniff_override_destination / domain_strategy（sing-box 1.13 已移除，改用 route.rules 的 action: sniff / resolve）",
			"路由 geoip / geosite / user（sing-box 1.12 已移除，改用 route.rule_set 与 auth_user）",
		},
	}
}

// LookupProtocol 按协议名查找原生入站协议定义。
func LookupProtocol(protocol string) (ProtocolInfo, bool) {
	p, ok := nativeProtocolMap[strings.ToLower(strings.TrimSpace(protocol))]
	return p, ok
}

var deprecatedInboundKeys = map[string]string{
	"sniff":                      `sing-box 1.13 已移除入站字段 "sniff"，请改用路由规则 {"action":"sniff"}（渲染器会自动前置）`,
	"sniff_override_destination": `sing-box 1.13 已移除入站字段 "sniff_override_destination"，请改用路由规则 {"action":"sniff"}`,
	"domain_strategy":            `sing-box 1.13 已移除入站字段 "domain_strategy"，请改用路由规则 {"action":"resolve"}`,
	"override_address":           `sing-box 1.13 已移除 direct 入站字段 "override_address"，请改用路由规则动作中的 override_address`,
	"override_port":              `sing-box 1.13 已移除 direct 入站字段 "override_port"，请改用路由规则动作中的 override_port`,
}

// ValidateInboundSpec 校验单条入站规格的协议、传输、安全组合及废弃字段。
func ValidateInboundSpec(spec v1.InboundSpec) error {
	proto := strings.ToLower(strings.TrimSpace(spec.Protocol))
	info, ok := nativeProtocolMap[proto]
	if !ok {
		return v1.Newf(v1.CodeBadRequest, "入站 %s 使用了不支持的原生协议 %q（可用 inbound protocols 查看支持清单）", spec.Tag, spec.Protocol)
	}
	settingsObj, err := decodeOptionalObject(spec.Settings.Reveal(), "Inbound.spec.settings")
	if err != nil {
		return fmt.Errorf("入站 %s: %w", spec.Tag, err)
	}
	for k, hint := range deprecatedInboundKeys {
		if _, exists := settingsObj[k]; exists {
			return v1.Newf(v1.CodeBadRequest, "入站 %s: %s", spec.Tag, hint)
		}
	}
	tlsObj, err := decodeOptionalObject(spec.TLS.Reveal(), "Inbound.spec.tls")
	if err != nil {
		return fmt.Errorf("入站 %s: %w", spec.Tag, err)
	}
	trObj, err := decodeOptionalObject(spec.Transport, "Inbound.spec.transport")
	if err != nil {
		return fmt.Errorf("入站 %s: %w", spec.Tag, err)
	}

	// 检查传输层组合
	if len(trObj) > 0 {
		trType, _ := trObj["type"].(string)
		trType = strings.ToLower(strings.TrimSpace(trType))
		if trType == "" {
			return v1.Newf(v1.CodeBadRequest, "入站 %s 的 transport 缺少 type 字段", spec.Tag)
		}
		if !slices.Contains(info.Transports, trType) {
			return v1.Newf(v1.CodeBadRequest, "入站 %s 的协议 %s 不支持传输层 %q（允许：%s）", spec.Tag, proto, trType, strings.Join(info.Transports, ", "))
		}
	}

	// 检查安全层（none / tls / reality）
	security := "none"
	if len(tlsObj) > 0 {
		if en, _ := tlsObj["enabled"].(bool); en {
			security = "tls"
			if realMap, ok := tlsObj["reality"].(map[string]any); ok {
				if ren, _ := realMap["enabled"].(bool); ren {
					security = "reality"
				}
			}
		}
	}
	if !slices.Contains(info.Securities, security) {
		return v1.Newf(v1.CodeBadRequest, "入站 %s 的协议 %s 不支持安全层 %q（允许：%s）", spec.Tag, proto, security, strings.Join(info.Securities, ", "))
	}
	if security == "reality" {
		realMap, _ := tlsObj["reality"].(map[string]any)
		priv, _ := realMap["private_key"].(string)
		if strings.TrimSpace(priv) == "" {
			return v1.Newf(v1.CodeBadRequest, "入站 %s 启用 Reality 时必须提供 tls.reality.private_key", spec.Tag)
		}
		hs, _ := realMap["handshake"].(map[string]any)
		if hs != nil {
			if srv, _ := hs["server"].(string); strings.TrimSpace(srv) == "" {
				return v1.Newf(v1.CodeBadRequest, "入站 %s 的 tls.reality.handshake.server 不能为空", spec.Tag)
			}
		}
	}

	// direct 入站校验 settings.network 仅允许省略、"tcp"、"udp" 或 ["tcp","udp"]（拒绝非法 "tcp,udp" 字符串）。
	if proto == "direct" {
		if rawNet, exists := settingsObj["network"]; exists {
			if err := validateDirectNetworkValue(spec.Tag, rawNet); err != nil {
				return err
			}
		}
	}

	// SS2022 校验算法名（若给出 2022- 前缀则校验合法算法名）
	if proto == "shadowsocks" {
		if method, _ := settingsObj["method"].(string); strings.HasPrefix(method, "2022-") {
			if _, ok := SS2022KeyBytes[method]; !ok {
				return v1.Newf(v1.CodeBadRequest, "入站 %s 的 Shadowsocks 2022 加密算法 %q 不合法", spec.Tag, method)
			}
		}
	}
	return nil
}

// ValidateSS2022KeyFormat 校验单段 SS2022 密钥是否为指定算法对应的标准 Base64 字节长度。
func ValidateSS2022KeyFormat(cipher, key string) error {
	wantBytes, ok := SS2022KeyBytes[cipher]
	if !ok {
		return v1.Newf(v1.CodeBadRequest, "不支持的 Shadowsocks 2022 算法 %q", cipher)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil || len(raw) != wantBytes {
		return v1.Newf(v1.CodeBadRequest, "算法 %s 的密码必须是 %d 字节的标准 Base64 字符串", cipher, wantBytes)
	}
	return nil
}
