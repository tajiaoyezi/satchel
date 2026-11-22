// Package nginx 为主控提供纯 Go 的 Website 校验、模板目录与确定性 nginx 站点配置渲染（node-nginx-websites）。
// 所有输出配置首行均携带 # satchel-managed: 标记，且严格拦截配置注入字符、路径穿越与同机 443 抢占。
package nginx

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	TypeStatic = "static"
	TypeProxy  = "proxy"
	TypeDecoy  = "decoy"

	ManagedFilePrefix = "satchel-site-"
	DefaultServersDir = "/etc/nginx/servers"
	DefaultHTMLDir    = "/etc/nginx/html"
	DefaultCertDir    = "/etc/nginx/cert"
)

// TemplateInfo 描述 website templates 返回的单个站点或伪装模板说明。
type TemplateInfo struct {
	ID            string `json:"id"`
	Type          string `json:"type"` // static | proxy | decoy
	DisplayName   string `json:"display_name"`
	Description   string `json:"description"`
	TargetFormat  string `json:"target_format"`
	ExampleTarget string `json:"example_target"`
}

// RenderedSite 是单条 Website 经确定性渲染后用于预览与下发节点的结果。
type RenderedSite struct {
	Domain      string `json:"domain"`
	ConfPath    string `json:"conf_path"`
	Conf        string `json:"conf"`
	HTMLPath    string `json:"html_path,omitempty"`
	HTMLContent string `json:"html_content,omitempty"`
}

var decoyPresets = map[string]struct {
	Title string
	Body  string
}{
	"landing": {
		Title: "Cloud Infrastructure Status",
		Body:  "<h1>Service Portal</h1><p>All regional edge services are operating normally.</p>",
	},
	"blog": {
		Title: "Engineering Notes",
		Body:  "<h1>Engineering Notes</h1><article><h2>Notes on Distributed Caching</h2><p>Static archive.</p></article>",
	},
	"docs": {
		Title: "API Reference Documentation",
		Body:  "<h1>Documentation</h1><p>Browse static technical reference pages.</p>",
	},
	"empty": {
		Title: "Welcome",
		Body:  "<p>OK</p>",
	},
}

// Templates 返回内置支持的站点类型、WSS 反代与伪装站模板目录。
func Templates() []TemplateInfo {
	return []TemplateInfo{
		{
			ID:            "static",
			Type:          TypeStatic,
			DisplayName:   "静态站点",
			Description:   "托管本地静态目录（默认 /etc/nginx/html/<domain>，仅允许 /etc/nginx/html/ 或 /var/www/ 下的安全目录）",
			TargetFormat:  "空（自动默认目录）或绝对目录路径",
			ExampleTarget: "/etc/nginx/html/site.example.com",
		},
		{
			ID:            "proxy",
			Type:          TypeProxy,
			DisplayName:   "HTTP/HTTPS 反向代理",
			Description:   "将域名流量反向代理至后端 HTTP/HTTPS 上游并透传 Host 与 X-Forwarded-* 头",
			TargetFormat:  "http://host:port 或 https://host:port",
			ExampleTarget: "http://127.0.0.1:8080",
		},
		{
			ID:            "wss-proxy",
			Type:          TypeProxy,
			DisplayName:   "WSS 域名反代模板",
			Description:   "以 ws:// 或 wss:// 作为目标 URL（含路径），自动渲染 WebSocket Upgrade/Connection 与长连接超时配置",
			TargetFormat:  "ws://host:port/path 或 wss://host:port/path",
			ExampleTarget: "ws://127.0.0.1:10000/ws",
		},
		{
			ID:            "decoy-landing",
			Type:          TypeDecoy,
			DisplayName:   "伪装站 · 门户首页",
			Description:   "生成纯静态门户占位页面并写入 /etc/nginx/html/<domain>/index.html",
			TargetFormat:  "landing",
			ExampleTarget: "landing",
		},
		{
			ID:            "decoy-blog",
			Type:          TypeDecoy,
			DisplayName:   "伪装站 · 技术博客",
			Description:   "生成纯静态技术博客占位页面",
			TargetFormat:  "blog",
			ExampleTarget: "blog",
		},
		{
			ID:            "decoy-docs",
			Type:          TypeDecoy,
			DisplayName:   "伪装站 · 文档站点",
			Description:   "生成纯静态文档占位页面",
			TargetFormat:  "docs",
			ExampleTarget: "docs",
		},
		{
			ID:            "decoy-empty",
			Type:          TypeDecoy,
			DisplayName:   "伪装站 · 极简页面",
			Description:   "生成极简静态占位页面（也可传入 http(s):// 上游做镜像伪装）",
			TargetFormat:  "empty 或 https://mirror.example.com",
			ExampleTarget: "empty",
		},
	}
}

var domainLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateDomain 校验并规范化域名（小写、无尾点、RFC 标签、禁止路径穿越与注入字符）。
func ValidateDomain(raw string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(raw))
	d = strings.TrimSuffix(d, ".")
	if d == "" {
		return "", v1.New(v1.CodeBadRequest, "domain 不能为空")
	}
	if len(d) > 253 {
		return "", v1.Newf(v1.CodeBadRequest, "domain %q 长度超过 253 字符", d)
	}
	if hasForbiddenNginxChars(d) || strings.ContainsAny(d, "/\\:*?[]") {
		return "", v1.Newf(v1.CodeBadRequest, "domain %q 包含非法字符", raw)
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", v1.Newf(v1.CodeBadRequest, "domain %q 必须是包含至少两段的合法域名（如 app.example.com）", raw)
	}
	for _, lb := range labels {
		if !domainLabelRe.MatchString(lb) {
			return "", v1.Newf(v1.CodeBadRequest, "domain %q 的标签 %q 不合法", raw, lb)
		}
	}
	return d, nil
}

func hasForbiddenNginxChars(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < 0x20 || b == 0x7f {
			return true
		}
		switch b {
		case ';', '{', '}', '"', '\'', ' ', '\t', '`', '$':
			return true
		}
	}
	return false
}

// ManagedConfPath 返回某域名对应的 Satchel 纳管 nginx 配置文件路径。
func ManagedConfPath(domain string) string {
	return path.Join(DefaultServersDir, ManagedFilePrefix+strings.ToLower(strings.TrimSpace(domain))+".conf")
}

// NormalizeBuildTarget 供 website build 将 --target 与可选 --wss-path 归一化为 WebsiteSpec.Target。
func NormalizeBuildTarget(typ, target, wssPath string) (string, error) {
	typ = strings.ToLower(strings.TrimSpace(typ))
	target = strings.TrimSpace(target)
	wssPath = strings.TrimSpace(wssPath)

	if wssPath != "" {
		if typ != TypeProxy {
			return "", v1.New(v1.CodeBadRequest, "--wss-path 只能与 --type proxy 一起使用")
		}
		if hasForbiddenNginxChars(wssPath) || !strings.HasPrefix(wssPath, "/") {
			return "", v1.Newf(v1.CodeBadRequest, "--wss-path %q 必须是以 / 开头且不含非法字符的路径", wssPath)
		}
		if target == "" {
			return "", v1.New(v1.CodeBadRequest, "使用 --wss-path 时必须同时提供上游 --target（如 http://127.0.0.1:10000）")
		}
		u, err := url.Parse(target)
		if err != nil || u.Host == "" {
			return "", v1.Newf(v1.CodeBadRequest, "--target %q 不是合法的上游地址", target)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "ws":
			u.Scheme = "ws"
		case "https", "wss":
			u.Scheme = "wss"
		default:
			return "", v1.Newf(v1.CodeBadRequest, "WSS 反代上游协议仅支持 http/https/ws/wss，得到 %q", u.Scheme)
		}
		u.Path = wssPath
		target = u.String()
	}
	return target, nil
}

// ValidateWebsiteSpec 校验单条 WebsiteSpec（含域名、类型、目标防注入与同机 443 防抢占）。
func ValidateWebsiteSpec(spec v1.WebsiteSpec, sameHost bool) error {
	if _, err := ValidateDomain(spec.Domain); err != nil {
		return err
	}
	typ := strings.ToLower(strings.TrimSpace(spec.Type))
	switch typ {
	case TypeStatic, TypeProxy, TypeDecoy:
	default:
		return v1.Newf(v1.CodeBadRequest, "Website.type %q 不合法，仅支持 static、proxy 或 decoy", spec.Type)
	}
	if sameHost && spec.Use443 {
		return v1.New(v1.CodeConflict, "同机节点禁止直接启用 use_443 抢占主控 443 端口（请关闭 use_443 或走主控 SNI 分流）")
	}
	target := strings.TrimSpace(spec.Target)
	if hasForbiddenNginxChars(target) {
		return v1.New(v1.CodeBadRequest, "Website.target 包含非法或注入风险字符（禁止空格、分号、花括号、引号、$ 与控制字符）")
	}

	switch typ {
	case TypeStatic:
		if target != "" {
			if err := validateStaticRoot(target); err != nil {
				return err
			}
		}
	case TypeProxy:
		if target == "" {
			return v1.New(v1.CodeBadRequest, "proxy 类型站点必须提供 target 上游地址（http/https/ws/wss）")
		}
		if _, _, err := parseProxyTarget(target); err != nil {
			return err
		}
	case TypeDecoy:
		if target == "" {
			return nil
		}
		if _, ok := decoyPresets[strings.ToLower(target)]; ok {
			return nil
		}
		u, err := url.Parse(target)
		scheme := ""
		if u != nil {
			scheme = strings.ToLower(u.Scheme)
		}
		if err != nil || (scheme != "http" && scheme != "https") || u.Host == "" {
			return v1.Newf(v1.CodeBadRequest, "decoy 类型 target %q 必须是内置模板名（landing/blog/docs/empty）或合法 http(s):// 上游地址", target)
		}
	}
	return nil
}

