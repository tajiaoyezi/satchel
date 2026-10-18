package web

import (
	"bytes"
	"encoding/json"
	"html"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// 网页处理器（master-web「网页的路径与回应」「首屏注入」「网页回应的安全头」）：/api/、/mcp、/public/ 之外的路径交给它。

// ContentSecurityPolicy 是网页回应的 CSP：脚本只许同源与 Turnstile，logo 与壁纸可以是外链图片，不许被嵌进别人的页面。
// Radix 组件会写内联的 style 属性，所以 style-src 放行 'unsafe-inline'；首屏定主题的脚本是同源文件，不是内联脚本。
const ContentSecurityPolicy = "default-src 'self'; script-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; " +
	"img-src 'self' data: https: http:; style-src 'self' 'unsafe-inline'; connect-src 'self'; font-src 'self' data:; object-src 'none'; " +
	"base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// index.html 里给首屏注入留的两处占位（satchel-web 的 index.html 里写着它们）。
const (
	titlePlaceholder = "__SATCHEL_SITE_TITLE__"
	bootPlaceholder  = "__SATCHEL_BOOT__"
)

// defaultSiteTitle 是站点标题为空时的 <title>。
const defaultSiteTitle = "Satchel"

// Branding 是首屏注入的公开字段：登录页本来就显示它们。别的设置不进网页回应。
type Branding struct {
	SiteTitle      string `json:"site_title"`
	BrandTitle     string `json:"brand_title"`
	LogoURL        string `json:"logo_url"`
	LoginWallpaper string `json:"login_wallpaper"`
	DefaultTheme   string `json:"default_theme"`
}

// Handler 给出编进二进制的网页。index 为 nil 时是开发构建（没跑过 tools/web/build.sh），一律回占位页。
type Handler struct {
	files    fs.FS
	index    []byte
	notFound http.Handler

	mu     sync.RWMutex
	served []byte
}

// NewHandler 用网页产物 files（dist 里面那一层）构造处理器；别的方法（不是 GET、HEAD）交给 notFound。
func NewHandler(files fs.FS, notFound http.Handler) *Handler {
	h := &Handler{files: files, notFound: notFound}
	if raw, err := fs.ReadFile(files, "index.html"); err == nil {
		h.index = raw
	}
	h.SetBranding(Branding{})
	return h
}

// SetBranding 换上首屏注入的值（启动时与每次设置写成功后调），下一个 index.html 回应就用新值。
// 站点标题按 HTML 转义；启动配置是 JSON（encoding/json 把 <、>、& 写成 \u003c 等，值逃不出数据块）。
func (h *Handler) SetBranding(b Branding) {
	if strings.TrimSpace(b.SiteTitle) == "" {
		b.SiteTitle = defaultSiteTitle
	}
	if h.index == nil {
		return
	}
	boot, err := json.Marshal(b)
	if err != nil {
		boot = []byte("{}")
	}
	served := bytes.ReplaceAll(h.index, []byte(titlePlaceholder), []byte(html.EscapeString(b.SiteTitle)))
	served = bytes.ReplaceAll(served, []byte(bootPlaceholder), boot)
	h.mu.Lock()
	h.served = served
	h.mu.Unlock()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.notFound.ServeHTTP(w, r)
		return
	}
	setSecurityHeaders(w.Header())
	if h.index == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(placeholderPage))
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "index.html" {
		h.serveIndex(w, r)
		return
	}
	if info, err := fs.Stat(h.files, name); err == nil && info.Mode().IsRegular() {
		raw, err := fs.ReadFile(h.files, name)
		if err != nil {
			http.Error(w, "读不了网页文件", http.StatusInternalServerError)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // 文件名带内容哈希
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(raw))
		return
	}
	if strings.HasPrefix(name, "assets/") {
		http.NotFound(w, r) // 缺的资源不能回 index.html，免得浏览器把 HTML 当脚本或样式
		return
	}
	h.serveIndex(w, r) // 网页的前端路由
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	served := h.served
	h.mu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// 不给修改时间（也就没有 Last-Modified）：内容随设置写换，按秒的时间会让同一秒里换上的新内容被 304 挡住。
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(served))
}

func setSecurityHeaders(hdr http.Header) {
	hdr.Set("Content-Security-Policy", ContentSecurityPolicy)
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "same-origin")
}

// placeholderPage 是开发构建的占位页：这个二进制没有编进网页。
const placeholderPage = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Satchel</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem; line-height: 1.6">
<h1>Satchel</h1>
<p>这个 satchel 二进制没有编进网页（开发构建）。主控与 CLI、REST、MCP 照常可用。</p>
<p>要用网页：下载发布版，或在 satchel 仓库里先运行 <code>tools/web/build.sh</code> 再构建。</p>
</body>
</html>
`
