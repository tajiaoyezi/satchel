package singbox

import (
	"encoding/json"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// InboundExample 是一条纯 Go 内嵌的 sing-box@v1.14.4 入站规范示例。
type InboundExample struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Protocol    string          `json:"protocol"`
	Transport   string          `json:"transport"`
	Security    string          `json:"security"`
	Description string          `json:"description"`
	Spec        v1.InboundSpec  `json:"spec"`
	PlanOp      json.RawMessage `json:"plan_op,omitempty"`
}

func makeExample(id, title, protocol, transport, security, desc string, spec v1.InboundSpec) InboundExample {
	spec.Enabled = true
	if spec.ServerID == 0 {
		spec.ServerID = 1
	}
	opBytes, _ := json.Marshal(map[string]any{
		"op":   "create",
		"kind": "Inbound",
		"spec": map[string]any{
			"server_id":  spec.ServerID,
			"tag":        spec.Tag,
			"protocol":   spec.Protocol,
			"port":       spec.Port,
			"listen":     spec.Listen,
			"tls":        json.RawMessage(spec.TLS.Reveal()),
			"transport":  spec.Transport,
			"settings":   json.RawMessage(spec.Settings.Reveal()),
			"enabled":    spec.Enabled,
			"sort_order": spec.SortOrder,
		},
	})
	return InboundExample{
		ID:          id,
		Title:       title,
		Protocol:    protocol,
		Transport:   transport,
		Security:    security,
		Description: desc,
		Spec:        spec,
		PlanOp:      opBytes,
	}
}

// exampleRealityPriv 是示例用的合规 32B RawURL X25519 占位私钥（仅供展示，实际部署用 inbound keygen/build 重新生成）。
const (
	exampleRealityPriv = "aB1cD2eF3gH4iJ5kL6mN7oP8qR9sT0uV1wX2yZ3aB0E"
	exampleSS2022Key16 = "AAAAAAAAAAAAAAAAAAAAAA=="
	exampleUUID        = "6a4f1c2e-3b5d-4e7f-8a9b-0c1d2e3f4a5b"
)

