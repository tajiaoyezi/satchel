package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-login-protection「Turnstile 登录验证码」：验证码配置入口不看身份，只给 enabled 与 site_key；登录请求体收 turnstile_token。
func TestCaptchaEndpointAndLoginBody(t *testing.T) {
	fs := &fakeSessions{}
	h := NewHandler(testTable(t), &echo{}, fs)
	rec := raw(t, h, "GET", SessionCaptchaPath, "", nil)
	f := fields(t, rec)
	if rec.Code != 200 || string(f["enabled"]) != "false" || string(f["site_key"]) != `""` || len(f) != 3 {
		t.Fatalf("没启用时是 enabled=false、site_key 为空（外加 apiVersion）：%d %s", rec.Code, rec.Body.String())
	}
	fs.captcha = auth.CaptchaConfig{Enabled: true, SiteKey: "0x4AAAAAAAsitekeyforsatchel"}
	rec = raw(t, h, "GET", SessionCaptchaPath, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer sat_revoked") })
	f = fields(t, rec)
	if rec.Code != 200 || string(f["enabled"]) != "true" || string(f["site_key"]) != `"0x4AAAAAAAsitekeyforsatchel"` || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("启用后给 site key、不含 secret，带无效令牌也照样 200：%d %s", rec.Code, rec.Body.String())
	}
	if rec := raw(t, h, "POST", SessionCaptchaPath, `{}`, nil); rec.Code != 404 {
		t.Fatalf("验证码配置只有 GET：%d", rec.Code)
	}
	if rec := raw(t, h, "POST", SessionPath, `{"username":"admin","password":"pw","turnstile_token":"tok-1"}`, nil); rec.Code != 200 || fs.turnstileTokens[len(fs.turnstileTokens)-1] != "tok-1" {
		t.Fatalf("turnstile_token 应当交给登录：%d %v", rec.Code, fs.turnstileTokens)
	}
	if rec := raw(t, h, "POST", SessionPath, `{"username":"admin","password":"pw","captcha":"x"}`, nil); rec.Code != 400 {
		t.Fatalf("别的未知字段仍是 bad_request：%d", rec.Code)
	}
}

// master-web-session「cookie 与同源检查」：经 HTTPS 到达（TLS 直连，或门判定的登记反代加 X-Forwarded-Proto: https）带 Secure。
func TestSecureCookieFollowsRemote(t *testing.T) {
	h := NewHandler(testTable(t), &echo{}, &fakeSessions{})
	login := `{"username":"admin","password":"pw"}`
	viaProxy := raw(t, h, "POST", SessionPath, login, func(r *http.Request) {
		*r = *r.WithContext(v1.WithRemote(r.Context(), v1.Remote{IP: "198.51.100.7", HTTPS: true}))
	})
	if c := sessionCookie(t, viaProxy); c == nil || !c.Secure {
		t.Fatalf("门判定经 HTTPS 到达时应当 Secure：%+v", c)
	}
	plain := raw(t, h, "POST", SessionPath, login, func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https") // 没经门、没登记：请求头本身不算
		*r = *r.WithContext(v1.WithRemote(r.Context(), v1.Remote{IP: "127.0.0.1", Local: true}))
	})
	if c := sessionCookie(t, plain); c == nil || c.Secure {
		t.Fatalf("明文且不是登记的反代时不带 Secure：%+v", c)
	}
}

// master-access-gates「跨域调用只给令牌用」。
func TestCORS(t *testing.T) {
	var reached int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(200) })
	send := func(h http.Handler, method, origin string, preflight bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/whoami", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if preflight {
			r.Header.Set("Access-Control-Request-Method", "GET")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	noCORS := func(w *httptest.ResponseRecorder) bool {
		for k := range w.Header() {
			if strings.HasPrefix(k, "Access-Control-") {
				return false
			}
		}
		return true
	}
	// 默认不跨域：原样交给路由，没有任何跨域头。
	if w := send(CORS(nil, next), "GET", "https://dash.example.com", false); w.Code != 200 || !noCORS(w) {
		t.Fatalf("不设时不发跨域头：%v", w.Header())
	}
	listed := CORS([]string{"https://dash.example.com"}, next)
	w := send(listed, "GET", "https://dash.example.com", false)
	h := w.Header()
	if w.Code != 200 || h.Get("Access-Control-Allow-Origin") != "https://dash.example.com" || h.Get("Vary") != "Origin" ||
		h.Get("Access-Control-Allow-Methods") != "GET, POST, DELETE, OPTIONS" || h.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" ||
		h.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("列出的来源拿到三项跨域头、没有 Credentials：%v", h)
	}
	before := reached
	w = send(listed, "OPTIONS", "https://dash.example.com", true)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") == "" || reached != before {
		t.Fatalf("允许来源的预检直接 204、不进路由：%d %v", w.Code, w.Header())
	}
	if w := send(listed, "GET", "https://evil.example", false); w.Code != 200 || !noCORS(w) {
		t.Fatalf("不在列表里的来源照常处理、没有跨域头：%v", w.Header())
	}
	if w := send(listed, "OPTIONS", "https://evil.example", true); w.Code != 200 {
		t.Fatalf("不在列表里的预检不拦，交给路由：%d", w.Code)
	}
	any := CORS([]string{"*"}, next)
	if w := send(any, "GET", "https://whatever.example", false); w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Vary") != "" {
		t.Fatalf("* 时回 *：%v", w.Header())
	}
}

// 门在静默模式下用的 not-found，与路由自己的 404 同一段代码。
func TestNotFoundMatchesRouter(t *testing.T) {
	h := NewHandler(testTable(t), &echo{}, &fakeSessions{})
	fromRouter := raw(t, h, "GET", "/api/v1/nosuch", "", nil)
	w := httptest.NewRecorder()
	NotFound(w, httptest.NewRequest("GET", "/api/v1/nosuch", nil))
	if w.Code != fromRouter.Code || w.Body.String() != fromRouter.Body.String() || w.Header().Get("Content-Type") != fromRouter.Header().Get("Content-Type") {
		t.Fatalf("两处的 404 应当一模一样：%d %s / %d %s", w.Code, w.Body.String(), fromRouter.Code, fromRouter.Body.String())
	}
}
