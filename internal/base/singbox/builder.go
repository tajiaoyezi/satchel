package singbox

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// BuildInboundRequest 是无状态入站向导（inbound build）的输入参数。
type BuildInboundRequest struct {
	ServerID    int64
	Tag         string
	Protocol    string
	Port        int
	Listen      string
	Transport   string
	Security    string
	SNI         string
	Handshake   string
	Path        string
	ServiceName string
	Cipher      string
	Flow        string
	CertPath    string
	KeyPath     string
	UserName    string
	Password    string
	UUID        string
	SortOrder   int
}

// BuildCredentials 是在 inbound build 时生成的凭据与客户端连接所需元信息（含 Reality 公钥）。
type BuildCredentials struct {
	UserName          string `json:"user_name,omitempty"`
	UUID              string `json:"uuid,omitempty"`
	Password          string `json:"password,omitempty"`
	ServerPSK         string `json:"server_psk,omitempty"`
	UserPSK           string `json:"user_psk,omitempty"`
	RealityPrivateKey string `json:"reality_private_key,omitempty"`
	RealityPublicKey  string `json:"reality_public_key,omitempty"`
	ShortID           string `json:"short_id,omitempty"`
	Flow              string `json:"flow,omitempty"`
}

// BuiltInbound 是 inbound build 的完整输出，可直接把 PlanOp 提交给 satchel plan --spec。
type BuiltInbound struct {
	Spec        v1.InboundSpec   `json:"spec"`
	Credentials BuildCredentials `json:"credentials"`
	PlanOp      map[string]any   `json:"plan_op"`
}