var builtinExamples = []InboundExample{
	makeExample("vless-reality-vision", "VLESS + TCP + Reality (Vision)", "vless", "tcp", "reality",
		"推荐主力入站：无需域名证书，启用 xtls-rprx-vision",
		v1.InboundSpec{
			Tag: "vless-reality", Protocol: "vless", Port: 443, Listen: "::",
			TLS:      v1.SecretJSON(`{"enabled":true,"server_name":"www.microsoft.com","reality":{"enabled":true,"handshake":{"server":"www.microsoft.com","server_port":443},"private_key":"` + exampleRealityPriv + `","short_id":["a1b2c3d4"]}}`),
			Settings: v1.SecretJSON(`{"users":[{"name":"default","uuid":"` + exampleUUID + `","flow":"xtls-rprx-vision"}]}`),
		}),
	makeExample("vless-ws-tls", "VLESS + WebSocket + TLS", "vless", "ws", "tls",
		"适合搭配 CDN 反向代理的 VLESS WebSocket 入站",
		v1.InboundSpec{
			Tag: "vless-ws-tls", Protocol: "vless", Port: 8443, Listen: "::",
			TLS:       v1.SecretJSON(`{"enabled":true,"server_name":"example.com","certificate_path":"/etc/ssl/satchel/fullchain.pem","key_path":"/etc/ssl/satchel/privkey.pem"}`),
			Transport: json.RawMessage(`{"type":"ws","path":"/ws"}`),
			Settings:  v1.SecretJSON(`{"users":[{"name":"default","uuid":"` + exampleUUID + `"}]}`),
		}),
	makeExample("vless-grpc-tls", "VLESS + gRPC + TLS", "vless", "grpc", "tls",
		"多路复用 gRPC 传输层入站",
		v1.InboundSpec{
			Tag: "vless-grpc-tls", Protocol: "vless", Port: 8444, Listen: "::",
			TLS:       v1.SecretJSON(`{"enabled":true,"server_name":"example.com","certificate_path":"/etc/ssl/satchel/fullchain.pem","key_path":"/etc/ssl/satchel/privkey.pem"}`),
			Transport: json.RawMessage(`{"type":"grpc","service_name":"grpc-svc"}`),
			Settings:  v1.SecretJSON(`{"users":[{"name":"default","uuid":"` + exampleUUID + `"}]}`),
		}),
	makeExample("vmess-ws-tls", "VMess + WebSocket + TLS", "vmess", "ws", "tls",
		"兼容老客户端的 VMess WebSocket 入站",
		v1.InboundSpec{
			Tag: "vmess-ws-tls", Protocol: "vmess", Port: 8445, Listen: "::",
			TLS:       v1.SecretJSON(`{"enabled":true,"server_name":"example.com","certificate_path":"/etc/ssl/satchel/fullchain.pem","key_path":"/etc/ssl/satchel/privkey.pem"}`),
			Transport: json.RawMessage(`{"type":"ws","path":"/vmess"}`),
			Settings:  v1.SecretJSON(`{"users":[{"name":"default","uuid":"` + exampleUUID + `","alterId":0}]}`),
		}),
	makeExample("trojan-tls", "Trojan + TCP + TLS", "trojan", "tcp", "tls",
		"简洁稳定的 Trojan TLS 入站",
		v1.InboundSpec{
			Tag: "trojan-tls", Protocol: "trojan", Port: 8446, Listen: "::",
			TLS:      v1.SecretJSON(`{"enabled":true,"server_name":"example.com","certificate_path":"/etc/ssl/satchel/fullchain.pem","key_path":"/etc/ssl/satchel/privkey.pem"}`),
			Settings: v1.SecretJSON(`{"users":[{"name":"default","password":"replace-with-strong-password"}]}`),
		}),
	makeExample("shadowsocks-aead", "Shadowsocks AEAD", "shadowsocks", "tcp", "none",
		"经典 aes-256-gcm 加密入站",
		v1.InboundSpec{
			Tag: "ss-aead", Protocol: "shadowsocks", Port: 8388, Listen: "::",
			Settings: v1.SecretJSON(`{"method":"aes-256-gcm","password":"replace-with-strong-password"}`),
		}),
	makeExample("shadowsocks-2022", "Shadowsocks 2022 (多用户)", "shadowsocks", "tcp", "none",
		"抗重放高性能 SS2022 多用户入站",
		v1.InboundSpec{
			Tag: "ss-2022", Protocol: "shadowsocks", Port: 8389, Listen: "::",
			Settings: v1.SecretJSON(`{"method":"2022-blake3-aes-128-gcm","password":"` + exampleSS2022Key16 + `","users":[{"name":"default","password":"` + exampleSS2022Key16 + `"}]}`),
		}),
	makeExample("hysteria2-tls", "Hysteria2 (QUIC + TLS)", "hysteria2", "quic", "tls",
		"弱网高吞吐 QUIC 入站",
		v1.InboundSpec{
			Tag: "hy2-in", Protocol: "hysteria2", Port: 2443, Listen: "::",
			TLS:      v1.SecretJSON(`{"enabled":true,"server_name":"example.com","certificate_path":"/etc/ssl/satchel/fullchain.pem","key_path":"/etc/ssl/satchel/privkey.pem"}`),
			Settings: v1.SecretJSON(`{"users":[{"name":"default","password":"replace-with-strong-password"}]}`),
		}),
	makeExample("anytls-tls", "AnyTLS + TLS", "anytls", "tcp", "tls",
		"抗填充特征检测的 AnyTLS 入站（仅支持 TLS）",
		v1.InboundSpec{
			Tag: "anytls-in", Protocol: "anytls", Port: 8447, Listen: "::",
			TLS:      v1.SecretJSON(`{"enabled":true,"server_name":"example.com","certificate_path":"/etc/ssl/satchel/fullchain.pem","key_path":"/etc/ssl/satchel/privkey.pem"}`),
			Settings: v1.SecretJSON(`{"users":[{"name":"default","password":"replace-with-strong-password"}]}`),
		}),
	makeExample("tuic-tls", "TUIC v5 (QUIC + TLS)", "tuic", "quic", "tls",
		"低延迟 QUIC 代理入站",
		v1.InboundSpec{
			Tag: "tuic-in", Protocol: "tuic", Port: 2444, Listen: "::",
			TLS:      v1.SecretJSON(`{"enabled":true,"server_name":"example.com","certificate_path":"/etc/ssl/satchel/fullchain.pem","key_path":"/etc/ssl/satchel/privkey.pem"}`),
			Settings: v1.SecretJSON(`{"users":[{"name":"default","uuid":"` + exampleUUID + `","password":"replace-with-strong-password"}],"congestion_control":"bbr"}`),
		}),
	makeExample("socks-plain", "SOCKS5", "socks", "tcp", "none",
		"内网或本机调试用 SOCKS5 入站",
		v1.InboundSpec{
			Tag: "socks-in", Protocol: "socks", Port: 1080, Listen: "127.0.0.1",
			Settings: v1.SecretJSON(`{"users":[{"username":"default","password":"replace-with-strong-password"}]}`),
		}),
	makeExample("mixed-plain", "Mixed (HTTP + SOCKS5)", "mixed", "tcp", "none",
		"单端口同时提供 HTTP 与 SOCKS5 代理",
		v1.InboundSpec{
			Tag: "mixed-in", Protocol: "mixed", Port: 7890, Listen: "127.0.0.1",
			Settings: v1.SecretJSON(`{}`),
		}),
	makeExample("snell-v5", "Snell v5 (sing-box 原生)", "snell", "tcp", "none",
		"sing-box@v1.14.4 原生 Snell v5 多用户入站",
		v1.InboundSpec{
			Tag: "snell-in", Protocol: "snell", Port: 8450, Listen: "::",
			Settings: v1.SecretJSON(`{"version":5,"psk":"replace-with-strong-password","users":[{"name":"default","userkey":"replace-with-userkey"}]}`),
		}),
	makeExample("direct-tunnel", "Direct 任意门端口转发", "direct", "tcp", "none",
		"配合路由 override_address / override_port 做中转端口转发",
		v1.InboundSpec{
			Tag: "direct-in", Protocol: "direct", Port: 10080, Listen: "::",
			Settings: v1.SecretJSON(`{}`),
		}),
}

// ListExamples 按可选协议、传输、安全筛选内嵌示例库。
func ListExamples(protocol, transport, security string) []InboundExample {
	p := strings.ToLower(strings.TrimSpace(protocol))
	tr := strings.ToLower(strings.TrimSpace(transport))
	sec := strings.ToLower(strings.TrimSpace(security))
	out := make([]InboundExample, 0, len(builtinExamples))
	for _, ex := range builtinExamples {
		if p != "" && ex.Protocol != p {
			continue
		}
		if tr != "" && ex.Transport != tr {
			continue
		}
		if sec != "" && ex.Security != sec {
			continue
		}
		out = append(out, ex)
	}
	return out
}
