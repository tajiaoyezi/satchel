package nginx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"sort"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	// StealListenPort 是偷自己模式下本地 nginx 伪装站固定监听的回环 TLS 端口（无 proxy_protocol）。
	StealListenPort = 8001
	// ManagedStealFile 是服务器偷自己 nginx 配置文件的固定文件名，避免域名变更产生旧文件残留。
	ManagedStealFile = "satchel-steal.conf"
)

// ManagedStealConfPath 返回服务器偷自己本地 nginx 配置文件的固定绝对路径。
func ManagedStealConfPath() string {
	return path.Join(DefaultServersDir, ManagedStealFile)
}

// WSSLocation 描述同机需要合并进偷自己 nginx 443/8001 域名的 WebSocket 入站转发块。
type WSSLocation struct {
	Tag    string `json:"tag,omitempty"`
	Path   string `json:"path"`
	Listen string `json:"listen,omitempty"`
	Port   int64  `json:"port"`
}

// StealRenderInput 是 RenderStealSite 的确定性渲染输入。
type StealRenderInput struct {
	ServerID     int64
	Domain       string
	StealMode    string // "tunnel" | "fallback"
	SiteType     string // "" (默认 decoy) | "static" | "proxy" | "decoy"
	SiteValue    string
	Ipv6Enabled  bool
	SameHost     bool
	CertPath     string
	KeyPath      string
	WSSLocations []WSSLocation
}

// ExtractWSSLocations 从同台服务器的入站规格列表中提取所有启用中且使用 WebSocket 传输层的入站。
// 结果按 Path ASC、Port ASC、Tag ASC 确定性排序，同一 Path 只保留首条，避免 nginx location 重复。
func ExtractWSSLocations(inbounds []v1.InboundSpec) []WSSLocation {
	var raw []WSSLocation
	for _, ib := range inbounds {
		if !ib.Enabled || ib.Port < 1 || ib.Port > 65535 {
			continue
		}
		trimmed := bytes.TrimSpace(ib.Transport)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			continue
		}
		var tr map[string]any
		if err := json.Unmarshal(trimmed, &tr); err != nil {
			continue
		}
		trType, _ := tr["type"].(string)
		if !strings.EqualFold(strings.TrimSpace(trType), "ws") {
			continue
		}
		wsPath, _ := tr["path"].(string)
		wsPath = strings.TrimSpace(wsPath)
		if wsPath == "" {
			wsPath = "/ws"
		}
		listen := strings.TrimSpace(ib.Listen)
		if listen == "" || listen == "0.0.0.0" || listen == "::" {
			listen = "127.0.0.1"
		}
		raw = append(raw, WSSLocation{
			Tag:    ib.Tag,
			Path:   wsPath,
			Listen: listen,
			Port:   ib.Port,
		})
	}
	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].Path != raw[j].Path {
			return raw[i].Path < raw[j].Path
		}
		if raw[i].Port != raw[j].Port {
			return raw[i].Port < raw[j].Port
		}
		return raw[i].Tag < raw[j].Tag
	})
	seen := make(map[string]bool, len(raw))
	out := make([]WSSLocation, 0, len(raw))
	for _, loc := range raw {
		if seen[loc.Path] {
			continue
		}
		seen[loc.Path] = true
		out = append(out, loc)
	}
	return out
}

// ValidateStealInput 校验偷自己本地 nginx 渲染输入（含同机 443 防抢占、域名与伪装站防注入校验）。
func ValidateStealInput(in StealRenderInput) error {
	if in.SameHost {
		return v1.New(v1.CodeConflict, "主控同机节点禁止启用 443 偷取模式与本地偷自己 nginx（防止抢占主控 HTTPS 443 端口）")
	}
	domain, err := ValidateDomain(in.Domain)
	if err != nil {
		return err
	}
	mode := strings.ToLower(strings.TrimSpace(in.StealMode))
	if mode == "" {
		mode = "tunnel"
	}
	if mode != "tunnel" && mode != "fallback" {
		return v1.Newf(v1.CodeBadRequest, "steal_mode %q 不合法（仅支持 tunnel 或 fallback）", in.StealMode)
	}
	siteType := strings.ToLower(strings.TrimSpace(in.SiteType))
	if siteType == "" {
		siteType = TypeDecoy
	}
	if err := ValidateWebsiteSpec(v1.WebsiteSpec{
		ServerID: in.ServerID,
		Domain:   domain,
		Type:     siteType,
		Target:   strings.TrimSpace(in.SiteValue),
		Use443:   false,
	}, false); err != nil {
		return err
	}
	for _, cp := range []string{in.CertPath, in.KeyPath} {
		s := strings.TrimSpace(cp)
		if s == "" {
			continue
		}
		if hasForbiddenNginxChars(s) || strings.Contains(s, "..") || !strings.HasPrefix(s, "/") {
			return v1.Newf(v1.CodeBadRequest, "证书路径 %q 不合法", cp)
		}
	}
	for _, loc := range in.WSSLocations {
		p := strings.TrimSpace(loc.Path)
		if !strings.HasPrefix(p, "/") || hasForbiddenNginxChars(p) {
			return v1.Newf(v1.CodeBadRequest, "WS 入站路径 %q 不合法", loc.Path)
		}
		if loc.Port < 1 || loc.Port > 65535 {
			return v1.Newf(v1.CodeBadRequest, "WS 入站端口 %d 不合法", loc.Port)
		}
		listen := strings.TrimSpace(loc.Listen)
		if listen != "" && hasForbiddenNginxChars(listen) {
			return v1.Newf(v1.CodeBadRequest, "WS 入站监听地址 %q 不合法", loc.Listen)
		}
	}
	return nil
}