// BuildInbound 按 sing-box@v1.14.4 协议矩阵无状态构造合规的 InboundSpec 并自动生成缺省凭据。
func BuildInbound(req BuildInboundRequest) (*BuiltInbound, error) {
	if req.ServerID <= 0 {
		return nil, v1.New(v1.CodeBadRequest, "server_id 必须为正整数")
	}
	proto := strings.ToLower(strings.TrimSpace(req.Protocol))
	info, ok := LookupProtocol(proto)
	if !ok {
		return nil, v1.Newf(v1.CodeBadRequest, "不支持的入站协议 %q（可用 inbound protocols 查看支持清单）", req.Protocol)
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "port %d 必须在 1..65535 之间", req.Port)
	}
	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		tag = fmt.Sprintf("%s-%d", proto, req.Port)
	}
	userName := strings.TrimSpace(req.UserName)
	if userName == "" {
		userName = "default"
	}

	transport := strings.ToLower(strings.TrimSpace(req.Transport))
	if transport == "" {
		transport = info.Transports[0]
	}
	if !slices.Contains(info.Transports, transport) {
		return nil, v1.Newf(v1.CodeBadRequest, "协议 %s 不支持传输层 %q（允许：%s）", proto, transport, strings.Join(info.Transports, ", "))
	}

	security := strings.ToLower(strings.TrimSpace(req.Security))
	if security == "" {
		if len(info.Securities) == 1 {
			security = info.Securities[0]
		} else if proto == "vless" && transport == "tcp" {
			security = "reality"
		} else {
			security = info.Securities[0]
		}
	}
	if !slices.Contains(info.Securities, security) {
		return nil, v1.Newf(v1.CodeBadRequest, "协议 %s 不支持安全层 %q（允许：%s）", proto, security, strings.Join(info.Securities, ", "))
	}

	creds := BuildCredentials{UserName: userName}
	settings := map[string]any{}
	tlsObj := map[string]any{}
	trObj := map[string]any{}

	// 1. 协议认证字段与随机密钥生成
	switch proto {
	case "vless":
		u := strings.TrimSpace(req.UUID)
		if u == "" {
			var err error
			u, err = GenerateUUID()
			if err != nil {
				return nil, err
			}
		}
		user := map[string]any{"name": userName, "uuid": u}
		flow := strings.TrimSpace(req.Flow)
		if flow == "" && transport == "tcp" && (security == "reality" || security == "tls") {
			flow = "xtls-rprx-vision"
		}
		if flow != "" {
			if transport != "tcp" {
				return nil, v1.Newf(v1.CodeBadRequest, "VLESS flow=%q 仅支持 tcp 传输层", flow)
			}
			user["flow"] = flow
			creds.Flow = flow
		}
		creds.UUID = u
		settings["users"] = []any{user}

	case "vmess":
		u := strings.TrimSpace(req.UUID)
		if u == "" {
			var err error
			u, err = GenerateUUID()
			if err != nil {
				return nil, err
			}
		}
		creds.UUID = u
		settings["users"] = []any{map[string]any{"name": userName, "uuid": u, "alterId": 0}}

	case "trojan", "hysteria2", "anytls":
		pw := strings.TrimSpace(req.Password)
		if pw == "" {
			var err error
			pw, err = GeneratePassword(16)
			if err != nil {
				return nil, err
			}
		}
		creds.Password = pw
		settings["users"] = []any{map[string]any{"name": userName, "password": pw}}

	case "hysteria":
		pw := strings.TrimSpace(req.Password)
		if pw == "" {
			var err error
			pw, err = GeneratePassword(16)
			if err != nil {
				return nil, err
			}
		}
		creds.Password = pw
		settings["users"] = []any{map[string]any{"name": userName, "auth_str": pw}}

	case "tuic":
		u := strings.TrimSpace(req.UUID)
		if u == "" {
			var err error
			u, err = GenerateUUID()
			if err != nil {
				return nil, err
			}
		}
		pw := strings.TrimSpace(req.Password)
		if pw == "" {
			var err error
			pw, err = GeneratePassword(16)
			if err != nil {
				return nil, err
			}
		}
		creds.UUID = u
		creds.Password = pw
		settings["users"] = []any{map[string]any{"name": userName, "uuid": u, "password": pw}}
		settings["congestion_control"] = "bbr"

	case "shadowsocks":
		cipher := strings.TrimSpace(req.Cipher)
		if cipher == "" {
			cipher = "2022-blake3-aes-128-gcm"
		}
		settings["method"] = cipher
		if strings.HasPrefix(cipher, "2022-") {
			srvPSK := strings.TrimSpace(req.Password)
			if srvPSK == "" {
				var err error
				srvPSK, err = GenerateSS2022PSK(cipher)
				if err != nil {
					return nil, err
				}
			} else if err := ValidateSS2022KeyFormat(cipher, srvPSK); err != nil {
				return nil, err
			}
			userPSK, err := GenerateSS2022PSK(cipher)
			if err != nil {
				return nil, err
			}
			creds.ServerPSK = srvPSK
			creds.UserPSK = userPSK
			creds.Password = srvPSK + ":" + userPSK
			settings["password"] = srvPSK
			settings["users"] = []any{map[string]any{"name": userName, "password": userPSK}}
		} else {
			pw := strings.TrimSpace(req.Password)
			if pw == "" {
				var err error
				pw, err = GeneratePassword(16)
				if err != nil {
					return nil, err
				}
			}
			creds.Password = pw
			settings["password"] = pw
		}

	case "socks", "http", "mixed":
		pw := strings.TrimSpace(req.Password)
		if pw != "" {
			creds.Password = pw
			settings["users"] = []any{map[string]any{"username": userName, "password": pw}}
		}

	case "naive":
		pw := strings.TrimSpace(req.Password)
		if pw == "" {
			var err error
			pw, err = GeneratePassword(16)
			if err != nil {
				return nil, err
			}
		}
		creds.Password = pw
		settings["users"] = []any{map[string]any{"username": userName, "password": pw}}

	case "snell":
		psk := strings.TrimSpace(req.Password)
		if psk == "" {
			var err error
			psk, err = GeneratePassword(16)
			if err != nil {
				return nil, err
			}
		}
		uk, err := GeneratePassword(16)
		if err != nil {
			return nil, err
		}
		creds.ServerPSK = psk
		creds.UserPSK = uk
		settings["version"] = 5
		settings["psk"] = psk
		settings["users"] = []any{map[string]any{"name": userName, "userkey": uk}}

	case "shadowtls":
		pw := strings.TrimSpace(req.Password)
		if pw == "" {
			var err error
			pw, err = GeneratePassword(16)
			if err != nil {
				return nil, err
			}
		}
		hsHost, hsPort := parseHandshake(req.Handshake, req.SNI)
		if hsHost == "" {
			return nil, v1.New(v1.CodeBadRequest, "shadowtls 协议必须指定 --handshake 或 --sni（如 www.microsoft.com:443）")
		}
		creds.Password = pw
		settings["version"] = 3
		settings["handshake"] = map[string]any{"server": hsHost, "server_port": hsPort}
		settings["users"] = []any{map[string]any{"name": userName, "password": pw}}

	case "direct":
		creds.UserName = ""
	}

	// 2. 传输层配置
	switch transport {
	case "ws":
		p := strings.TrimSpace(req.Path)
		if p == "" {
			p = "/ws"
		}
		trObj = map[string]any{"type": "ws", "path": p}
	case "grpc":
		svc := strings.TrimSpace(req.ServiceName)
		if svc == "" {
			svc = "grpc"
		}
		trObj = map[string]any{"type": "grpc", "service_name": svc}
	case "http":
		p := strings.TrimSpace(req.Path)
		if p == "" {
			p = "/"
		}
		trObj = map[string]any{"type": "http", "path": p}
	case "httpupgrade":
		p := strings.TrimSpace(req.Path)
		if p == "" {
			p = "/upgrade"
		}
		trObj = map[string]any{"type": "httpupgrade", "path": p}
	}

	// 3. 安全层配置（TLS / Reality）
	switch security {
	case "tls":
		tlsObj["enabled"] = true
		if sni := strings.TrimSpace(req.SNI); sni != "" {
			tlsObj["server_name"] = sni
		}
		if cp := strings.TrimSpace(req.CertPath); cp != "" {
			tlsObj["certificate_path"] = cp
		}
		if kp := strings.TrimSpace(req.KeyPath); kp != "" {
			tlsObj["key_path"] = kp
		}
	case "reality":
		hsHost, hsPort := parseHandshake(req.Handshake, req.SNI)
		if hsHost == "" {
			return nil, v1.New(v1.CodeBadRequest, "启用 Reality 时必须指定 --sni 或 --handshake（例如 www.microsoft.com）")
		}
		sni := strings.TrimSpace(req.SNI)
		if sni == "" {
			sni = hsHost
		}
		priv, pub, err := GenerateRealityKeypair()
		if err != nil {
			return nil, err
		}
		sid, err := GenerateShortID(4)
		if err != nil {
			return nil, err
		}
		creds.RealityPrivateKey = priv
		creds.RealityPublicKey = pub
		creds.ShortID = sid
		// 注意：sing-box 严格模式禁止 tls.reality.public_key，公钥只在 credentials 中返回。
		tlsObj = map[string]any{
			"enabled":     true,
			"server_name": sni,
			"reality": map[string]any{
				"enabled":     true,
				"handshake":   map[string]any{"server": hsHost, "server_port": hsPort},
				"private_key": priv,
				"short_id":    []string{sid},
			},
		}
	}

	setBytes, _ := json.Marshal(settings)
	tlsBytes, _ := json.Marshal(tlsObj)
	trBytes, _ := json.Marshal(trObj)

	spec := v1.InboundSpec{
		ServerID:  req.ServerID,
		Tag:       tag,
		Protocol:  proto,
		Port:      int64(req.Port),
		Listen:    strings.TrimSpace(req.Listen),
		TLS:       v1.SecretJSON(tlsBytes),
		Transport: trBytes,
		Settings:  v1.SecretJSON(setBytes),
		Enabled:   true,
		SortOrder: int64(req.SortOrder),
	}

	// 4. 立刻过一遍完整渲染校验，确保可无误落入 sing-box。
	if _, err := Render(ServerInput{
		Server:   v1.ServerSpec{CoreLogLevel: "warn"},
		Inbounds: []v1.InboundSpec{spec},
	}); err != nil {
		return nil, err
	}

	planSpec := map[string]any{
		"server_id":  spec.ServerID,
		"tag":        spec.Tag,
		"protocol":   spec.Protocol,
		"port":       spec.Port,
		"listen":     spec.Listen,
		"tls":        json.RawMessage(tlsBytes),
		"transport":  json.RawMessage(trBytes),
		"settings":   json.RawMessage(setBytes),
		"enabled":    spec.Enabled,
		"sort_order": spec.SortOrder,
	}
	return &BuiltInbound{
		Spec:        spec,
		Credentials: creds,
		PlanOp: map[string]any{
			"op":   "create",
			"kind": "Inbound",
			"spec": planSpec,
		},
	}, nil
}

func parseHandshake(handshake, sni string) (host string, port int) {
	raw := strings.TrimSpace(handshake)
	if raw == "" {
		raw = strings.TrimSpace(sni)
	}
	if raw == "" {
		return "", 443
	}
	if idx := strings.LastIndex(raw, ":"); idx > 0 && !strings.Contains(raw[idx+1:], "]") {
		if p, err := strconv.Atoi(raw[idx+1:]); err == nil && p >= 1 && p <= 65535 {
			return strings.Trim(raw[:idx], "[]"), p
		}
	}
	return strings.Trim(raw, "[]"), 443
}