func validateStaticRoot(raw string) error {
	if strings.Contains(raw, "..") || strings.Contains(raw, "\\") {
		return v1.Newf(v1.CodeBadRequest, "静态目录 %q 不允许包含 .. 或反斜杠", raw)
	}
	cleaned := path.Clean(raw)
	if !strings.HasPrefix(cleaned, "/etc/nginx/html/") && cleaned != "/etc/nginx/html" &&
		!strings.HasPrefix(cleaned, "/var/www/") && cleaned != "/var/www" {
		return v1.Newf(v1.CodeBadRequest, "静态目录 %q 必须位于 /etc/nginx/html/ 或 /var/www/ 之下", raw)
	}
	return nil
}

func parseProxyTarget(raw string) (u *url.URL, isWS bool, err error) {
	u, err = url.Parse(raw)
	if err != nil || u == nil || strings.TrimSpace(u.Host) == "" {
		return nil, false, v1.Newf(v1.CodeBadRequest, "proxy 上游 %q 不是合法 URL", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u, false, nil
	case "ws", "wss":
		return u, true, nil
	default:
		return nil, false, v1.Newf(v1.CodeBadRequest, "proxy 上游协议仅支持 http/https/ws/wss，得到 %q", u.Scheme)
	}
}

// RenderWebsite 将合法 WebsiteSpec 确定性渲染为带 # satchel-managed: 头的 nginx server 块配置。
func RenderWebsite(websiteID int64, spec v1.WebsiteSpec, sameHost bool) (*RenderedSite, error) {
	if err := ValidateWebsiteSpec(spec, sameHost); err != nil {
		return nil, err
	}
	domain, _ := ValidateDomain(spec.Domain)
	typ := strings.ToLower(strings.TrimSpace(spec.Type))
	target := strings.TrimSpace(spec.Target)

	var b strings.Builder
	fmt.Fprintf(&b, "%s website_id=%d domain=%s type=%s\n", xrpc.SatchelManagedHeader, websiteID, domain, typ)
	b.WriteString("server {\n")
	b.WriteString("    listen 80;\n")
	b.WriteString("    listen [::]:80;\n")
	if spec.Use443 {
		b.WriteString("    listen 443 ssl;\n")
		b.WriteString("    listen [::]:443 ssl;\n")
		b.WriteString("    http2 on;\n")
		fmt.Fprintf(&b, "    ssl_certificate %s/%s.crt;\n", DefaultCertDir, domain)
		fmt.Fprintf(&b, "    ssl_certificate_key %s/%s.key;\n", DefaultCertDir, domain)
		b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n")
	}
	fmt.Fprintf(&b, "    server_name %s;\n\n", domain)

	out := &RenderedSite{
		Domain:   domain,
		ConfPath: ManagedConfPath(domain),
	}

	switch typ {
	case TypeStatic:
		rootDir := target
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
		u, isWS, _ := parseProxyTarget(target)
		if isWS {
			locPath := u.Path
			if locPath == "" {
				locPath = "/"
			}
			upstreamScheme := "http"
			if strings.EqualFold(u.Scheme, "wss") {
				upstreamScheme = "https"
			}
			upstream := fmt.Sprintf("%s://%s", upstreamScheme, u.Host)
			fmt.Fprintf(&b, "    location %s {\n", locPath)
			fmt.Fprintf(&b, "        proxy_pass %s;\n", upstream)
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
			if locPath != "/" {
				b.WriteString("    location / {\n")
				b.WriteString("        return 404;\n")
				b.WriteString("    }\n")
			}
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
		presetKey := strings.ToLower(target)
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
			u, _ := url.Parse(target)
			b.WriteString("    location / {\n")
			fmt.Fprintf(&b, "        proxy_pass %s;\n", u.String())
			b.WriteString("        proxy_ssl_server_name on;\n")
			fmt.Fprintf(&b, "        proxy_set_header Host %s;\n", u.Host)
			b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
			b.WriteString("    }\n")
		}
	}

	b.WriteString("}\n")
	out.Conf = b.String()
	return out, nil
}

// HasPort443Listen 检测一段 nginx 配置文本是否声明了直接监听 443 端口。
var listen443Re = regexp.MustCompile(`(?m)^\s*listen\s+(?:\[::\]:|\*:|[0-9.]+:)?443(?:\s|;)`)

func HasPort443Listen(conf string) bool {
	return listen443Re.MatchString(conf)
}