// RenderStealSite 将服务器偷自己伪装站与同机活跃 WS 入站确定性渲染为固定路径 /etc/nginx/servers/satchel-steal.conf。
// 输出严格遵守 sing-box@v1.14.4 约束：监听 127.0.0.1:8001 ssl 且绝不写 proxy_protocol、绝不直接监听 443。
func RenderStealSite(in StealRenderInput) (*RenderedSite, error) {
	if err := ValidateStealInput(in); err != nil {
		return nil, err
	}
	domain, _ := ValidateDomain(in.Domain)
	mode := strings.ToLower(strings.TrimSpace(in.StealMode))
	if mode == "" {
		mode = "tunnel"
	}
	siteType := strings.ToLower(strings.TrimSpace(in.SiteType))
	if siteType == "" {
		siteType = TypeDecoy
	}
	siteValue := strings.TrimSpace(in.SiteValue)

	certPath := strings.TrimSpace(in.CertPath)
	if certPath == "" {
		certPath = fmt.Sprintf("%s/%s.crt", DefaultCertDir, domain)
	}
	keyPath := strings.TrimSpace(in.KeyPath)
	if keyPath == "" {
		keyPath = fmt.Sprintf("%s/%s.key", DefaultCertDir, domain)
	}

	// 对 WSS locations 排序去重，确保输出完全确定性。
	locs := append([]WSSLocation(nil), in.WSSLocations...)
	sort.SliceStable(locs, func(i, j int) bool {
		if locs[i].Path != locs[j].Path {
			return locs[i].Path < locs[j].Path
		}
		if locs[i].Port != locs[j].Port {
			return locs[i].Port < locs[j].Port
		}
		return locs[i].Tag < locs[j].Tag
	})
	var dedupLocs []WSSLocation
	seenPath := map[string]bool{}
	hasRootWS := false
	for _, loc := range locs {
		p := strings.TrimSpace(loc.Path)
		if seenPath[p] {
			continue
		}
		seenPath[p] = true
		loc.Path = p
		if strings.TrimSpace(loc.Listen) == "" || loc.Listen == "0.0.0.0" || loc.Listen == "::" {
			loc.Listen = "127.0.0.1"
		}
		if p == "/" {
			hasRootWS = true
		}
		dedupLocs = append(dedupLocs, loc)
	}

	out := &RenderedSite{
		Domain:   domain,
		ConfPath: ManagedStealConfPath(),
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s steal_self=true server_id=%d domain=%s mode=%s\n", xrpc.SatchelManagedHeader, in.ServerID, domain, mode)

	// 1. 回环 8001 兜底拒绝非匹配 SNI 的握手。
	b.WriteString("server {\n")
	fmt.Fprintf(&b, "    listen 127.0.0.1:%d ssl default_server;\n", StealListenPort)
	b.WriteString("    ssl_reject_handshake on;\n")
	b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n")
	b.WriteString("}\n\n")

	// 2. 伪装站与 WSS 共存主 server 块（严禁写 proxy_protocol）。
	b.WriteString("server {\n")
	fmt.Fprintf(&b, "    listen 127.0.0.1:%d ssl;\n", StealListenPort)
	b.WriteString("    http2 on;\n")
	fmt.Fprintf(&b, "    server_name %s;\n\n", domain)
	fmt.Fprintf(&b, "    ssl_certificate %s;\n", certPath)
	fmt.Fprintf(&b, "    ssl_certificate_key %s;\n", keyPath)
	b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n\n")

	// 写入同机活跃 WS 入站 location 块。
	for _, loc := range dedupLocs {
		host := loc.Listen
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		fmt.Fprintf(&b, "    location %s {\n", loc.Path)
		fmt.Fprintf(&b, "        proxy_pass http://%s:%d;\n", host, loc.Port)
		b.WriteString("        proxy_http_version 1.1;\n")
		b.WriteString("        proxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("        proxy_set_header Connection \"upgrade\";\n")
		b.WriteString("        proxy_set_header Host $host;\n")
		b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
		b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("        proxy_read_timeout 86400s;\n")
		b.WriteString("        proxy_send_timeout 86400s;\n")
		b.WriteString("        proxy_buffering off;\n")
		b.WriteString("    }\n\n")
	}

	// 写入伪装站 location /（若无根路径 WS 占用）。
	if !hasRootWS {
		switch siteType {
		case TypeStatic:
			rootDir := siteValue
			if rootDir == "" {
				rootDir = path.Join(DefaultHTMLDir, domain)
			} else {
				rootDir = path.Clean(rootDir)
			}
			fmt.Fprintf(&b, "    root %s;\n", rootDir)
			b.WriteString("    index index.html index.htm;\n")
			b.WriteString("    location / {\n")
			b.WriteString("        try_files $uri $uri/ =404;\n")
			b.WriteString("    }\n")
			if strings.HasPrefix(rootDir, DefaultHTMLDir+"/") {
				out.HTMLPath = path.Join(rootDir, "index.html")
				out.HTMLContent = fmt.Sprintf("<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>%s</title></head><body><h1>%s</h1></body></html>\n", domain, domain)
			}
		case TypeProxy:
			u, isWS, _ := parseProxyTarget(siteValue)
			if isWS {
				upstreamScheme := "http"
				if strings.EqualFold(u.Scheme, "wss") {
					upstreamScheme = "https"
				}
				b.WriteString("    location / {\n")
				fmt.Fprintf(&b, "        proxy_pass %s://%s;\n", upstreamScheme, u.Host)
				b.WriteString("        proxy_http_version 1.1;\n")
				b.WriteString("        proxy_set_header Upgrade $http_upgrade;\n")
				b.WriteString("        proxy_set_header Connection \"upgrade\";\n")
				b.WriteString("        proxy_set_header Host $host;\n")
				b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
				b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
				b.WriteString("        proxy_read_timeout 86400s;\n")
				b.WriteString("        proxy_send_timeout 86400s;\n")
				b.WriteString("        proxy_buffering off;\n")
				b.WriteString("    }\n")
			} else {
				b.WriteString("    location / {\n")
				fmt.Fprintf(&b, "        proxy_pass %s;\n", u.String())
				b.WriteString("        proxy_http_version 1.1;\n")
				b.WriteString("        proxy_set_header Host $host;\n")
				b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
				b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
				b.WriteString("        proxy_set_header X-Forwarded-Proto $scheme;\n")
				b.WriteString("    }\n")
			}
		case TypeDecoy:
			presetKey := strings.ToLower(siteValue)
			if presetKey == "" {
				presetKey = "landing"
			}
			if p, ok := decoyPresets[presetKey]; ok {
				rootDir := path.Join(DefaultHTMLDir, domain)
				fmt.Fprintf(&b, "    root %s;\n", rootDir)
				b.WriteString("    index index.html;\n")
				b.WriteString("    location / {\n")
				b.WriteString("        try_files $uri $uri/ =404;\n")
				b.WriteString("    }\n")
				out.HTMLPath = path.Join(rootDir, "index.html")
				out.HTMLContent = fmt.Sprintf("<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>%s</title></head><body>%s</body></html>\n", p.Title, p.Body)
			} else {
				u, _ := url.Parse(siteValue)
				b.WriteString("    location / {\n")
				fmt.Fprintf(&b, "        proxy_pass %s;\n", u.String())
				b.WriteString("        proxy_ssl_server_name on;\n")
				fmt.Fprintf(&b, "        proxy_set_header Host %s;\n", u.Host)
				b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
				b.WriteString("    }\n")
			}
		}
	}
	b.WriteString("}\n\n")

	// 3. HTTP 80 自动跳转 HTTPS（不设 default_server，避免与系统默认站点冲突）。
	b.WriteString("server {\n")
	b.WriteString("    listen 80;\n")
	if in.Ipv6Enabled {
		b.WriteString("    listen [::]:80;\n")
	}
	fmt.Fprintf(&b, "    server_name %s;\n", domain)
	b.WriteString("    return 301 https://$host$request_uri;\n")
	b.WriteString("}\n")

	out.Conf = b.String()
	return out, nil
}

// IsSameHost443Protected 判断目标服务器是否受主控同机 443 防抢占守卫保护：
// 1. sameHostAsMaster == true 直接受保护；
// 2. 服务器尚为 pending（或新建未落库，status 为空或 "pending"）且其非回环域名与 https:// master_url 非回环域名一致时受保护。
func IsSameHost443Protected(sameHostAsMaster bool, serverStatus, serverDomain, masterURL string) bool {
	if sameHostAsMaster {
		return true
	}
	st := strings.ToLower(strings.TrimSpace(serverStatus))
	if st != "" && st != "pending" {
		return false
	}
	srvHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(serverDomain), "."))
	if srvHost == "" || isLoopbackHost(srvHost) {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(masterURL))
	if err != nil || u == nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	masterHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if masterHost == "" || isLoopbackHost(masterHost) {
		return false
	}
	return srvHost == masterHost
}

func isLoopbackHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}
