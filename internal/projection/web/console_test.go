package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const testIndex = `<!doctype html><html><head><title>__SATCHEL_SITE_TITLE__</title>` +
	`<script id="satchel-boot" type="application/json">__SATCHEL_BOOT__</script><script src="/boot.js"></script></head><body></body></html>`

// testDist 是一份假的网页产物：index.html、一个带哈希的脚本与首屏定主题的 boot.js。
func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(testIndex)},
		"assets/index-abc1.js": {Data: []byte("console.log(1)")},
		"boot.js":              {Data: []byte("/* boot */")},
	}
}

func notFoundJSON() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","reason":"没有这个路径","state":{},"next":""}`))
	})
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func wantSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	h := rec.Header()
	if h.Get("Content-Security-Policy") != ContentSecurityPolicy || h.Get("X-Frame-Options") != "DENY" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("%s 应当带四个安全头：%v", what, h)
	}
}

// master-web「前端路由回 index」。
func TestFrontendRoutesServeIndex(t *testing.T) {
	h := NewHandler(testDist(), notFoundJSON())
	for _, p := range []string{"/", "/tokens", "/settings/gates", "/no/such/page", "/index.html"} {
		rec := get(t, h, "GET", p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="satchel-boot"`) || rec.Header().Get("Cache-Control") != "no-cache" ||
			!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s 应当回 index.html：%d %v %s", p, rec.Code, rec.Header(), rec.Body)
		}
		wantSecurityHeaders(t, rec, p)
	}
	if rec := get(t, h, "HEAD", "/tokens"); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD 照常：%d %d", rec.Code, rec.Body.Len())
	}
}

// master-web「静态资源与缺失的资源」。
func TestAssets(t *testing.T) {
	h := NewHandler(testDist(), notFoundJSON())
	rec := get(t, h, "GET", "/assets/index-abc1.js")
	if rec.Code != 200 || rec.Body.String() != "console.log(1)" || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") ||
		rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("带哈希的资源：%d %v", rec.Code, rec.Header())
	}
	wantSecurityHeaders(t, rec, "资源")
	if rec := get(t, h, "GET", "/boot.js"); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("不带哈希的文件不长缓存：%d %v", rec.Code, rec.Header())
	}
	if rec := get(t, h, "GET", "/assets/missing-0000.js"); rec.Code != 404 || strings.Contains(rec.Body.String(), "satchel-boot") {
		t.Fatalf("缺的资源应当 404、不回 index：%d %s", rec.Code, rec.Body)
	}
	if rec := get(t, h, "GET", "/assets/"); strings.Contains(rec.Body.String(), "index-abc1.js") {
		t.Fatalf("目录不列：%d %s", rec.Code, rec.Body)
	}
}

// master-web「别的方法」：交回四字段 not_found。
func TestOtherMethods(t *testing.T) {
	h := NewHandler(testDist(), notFoundJSON())
	for _, m := range []string{"POST", "DELETE", "PUT"} {
		if rec := get(t, h, m, "/tokens"); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Fatalf("%s 应当 404 not_found：%d %s", m, rec.Code, rec.Body)
		}
	}
}

// master-web「开发构建的占位页」与「安全头齐全」：没有 index.html 时任何网页路径都回占位页。
func TestPlaceholderWithoutWeb(t *testing.T) {
	h := NewHandler(fstest.MapFS{".gitkeep": {}}, notFoundJSON())
	for _, p := range []string{"/", "/tokens", "/assets/x.js"} {
		rec := get(t, h, "GET", p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "没有编进网页") {
			t.Fatalf("%s 应当回占位页：%d %s", p, rec.Code, rec.Body)
		}
		wantSecurityHeaders(t, rec, "占位页")
	}
}

// master-web「注入的值不能逃出去」：站点标题按 HTML 转义，启动配置里的 < > & 写成 \u 形式；为空时标题用 Satchel。
func TestBrandingInjection(t *testing.T) {
	h := NewHandler(testDist(), notFoundJSON())
	if body := get(t, h, "GET", "/").Body.String(); !strings.Contains(body, "<title>Satchel</title>") || !strings.Contains(body, `"default_theme":""`) {
		t.Fatalf("没设置时：%s", body)
	}
	h.SetBranding(Branding{SiteTitle: "</title><script>alert(1)</script>", BrandTitle: "</script><script>alert(2)</script>",
		LogoURL: "/public/logo.png", LoginWallpaper: "https://img.example/a.jpg?x=1&y=2", DefaultTheme: "flat"})
	body := get(t, h, "GET", "/").Body.String()
	if strings.Contains(body, "<script>alert(") {
		t.Fatalf("注入的值逃出去了：%s", body)
	}
	for _, want := range []string{"<title>&lt;/title&gt;&lt;script&gt;alert(1)&lt;/script&gt;</title>", `\u003c/script\u003e\u003cscript\u003ealert(2)`,
		`"logo_url":"/public/logo.png"`, `"login_wallpaper":"https://img.example/a.jpg?x=1\u0026y=2"`, `"default_theme":"flat"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("回应里应当有 %s：%s", want, body)
		}
	}
	if strings.Count(body, "</script>") != 2 {
		t.Fatalf("只有页面自己的两个 </script>：%s", body)
	}
}

// master-web「不注入别的设置」：Branding 只有五个字段，回应里没有它们以外的设置。
func TestBrandingOnlyPublicFields(t *testing.T) {
	h := NewHandler(testDist(), notFoundJSON())
	h.SetBranding(Branding{SiteTitle: "甲", BrandTitle: "乙", DefaultTheme: "flat"})
	body := get(t, h, "GET", "/").Body.String()
	start := strings.Index(body, `type="application/json">`) + len(`type="application/json">`)
	boot := body[start : start+strings.Index(body[start:], "</script>")]
	if boot != `{"site_title":"甲","brand_title":"乙","logo_url":"","login_wallpaper":"","default_theme":"flat"}` {
		t.Fatalf("启动配置只有五个公开字段：%s", boot)
	}
}

// master-web：设置写成功后下一个 index.html 用新值，带着上一份的 If-Modified-Since 也不能回 304（同一秒里改也一样）。
func TestBrandingNotCachedByModTime(t *testing.T) {
	h := NewHandler(testDist(), notFoundJSON())
	if lm := get(t, h, "GET", "/").Header().Get("Last-Modified"); lm != "" {
		t.Fatalf("index.html 不该带 Last-Modified：%s", lm)
	}
	h.SetBranding(Branding{SiteTitle: "新标题"})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>新标题</title>") {
		t.Fatalf("应当是带新标题的 200：%d %s", rec.Code, rec.Body.String())
	}
}

// 设置写成功后从别的 goroutine 换值，同时有请求在读：没有数据竞争（配合 -race）。
func TestBrandingConcurrent(t *testing.T) {
	h := NewHandler(testDist(), notFoundJSON())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.SetBranding(Branding{SiteTitle: "x"}) }()
		go func() { defer wg.Done(); _ = get(t, h, "GET", "/") }()
	}
	wg.Wait()
}
